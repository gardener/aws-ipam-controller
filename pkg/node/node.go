/*
 * SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package node

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
)

// NodePatch holds the fields to patch
type NodePatch struct {
	Spec     *NodePatchSpec     `json:"spec,omitempty"`
	Metadata *NodePatchMetadata `json:"metadata,omitempty"`
}

// NodePatchSpec holds the spec for the node patch operation
type NodePatchSpec struct {
	PodCIDR  string   `json:"podCIDR,omitempty"`
	PodCIDRs []string `json:"podCIDRs,omitempty"`
}

// NodePatchMetadata holds the metadata for the node patch operation
type NodePatchMetadata struct {
	Labels map[string]*string `json:"labels,omitempty"`
}

// PatchNodePodCIDRs patches the node podCIDR to the specified value.
func PatchNodePodCIDRs(corev1client *corev1client.CoreV1Client, node *v1.Node, cidr []string) error {
	klog.Infof("assigning CIDR %q to node %q", cidr, node.ObjectMeta.Name)
	nodePatchSpec := &NodePatchSpec{
		PodCIDR:  cidr[0],
		PodCIDRs: cidr,
	}
	nodePatch := &NodePatch{
		Spec: nodePatchSpec,
	}
	nodePatchJSON, err := json.Marshal(nodePatch)
	if err != nil {
		return fmt.Errorf("error building node patch: %v", err)
	}

	klog.V(4).Infof("sending patch for node %q: %q", node.Name, string(nodePatchJSON))

	_, err = corev1client.Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, nodePatchJSON, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("error applying patch to node: %v", err)
	}

	return nil
}

// PtrTo returns a pointer to a copy of any value.
func PtrTo[T any](v T) *T {
	return &v
}
