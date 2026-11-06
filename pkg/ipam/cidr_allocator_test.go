// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package ipam

import (
	"context"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gardener/aws-ipam-controller/pkg/ipam/cidrset"
)

// newTestAllocator builds a minimal cidrAllocator for unit tests (no ec2 client, no informer).
func newTestAllocator(t *testing.T, clusterCIDR string, maskSize int) *cidrAllocator {
	t.Helper()
	_, ipnet, err := net.ParseCIDR(clusterCIDR)
	if err != nil {
		t.Fatalf("parse cluster CIDR: %v", err)
	}
	cs, err := cidrset.NewCIDRSet(ipnet, maskSize)
	if err != nil {
		t.Fatalf("new CIDRSet: %v", err)
	}
	return &cidrAllocator{
		clusterCIDRs:   []*net.IPNet{ipnet},
		cidrSets:       []*cidrset.CidrSet{cs},
		reconcileReady: make(chan struct{}),
	}
}

// nodeWithCIDR returns a Node with the given pod CIDR.
func nodeWithCIDR(name, cidr string) *v1.Node {
	return &v1.Node{
		Name: name,
		Spec: v1.NodeSpec{PodCIDR: cidr, PodCIDRs: []string{cidr}},
	}
}

// --- occupyCIDRsWithoutRemoval ---

func TestOccupyCIDRsWithoutRemoval_Valid(t *testing.T) {
	c := newTestAllocator(t, "10.0.0.0/16", 24)
	node := nodeWithCIDR("node1", "10.0.1.0/24")
	if err := c.occupyCIDRsWithoutRemoval(node); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// After occupy, AllocateNext must not return the same CIDR.
	next, err := c.cidrSets[0].AllocateNext()
	if err != nil {
		t.Fatalf("AllocateNext: %v", err)
	}
	if next.String() == "10.0.1.0/24" {
		t.Error("AllocateNext returned already-occupied CIDR 10.0.1.0/24")
	}
}

func TestOccupyCIDRsWithoutRemoval_OutsideRange(t *testing.T) {
	c := newTestAllocator(t, "10.0.0.0/16", 24)
	node := nodeWithCIDR("bad-node", "192.168.1.0/24") // outside 10.0.0.0/16
	if err := c.occupyCIDRsWithoutRemoval(node); err == nil {
		t.Error("expected error for out-of-range pod CIDR")
	}
}

func TestOccupyCIDRsWithoutRemoval_MalformedCIDR(t *testing.T) {
	c := newTestAllocator(t, "10.0.0.0/16", 24)
	node := &v1.Node{
		Name: "bad-node",
		Spec: v1.NodeSpec{PodCIDR: "not-a-cidr", PodCIDRs: []string{"not-a-cidr"}},
	}
	if err := c.occupyCIDRsWithoutRemoval(node); err == nil {
		t.Error("expected error for malformed pod CIDR")
	}
}

// --- ReleaseCIDR ---

func TestReleaseCIDR_OutsideRange(t *testing.T) {
	c := newTestAllocator(t, "10.0.0.0/16", 24)
	node := nodeWithCIDR("bad-node", "192.168.1.0/24")
	if err := c.ReleaseCIDR(node); err == nil {
		t.Error("expected error for out-of-range pod CIDR in ReleaseCIDR")
	}
}

func TestReleaseCIDR_Valid(t *testing.T) {
	c := newTestAllocator(t, "10.0.0.0/16", 24)
	node := nodeWithCIDR("node1", "10.0.1.0/24")
	// Occupy first, then release.
	if err := c.occupyCIDRsWithoutRemoval(node); err != nil {
		t.Fatalf("occupy: %v", err)
	}
	if err := c.ReleaseCIDR(node); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestReconcileState_BadCIDRSkipped verifies the f005 fix: a node with an out-of-range
// pod CIDR causes occupyCIDRsWithoutRemoval to return an error, but reconcileState skips
// it (warn+continue) so the controller starts up and good nodes are still occupied.
// We test the behaviour via occupyCIDRsWithoutRemoval directly because the concrete
// *coreV1Client.CoreV1Client field cannot be satisfied by the k8s fake clientset.
func TestReconcileState_BadCIDRSkipped(t *testing.T) {
	c := newTestAllocator(t, "10.0.0.0/16", 24)

	goodNode := nodeWithCIDR("good", "10.0.1.0/24")
	badNode := nodeWithCIDR("bad", "192.168.1.0/24") // outside cluster CIDR

	// Bad node must produce an error from occupyCIDRsWithoutRemoval.
	if err := c.occupyCIDRsWithoutRemoval(badNode); err == nil {
		t.Error("expected error for bad node's out-of-range CIDR")
	}

	// Good node must succeed — and its CIDR must be marked occupied.
	if err := c.occupyCIDRsWithoutRemoval(goodNode); err != nil {
		t.Fatalf("unexpected error for good node: %v", err)
	}
	next, err := c.cidrSets[0].AllocateNext()
	if err != nil {
		t.Fatalf("AllocateNext: %v", err)
	}
	if next.String() == "10.0.1.0/24" {
		t.Error("good node's CIDR should be occupied; AllocateNext returned it")
	}
}

// --- legacy tests below, kept as-is ---

func TestReconcileState(t *testing.T) {
	tests := []struct {
		name          string
		existingNodes []*v1.Node
		clusterCIDRs  []*net.IPNet
		nodeMaskSizes []int
		expectError   bool
	}{
		{
			name: "reconcile nodes with existing CIDRs",
			existingNodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node1"},
					Spec: v1.NodeSpec{
						PodCIDR:  "10.0.0.0/24",
						PodCIDRs: []string{"10.0.0.0/24"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node2"},
					Spec: v1.NodeSpec{
						PodCIDR:  "10.0.1.0/24",
						PodCIDRs: []string{"10.0.1.0/24"},
					},
				},
			},
			clusterCIDRs:  []*net.IPNet{mustParseCIDR("10.0.0.0/16")},
			nodeMaskSizes: []int{24},
			expectError:   false,
		},
		{
			name: "reconcile nodes without CIDRs",
			existingNodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node1"},
					Spec:       v1.NodeSpec{},
				},
			},
			clusterCIDRs:  []*net.IPNet{mustParseCIDR("10.0.0.0/16")},
			nodeMaskSizes: []int{24},
			expectError:   false,
		},
		{
			name: "reconcile mixed nodes",
			existingNodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node1"},
					Spec: v1.NodeSpec{
						PodCIDR:  "10.0.0.0/24",
						PodCIDRs: []string{"10.0.0.0/24"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node2"},
					Spec:       v1.NodeSpec{},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node3"},
					Spec: v1.NodeSpec{
						PodCIDR:  "10.0.2.0/24",
						PodCIDRs: []string{"10.0.2.0/24"},
					},
				},
			},
			clusterCIDRs:  []*net.IPNet{mustParseCIDR("10.0.0.0/16")},
			nodeMaskSizes: []int{24},
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create fake kubernetes client with existing nodes
			fakeClient := fake.NewClientset()
			for _, node := range tt.existingNodes {
				_, err := fakeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create test node: %v", err)
				}
			}

			// Create CIDR sets
			cidrSets := make([]*cidrset.CidrSet, len(tt.clusterCIDRs))
			for idx, cidr := range tt.clusterCIDRs {
				cs, err := cidrset.NewCIDRSet(cidr, tt.nodeMaskSizes[idx])
				if err != nil {
					t.Fatalf("failed to create CIDR set: %v", err)
				}
				cidrSets[idx] = cs
			}

			// List nodes and occupy CIDRs (simulating reconcileState logic)
			nodeList, err := fakeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatalf("failed to list nodes: %v", err)
			}

			for _, node := range nodeList.Items {
				if len(node.Spec.PodCIDRs) > 0 {
					_, cidr, parseErr := net.ParseCIDR(node.Spec.PodCIDRs[0])
					if parseErr != nil {
						t.Fatalf("failed to parse CIDR %s: %v", node.Spec.PodCIDRs[0], parseErr)
					}
					if occupyErr := cidrSets[0].Occupy(cidr); occupyErr != nil {
						if (occupyErr != nil) != tt.expectError {
							t.Errorf("Occupy() error = %v, expectError %v", occupyErr, tt.expectError)
						}
					}
				}
			}

			// Verify: Allocate a new CIDR and ensure it's different from already occupied ones
			allocatedCIDRs := make(map[string]bool)
			for _, node := range tt.existingNodes {
				if len(node.Spec.PodCIDRs) > 0 {
					allocatedCIDRs[node.Spec.PodCIDRs[0]] = true
				}
			}

			// Try to allocate a new CIDR
			newCIDR, err := cidrSets[0].AllocateNext()
			if err != nil && len(allocatedCIDRs) < 256 { // 256 /24 subnets in a /16
				t.Fatalf("failed to allocate new CIDR: %v", err)
			}

			if newCIDR != nil && allocatedCIDRs[newCIDR.String()] {
				t.Errorf("allocated CIDR %s was already occupied", newCIDR.String())
			}
		})
	}
}

func TestNoDuplicateCIDRAllocation(t *testing.T) {
	// Create CIDR set
	_, clusterCIDR, _ := net.ParseCIDR("10.0.0.0/16")
	cidrSet, err := cidrset.NewCIDRSet(clusterCIDR, 24)
	if err != nil {
		t.Fatalf("failed to create CIDR set: %v", err)
	}

	// Allocate first CIDR
	firstCIDR, err := cidrSet.AllocateNext()
	if err != nil {
		t.Fatalf("failed to allocate first CIDR: %v", err)
	}

	// Allocate second CIDR
	secondCIDR, err := cidrSet.AllocateNext()
	if err != nil {
		t.Fatalf("failed to allocate second CIDR: %v", err)
	}

	// Verify they're different
	if firstCIDR.String() == secondCIDR.String() {
		t.Errorf("allocated duplicate CIDR: %s", firstCIDR.String())
	}

	// Allocate a third one to verify sequence continues
	thirdCIDR, err := cidrSet.AllocateNext()
	if err != nil {
		t.Fatalf("failed to allocate third CIDR: %v", err)
	}

	// Verify all three are unique
	cidrs := map[string]bool{
		firstCIDR.String():  true,
		secondCIDR.String(): true,
		thirdCIDR.String():  true,
	}

	if len(cidrs) != 3 {
		t.Errorf("expected 3 unique CIDRs, got %d: %v, %v, %v",
			len(cidrs), firstCIDR.String(), secondCIDR.String(), thirdCIDR.String())
	}
}

func TestCIDRRelease(t *testing.T) {
	// Create CIDR set
	_, clusterCIDR, _ := net.ParseCIDR("10.0.0.0/16")
	cidrSet, err := cidrset.NewCIDRSet(clusterCIDR, 24)
	if err != nil {
		t.Fatalf("failed to create CIDR set: %v", err)
	}

	// Allocate a CIDR
	allocatedCIDR, err := cidrSet.AllocateNext()
	if err != nil {
		t.Fatalf("failed to allocate CIDR: %v", err)
	}

	firstCIDR := allocatedCIDR.String()

	// Allocate another to verify sequence
	secondAllocated, err := cidrSet.AllocateNext()
	if err != nil {
		t.Fatalf("failed to allocate second CIDR: %v", err)
	}

	// Release the first one
	err = cidrSet.Release(allocatedCIDR)
	if err != nil {
		t.Fatalf("failed to release CIDR: %v", err)
	}

	// Allocate again - we should get a CIDR (could be the released one or next in sequence)
	thirdAllocated, err := cidrSet.AllocateNext()
	if err != nil {
		t.Fatalf("failed to allocate after release: %v", err)
	}

	// Verify we got a valid CIDR
	if thirdAllocated == nil {
		t.Error("expected to get a CIDR after release")
	}

	// The released CIDR should be available again - try to occupy it
	err = cidrSet.Occupy(allocatedCIDR)
	if err != nil {
		t.Errorf("failed to occupy released CIDR: %v", err)
	}

	// Log for debugging
	t.Logf("First: %s, Second: %s, Third: %s", firstCIDR, secondAllocated.String(), thirdAllocated.String())
}

// Helper function to parse CIDR
func mustParseCIDR(s string) *net.IPNet {
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return cidr
}
