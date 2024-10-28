// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gardener/aws-ipam-controller/pkg/ipam"
	"github.com/gardener/aws-ipam-controller/pkg/logger"
	"github.com/gardener/aws-ipam-controller/pkg/updater"

	nodeutil "github.com/gardener/aws-ipam-controller/pkg/node"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	coreV1Client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Version is injected by build
var Version string

const (
	// componentName is the component name
	componentName = "aws-ipam-controller"
	// leaderElectionId is the name of the lease resource
	leaderElectionId = "aws-ipam-controller-leader-election"
	// defaultNodeCIDRMaskSizeIPv4 is default mask size for IPv4 node cidr
	defaultNodeCIDRMaskSizeIPv4 = int(24)
	// defaultNodeCIDRMaskSizeIPv6 is default mask size for IPv6 node cidr
	defaultNodeCIDRMaskSizeIPv6 = int(80)
)

var (
	clusterName             = pflag.String("cluster-name", "", "cluster name used for AWS tags")
	clusterCIDRs            = pflag.String("cluster-cidrs", "", "cluster CIDRs")
	nodeCIDRMaskSizeIPv4    = pflag.Int("node-cidr-mask-size-ipv4", defaultNodeCIDRMaskSizeIPv4, "node CIDR mask size for IPv4 CIDR range")
	nodeCIDRMaskSizeIPv6    = pflag.Int("node-cidr-mask-size-ipv6", defaultNodeCIDRMaskSizeIPv6, "node CIDR mask size for IPv6 CIDR range")
	mode                    = pflag.String("mode", "ipv6", "mode used for aws-ipam-controller. Must be one of [ipv4,dual-stack,ipv6]")
	controlKubeconfig       = pflag.String("control-kubeconfig", updater.InClusterConfig, fmt.Sprintf("path of control plane kubeconfig or '%s' for in-cluster config", updater.InClusterConfig))
	healthProbePort         = pflag.Int("health-probe-port", 8081, "port for health probes")
	metricsPort             = pflag.Int("metrics-port", 8080, "port for metrics")
	namespace               = pflag.String("namespace", "", "namespace of secret containing the AWS credentials on control plane")
	region                  = pflag.String("region", "", "AWS region")
	secretName              = pflag.String("secret-name", "cloudprovider", "name of secret containing the AWS credentials of shoot cluster")
	targetKubeconfig        = pflag.String("target-kubeconfig", "", "path of target kubeconfig/shoot-kubeconfig")
	leaderElection          = pflag.Bool("leader-election", false, "enable leader election")
	leaderElectionNamespace = pflag.String("leader-election-namespace", "kube-system", "namespace for the lease resource")
	tickPeriod              = pflag.Duration("tick-period", 500*time.Millisecond, "tick period between CIDR updates on worker (default 500 ms)")
	logLevel                = pflag.String("log-level", logger.InfoLevel, "LogLevel is the level/severity for the logs. Must be one of [info,debug,error].")
	logFormat               = pflag.String("log-format", logger.FormatJSON, "output format for the logs. Must be one of [text,json].")
)

func main() {
	ctx := context.Background()
	logf.SetLogger(logger.MustNewZapLogger(*logLevel, *logFormat))
	var log = logf.Log.WithName(componentName)
	klog.SetLogger(log)
	klog.Info("Version: ", Version)
	klog.InitFlags(nil)
	pflag.Parse()

	defer klog.Flush()

	checkRequiredFlag("mode", *mode)
	checkRequiredFlag("namespace", *namespace)
	checkRequiredFlag("secret-name", *secretName)
	checkRequiredFlag("cluster-name", *clusterName)
	checkRequiredFlag("region", *region)
	checkRequiredFlag("target-kubeconfig", *targetKubeconfig)

	clusterCIDRs, _, err := processCIDRs(*clusterCIDRs)
	if err != nil {
		klog.Error(err, " could not parse clusterCIDRs")
		os.Exit(1)
	}

	targetConfig, err := clientcmd.BuildConfigFromFlags("", *targetKubeconfig)
	if err != nil {
		klog.Error(err, " could not use target kubeconfig", "target-kubeconfig", *targetKubeconfig)
		os.Exit(1)
	}
	options := manager.Options{
		LeaderElection:             *leaderElection,
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaderElectionID:           leaderElectionId,
		LeaderElectionNamespace:    *leaderElectionNamespace,
		Metrics: server.Options{
			BindAddress: fmt.Sprintf(":%d", *metricsPort),
		},
		HealthProbeBindAddress: fmt.Sprintf(":%d", *healthProbePort),
	}

	mgr, err := manager.New(targetConfig, options)
	if err != nil {
		klog.Error(err, " could not create manager")
		os.Exit(1)
	}

	nodeInformer, err := mgr.GetCache().GetInformer(ctx, &corev1.Node{})
	if err != nil {
		klog.Error(err, " unable to get setup components informer")
		os.Exit(1)
	}

	coreV1Client, err := coreV1Client.NewForConfig(mgr.GetConfig())
	if err != nil {
		klog.Error(err, " could not build coreV1Client ", err)
		os.Exit(1)
	}

	credentials, err := updater.LoadCredentials(*controlKubeconfig, *namespace, *secretName)
	if err != nil {
		klog.Error(err, " could not load AWS credentials", "namespace", *namespace, "secretName", *secretName)
		os.Exit(1)
	}

	ec2Client, err := updater.NewAWSEC2V2(ctx, credentials, *region)
	if err != nil {
		klog.Error(err, " could not create AWS EC2 client")
		os.Exit(1)
	}

	// get list of node cidr mask sizes
	nodeCIDRMaskSizes, err := setNodeCIDRMaskSizes(clusterCIDRs, *nodeCIDRMaskSizeIPv4, *nodeCIDRMaskSizeIPv6)
	if err != nil {
		klog.Error(err, " could not set node CIDR mask sizes")
		os.Exit(1)
	}

	allocatorParams := ipam.CIDRAllocatorParams{
		ClusterCIDRs:      clusterCIDRs,
		NodeCIDRMaskSizes: nodeCIDRMaskSizes,
	}

	cidrAllocator, err := ipam.NewCIDRRangeAllocator(ctx, coreV1Client, ec2Client, allocatorParams, nodeInformer, *mode, tickPeriod, *nodeCIDRMaskSizeIPv6)
	if err != nil {
		klog.Error(err, " could not create CIDR range allocator")
		os.Exit(1)
	}

	err = mgr.AddReadyzCheck("node reconciler", cidrAllocator.ReadyChecker)
	if err != nil {
		klog.Error(err, " could not add ready checker")
		os.Exit(1)
	}
	err = mgr.AddHealthzCheck("node reconciler", cidrAllocator.HealthzChecker)
	if err != nil {
		klog.Error(err, " could not add healthz checker")
		os.Exit(1)
	}

	_, err = nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: nodeutil.CreateAddNodeHandler(cidrAllocator.AllocateOrOccupyCIDR),
		UpdateFunc: nodeutil.CreateUpdateNodeHandler(func(_, newNode *corev1.Node) error {
			// If the PodCIDRs list is not empty we either:
			// - already processed a Node that already had CIDRs after NC restarted
			//   (cidr is marked as used),
			// - already processed a Node successfully and allocated CIDRs for it
			//   (cidr is marked as used),
			// - already processed a Node but we did saw a "timeout" response and
			//   request eventually got through in this case we haven't released
			//   the allocated CIDRs (cidr is still marked as used).
			// There's a possible error here:
			// - NC sees a new Node and assigns CIDRs X,Y.. to it,
			// - Update Node call fails with a timeout,
			// - Node is updated by some other component, NC sees an update and
			//   assigns CIDRs A,B.. to the Node,
			// - Both CIDR X,Y.. and CIDR A,B.. are marked as used in the local cache,
			//   even though Node sees only CIDR A,B..
			// The problem here is that in in-memory cache we see CIDR X,Y.. as marked,
			// which prevents it from being assigned to any new node. The cluster
			// state is correct.
			// Restart of NC fixes the issue.
			if len(newNode.Spec.PodCIDRs) == 0 {
				return cidrAllocator.AllocateOrOccupyCIDR(newNode)
			}
			return nil
		}),
		DeleteFunc: nodeutil.CreateDeleteNodeHandler(cidrAllocator.ReleaseCIDR),
	})
	if err != nil {
		klog.Error(err, " unable to add components informer event handler")
		os.Exit(1)
	}

	// Create the stopCh channel
	stopCh := make(chan struct{})
	go cidrAllocator.Run(ctx, stopCh)

	ctx = signals.SetupSignalHandler()
	if err := mgr.Start(ctx); err != nil {
		klog.Error(err, " could not start manager")
		os.Exit(1)
	}
}

func checkRequiredFlag(name, value string) {
	if value == "" {
		klog.Info(fmt.Sprintf("'--%s' is required", name))
		pflag.Usage()
		os.Exit(1)
	}
}

// processCIDRs is a helper function that works on a comma separated cidrs and returns
// a list of typed cidrs
// a flag if cidrs represents a dual stack
// error if failed to parse any of the cidrs
func processCIDRs(cidrsList string) ([]*net.IPNet, bool, error) {
	cidrsSplit := strings.Split(strings.TrimSpace(cidrsList), ",")
	klog.Info("Cluster CIDRs: ", cidrsSplit)
	cidrs, err := netutils.ParseCIDRs(cidrsSplit)
	if err != nil {
		return nil, false, err
	}

	// if cidrs has an error then the previous call will fail
	// safe to ignore error checking on next call
	dualstack, _ := netutils.IsDualStackCIDRs(cidrs)

	return cidrs, dualstack, nil
}

// setNodeCIDRMaskSizes returns the IPv4 and IPv6 node cidr mask sizes to the value provided or default values
func setNodeCIDRMaskSizes(clusterCIDRs []*net.IPNet, ipv4Mask, ipv6Mask int) ([]int, error) {

	sortedSizes := func(maskSizeIPv4, maskSizeIPv6 int) []int {
		nodeMaskCIDRs := make([]int, len(clusterCIDRs))

		for idx, clusterCIDR := range clusterCIDRs {
			if netutils.IsIPv6CIDR(clusterCIDR) {
				nodeMaskCIDRs[idx] = maskSizeIPv6
			} else {
				nodeMaskCIDRs[idx] = maskSizeIPv4
			}
		}
		return nodeMaskCIDRs
	}

	if len(clusterCIDRs) > 1 {
		return sortedSizes(ipv4Mask, ipv6Mask), nil
	} else if len(clusterCIDRs) == 1 && netutils.IsIPv6CIDR(clusterCIDRs[0]) {
		return []int{ipv6Mask}, nil
	}
	return []int{ipv4Mask}, nil
}
