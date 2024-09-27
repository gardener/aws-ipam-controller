//Copyright 2016 The Kubernetes Authors.
//
//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.
//
//
// This file was copied from the kubernetes/cloud-provider-gcp project
// https://github.com/kubernetes/cloud-provider-gcp/blob/master/pkg/controller/nodeipam/ipam/cidr_allocator.go
//
// Modifications Copyright 2024 SAP SE or an SAP affiliate company and Gardener contributors

package ipam

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/gardener/aws-ipam-controller/pkg/ipam/cidrset"

	nodeutil "github.com/gardener/aws-ipam-controller/pkg/node"
	coreV1Client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	netutils "k8s.io/utils/net"
	runtimecache "sigs.k8s.io/controller-runtime/pkg/cache"
)

type Mode string

const (
	// IPv4 defines the ipv4 ipam controller mode
	IPv4 Mode = "ipv4"
	// IPv6 defines the ipv6 ipam controller mode
	IPv6 Mode = "ipv6"
	// DualStack defines the dual-stack ipam controller mode
	DualStack Mode = "dual-stack"
)

// cidrs are reserved, then node resource is patched with them
// this type holds the reservation info for a node

// NodeReservedCIDRs holds the allocated CIDRs
type NodeReservedCIDRs struct {
	allocatedCIDRs []*net.IPNet
	nodeName       string
}

// TODO: figure out the good setting for those constants.
const (
	// The no. of NodeSpec updates ipam controller can process concurrently.
	cidrUpdateWorkers = 30

	// The max no. of NodeSpec updates that can be enqueued.
	CidrUpdateQueueSize = 5000

	// cidrUpdateRetries is the no. of times a NodeSpec update will be retried before dropping it.
	cidrUpdateRetries = 3
)

// CIDRAllocator is an interface implemented by things that know how
// to allocate/occupy/recycle CIDR for nodes.
type CIDRAllocator interface {
	// AllocateOrOccupyCIDR looks at the given node, assigns it a valid
	// CIDR if it doesn't currently have one or mark the CIDR as used if
	// the node already have one.
	AllocateOrOccupyCIDR(node *v1.Node) error
	// ReleaseCIDR releases the CIDR of the removed node
	ReleaseCIDR(node *v1.Node) error
	// Run starts all the working logic of the allocator.
	Run(ctx context.Context, stopCh <-chan struct{})

	ReadyChecker(_ *http.Request) error

	HealthzChecker(_ *http.Request) error
}

// CIDRAllocatorParams is parameters that's required for creating new
// cidr range allocator.
type CIDRAllocatorParams struct {
	// ClusterCIDRs is list of cluster cidrs
	ClusterCIDRs []*net.IPNet
	// NodeCIDRMaskSizes is list of node cidr mask sizes
	NodeCIDRMaskSizes []int
}

type cidrAllocator struct {
	coreV1Client *coreV1Client.CoreV1Client
	// cluster cidrs as passed in during controller creation
	clusterCIDRs []*net.IPNet
	// for each entry in clusterCIDRs we maintain a list of what is used and what is not
	cidrSets  []*cidrset.CidrSet
	ec2Client *ec2.Client
	// nodesSynced returns true if the node shared informer has been synced at least once.
	nodesSynced cache.InformerSynced
	// Channel that is used to pass updating Nodes and their reserved CIDRs to the background
	// This increases a throughput of CIDR assignment by not blocking on long operations.
	nodeCIDRUpdateChannel chan NodeReservedCIDRs
	recorder              record.EventRecorder
	// Keep a set of nodes that are currently being processed to avoid races in CIDR allocation
	lock                 sync.Mutex
	nodesInProcessing    sets.Set[string]
	mode                 Mode
	tickPeriod           time.Duration
	nodeCIDRMaskSizeIPv6 int
}

// NewCIDRRangeAllocator returns a CIDRAllocator to allocate CIDRs for node (one from each of clusterCIDRs)
// Caller must ensure subNetMaskSize is not less than cluster CIDR mask size.
// Caller must always pass in a list of existing nodes so the new allocator.
// can initialize its CIDR map. NodeList is only nil in testing.
func NewCIDRRangeAllocator(ctx context.Context, client *coreV1Client.CoreV1Client, ec2Client *ec2.Client, allocatorParams CIDRAllocatorParams, nodeInformer runtimecache.Informer, mode string, tickPeriod *time.Duration, nodeCIDRMaskSizeIPv6 int) (CIDRAllocator, error) {
	if client == nil {
		klog.Fatalf("kubeClient is nil when starting NodeController")
	}

	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cidrAllocator"})
	eventBroadcaster.StartStructuredLogging(0)
	klog.V(0).Infof("Sending events to api server.")
	eventBroadcaster.StartRecordingToSink(&coreV1Client.EventSinkImpl{Interface: client.Events("")})

	// create a cidrSet for each CIDR we operate on.
	// cidrSet are mapped to clusterCIDR by index
	cidrSets := make([]*cidrset.CidrSet, len(allocatorParams.ClusterCIDRs))
	for idx, cidr := range allocatorParams.ClusterCIDRs {
		cidrSet, err := cidrset.NewCIDRSet(cidr, allocatorParams.NodeCIDRMaskSizes[idx])
		if err != nil {
			return nil, err
		}
		cidrSets[idx] = cidrSet
	}

	ca := &cidrAllocator{
		coreV1Client:          client,
		clusterCIDRs:          allocatorParams.ClusterCIDRs,
		ec2Client:             ec2Client,
		cidrSets:              cidrSets,
		nodeCIDRUpdateChannel: make(chan NodeReservedCIDRs, CidrUpdateQueueSize),
		recorder:              recorder,
		nodesInProcessing:     sets.New[string](),
		nodesSynced:           nodeInformer.HasSynced,
		mode:                  Mode(mode),
		tickPeriod:            *tickPeriod,
		nodeCIDRMaskSizeIPv6:  nodeCIDRMaskSizeIPv6,
	}

	nodeList, err := client.Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing nodes: %w", err)
	}
	if nodeList != nil {
		for _, node := range nodeList.Items {
			if len(node.Spec.PodCIDRs) == 0 {
				klog.V(4).Infof("Node %v has no CIDR, ignoring", node.Name)
				continue
			}
			klog.V(4).Infof("Node %v has CIDR %s, occupying it in CIDR map", node.Name, node.Spec.PodCIDR)
			if err := ca.occupyCIDRs(&node); err != nil {
				// This will happen if:
				// 1. We find garbage in the podCIDRs field. Retrying is useless.
				// 2. CIDR out of range: This means a node CIDR has changed.
				// This error will keep crashing aws-ipam-controller.
				return nil, err
			}
		}
	}

	return ca, nil
}

func (c *cidrAllocator) Run(ctx context.Context, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting CIDR allocator")
	defer klog.Infof("Shutting down CIDR allocator")

	if !cache.WaitForNamedCacheSync("CIDR allocator", stopCh, c.nodesSynced) {
		return
	}

	for i := 0; i < cidrUpdateWorkers; i++ {
		go c.worker(ctx, stopCh)
	}

	<-stopCh
}

func (c *cidrAllocator) worker(ctx context.Context, stopChan <-chan struct{}) {
	for {
		select {
		case workItem, ok := <-c.nodeCIDRUpdateChannel:
			if !ok {
				klog.Warning("Channel nodeCIDRUpdateChannel was unexpectedly closed")
				return
			}
			if err := c.updateCIDRsAllocation(ctx, workItem); err != nil {
				// Requeue the failed node for update again.

				c.nodeCIDRUpdateChannel <- workItem
			}
		case <-stopChan:
			return
		}
	}
}

func (c *cidrAllocator) insertNodeToProcessing(nodeName string) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.nodesInProcessing.Has(nodeName) {
		return false
	}
	c.nodesInProcessing.Insert(nodeName)

	return true
}

func (c *cidrAllocator) removeNodeFromProcessing(nodeName string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.nodesInProcessing.Delete(nodeName)

}

// marks node.PodCIDRs[...] as used in allocator's tracked cidrSet
func (c *cidrAllocator) occupyCIDRs(node *v1.Node) error {
	defer c.removeNodeFromProcessing(node.Name)
	if len(node.Spec.PodCIDRs) == 0 {
		return nil
	}
	for idx, cidr := range node.Spec.PodCIDRs {
		_, podCIDR, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse node %s, CIDR %s", node.Name, node.Spec.PodCIDR)
		}

		// only track IPv4 CIDRs
		if netutils.IsIPv6CIDR(podCIDR) {
			continue
		}

		// If node has a pre allocate cidr that does not exist in our cidrs.
		// This will happen if cluster went from dualstack(multi cidrs) to non-dualstack
		// then we have now way of locking it
		//if idx >= len(c.cidrSets) {
		//	return fmt.Errorf("node:%s has an allocated cidr: %v at index:%v that does not exist in cluster cidrs configuration", node.Name, cidr, idx)
		//}

		if err := c.cidrSets[idx].Occupy(podCIDR); err != nil {
			return fmt.Errorf("failed to mark cidr[%v] at idx [%v] as occupied for node: %v: %v", podCIDR, idx, node.Name, err)
		}
	}
	return nil
}

// WARNING: If you're adding any return calls or defer any more work from this
// function you have to make sure to update nodesInProcessing properly with the
// disposition of the node when the work is done.
func (c *cidrAllocator) AllocateOrOccupyCIDR(node *v1.Node) error {
	if node == nil {
		return nil
	}
	if !c.insertNodeToProcessing(node.Name) {
		klog.V(4).Infof("Node %v is already in a process of CIDR assignment.", node.Name)
		return nil
	}

	if len(node.Spec.PodCIDRs) > 0 {
		return c.occupyCIDRs(node)
	}
	// allocate and queue the assignment
	allocated := NodeReservedCIDRs{
		nodeName:       node.Name,
		allocatedCIDRs: make([]*net.IPNet, len(c.cidrSets)),
	}

	for idx := range c.cidrSets {
		podCIDR, err := c.cidrSets[idx].AllocateNext()
		if err != nil {
			c.removeNodeFromProcessing(node.Name)
			nodeutil.RecordNodeStatusChange(c.recorder, node, "CIDRNotAvailable")
			return fmt.Errorf("failed to allocate cidr from cluster cidr at idx:%v: %v", idx, err)
		}
		allocated.allocatedCIDRs[idx] = podCIDR
	}

	//queue the assignment
	klog.V(4).Infof("Putting node %s with CIDR %v into the work queue", node.Name, allocated.allocatedCIDRs)
	c.nodeCIDRUpdateChannel <- allocated
	return nil
}

// ReleaseCIDR marks node.podCIDRs[...] as unused in our tracked cidrSets
func (c *cidrAllocator) ReleaseCIDR(node *v1.Node) error {
	if node == nil || len(node.Spec.PodCIDRs) == 0 {
		return nil
	}

	for idx, cidr := range node.Spec.PodCIDRs {
		_, podCIDR, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse CIDR %s on Node %v: %v", cidr, node.Name, err)
		}
		// only track IPv4 CIDRs
		if netutils.IsIPv6CIDR(podCIDR) {
			continue
		}
		// If node has a pre allocate cidr that does not exist in our cidrs.
		// This will happen if cluster went from dualstack(multi cidrs) to non-dualstack
		// then we have now way of locking it
		//if idx >= len(c.cidrSets) {
		//	return fmt.Errorf("node:%s has an allocated cidr: %v at index:%v that does not exist in cluster cidrs configuration", node.Name, cidr, idx)
		//}

		klog.V(4).Infof("release CIDR %s for node:%v", cidr, node.Name)
		if err = c.cidrSets[idx].Release(podCIDR); err != nil {
			return fmt.Errorf("error when releasing CIDR %v: %v", cidr, err)
		}
	}
	return nil
}

// updateCIDRsAllocation assigns CIDR to Node and sends an update to the API server.
func (c *cidrAllocator) updateCIDRsAllocation(ctx context.Context, data NodeReservedCIDRs) error {
	// tick at beginning as it takes some time to for the node object to get the provider ID
	ticker := time.NewTicker(c.tickPeriod)
	<-ticker.C

	var err error
	var node *v1.Node

	cidrsString := cidrsAsString(data.allocatedCIDRs)
	node, err = c.coreV1Client.Nodes().Get(ctx, data.nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed while getting node %v for updating Node.Spec.PodCIDRs: %v", data.nodeName, err)
		return err
	}

	// if cidr list matches the proposed.
	// then we possibly updated this node
	// and just failed to ack the success.

	if len(node.Spec.PodCIDRs) == len(data.allocatedCIDRs) {
		match := true
		for idx, cidr := range cidrsString {
			if node.Spec.PodCIDRs[idx] != cidr {
				match = false
				break
			}
		}
		if match {
			klog.V(4).Infof("Node %v already has allocated CIDR %v. It matches the proposed one.", node.Name, data.allocatedCIDRs)
			c.removeNodeFromProcessing(data.nodeName)
			return nil
		}
	}

	// node has cidrs, release the reserved
	if len(node.Spec.PodCIDRs) != 0 {
		klog.Errorf("Node %v already has a CIDR allocated %v. Releasing the new one.", node.Name, node.Spec.PodCIDRs)
		for idx, cidr := range data.allocatedCIDRs {
			if releaseErr := c.cidrSets[idx].Release(cidr); releaseErr != nil {
				klog.Errorf("Error when releasing CIDR idx:%v value: %v err:%v", idx, cidr, releaseErr)
			}
		}
		c.removeNodeFromProcessing(data.nodeName)
		return nil
	}

	switch c.mode {
	case IPv4:
		// nothing to do for IPv4 case
	case DualStack:
		ipv6Address, err := c.fetchIPv6Address(ctx, node)
		if err != nil {
			klog.Errorf("Error when fetching IPv6 Address. Err:%v", err)
			return err
		}
		cidrsString = append(cidrsString, changeNetmask(ipv6Address, fmt.Sprintf("%v", c.nodeCIDRMaskSizeIPv6)))
	case IPv6:
		ipv6Address, err := c.fetchIPv6Address(ctx, node)
		if err != nil {
			klog.Errorf("Error when fetching IPv6 Address. Err:%v", err)
			return err
		}
		cidrsString = []string{changeNetmask(ipv6Address, fmt.Sprintf("%v", c.nodeCIDRMaskSizeIPv6))}
	}

	// If we reached here, it means that the node has no CIDR currently assigned. So we set it.
	for i := 0; i < cidrUpdateRetries; i++ {
		if err = nodeutil.PatchNodePodCIDRs(c.coreV1Client, node, cidrsString); err == nil {
			klog.Infof("Set node %v PodCIDR to %v", node.Name, cidrsString)
			c.removeNodeFromProcessing(data.nodeName)
			return nil
		}
	}
	// failed release back to the pool
	klog.Errorf("Failed to update node %v PodCIDR to %v after multiple attempts: %v", node.Name, cidrsString, err)
	nodeutil.RecordNodeStatusChange(c.recorder, node, "CIDRAssignmentFailed")
	// We accept the fact that we may leak CIDRs here. This is safer than releasing
	// them in case when we don't know if request went through.
	// NodeController restart will return all falsely allocated CIDRs to the pool.
	if !apierrors.IsServerTimeout(err) {
		klog.Errorf("CIDR assignment for node %v failed: %v. Releasing allocated CIDR", node.Name, err)
		for idx, cidr := range data.allocatedCIDRs {
			if releaseErr := c.cidrSets[idx].Release(cidr); releaseErr != nil {
				klog.Errorf("Error releasing allocated CIDR for node %v: %v", node.Name, releaseErr)
			}
		}
	}
	return err
}

// converts a slice of cidrs into <c-1>,<c-2>,<c-n>
func cidrsAsString(inCIDRs []*net.IPNet) []string {
	outCIDRs := make([]string, len(inCIDRs))
	for idx, inCIDR := range inCIDRs {
		outCIDRs[idx] = inCIDR.String()
	}
	return outCIDRs
}

func (c *cidrAllocator) ReadyChecker(_ *http.Request) error {
	return nil
}

func (c *cidrAllocator) HealthzChecker(_ *http.Request) error {
	return nil
}

func (c *cidrAllocator) fetchIPv6Address(ctx context.Context, node *v1.Node) (string, error) {
	if node.Spec.ProviderID == "" {
		return "", fmt.Errorf("node %q has empty provider ID", node.Name)
	}

	// aws:///eu-central-1a/i-07577a7bcf3e576f2
	providerURL, err := url.Parse(node.Spec.ProviderID)
	if err != nil {
		return "", fmt.Errorf("could not parse provider URL for Node %q", node.Name)
	}
	instanceID := strings.Split(providerURL.Path, "/")[2]

	klog.V(4).Infof("Instance ID of node is %q", instanceID)

	eni, err := c.ec2Client.DescribeNetworkInterfaces(
		ctx,
		&ec2.DescribeNetworkInterfacesInput{
			Filters: []ec2types.Filter{
				{
					Name: nodeutil.PtrTo("attachment.instance-id"),
					Values: []string{
						instanceID,
					},
				},
			},
		})
	if err != nil {
		return "", err
	}

	if len(eni.NetworkInterfaces) != 1 {
		return "", fmt.Errorf("unexpected number of network interfaces for instance %q: %v", instanceID, len(eni.NetworkInterfaces))
	}

	if len(eni.NetworkInterfaces[0].Ipv6Prefixes) != 1 {
		return "", fmt.Errorf("unexpected amount of ipv6 prefixes on interface %q: %v", *eni.NetworkInterfaces[0].NetworkInterfaceId, len(eni.NetworkInterfaces[0].Ipv6Prefixes))
	}

	return aws.ToString(eni.NetworkInterfaces[0].Ipv6Prefixes[0].Ipv6Prefix), nil
}

func changeNetmask(ipAddress string, newNetmask string) string {
	split := strings.Split(ipAddress, "/")
	oldMask, err := strconv.Atoi(split[1])
	if err != nil {
		return ipAddress
	}
	newMask, err := strconv.Atoi(newNetmask)
	if err != nil {
		return ipAddress
	}
	if oldMask < newMask {
		return fmt.Sprintf("%s/%s", split[0], newNetmask)
	}
	klog.V(2).Info("unexpected value for node-cidr-mask-size-ipv6 using ", oldMask)
	return ipAddress
}
