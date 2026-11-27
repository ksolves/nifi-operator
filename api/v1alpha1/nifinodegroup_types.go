/*
Copyright 2026 Ksolves India Limited.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NiFiNodeGroupSpec is one node group of a NiFiCluster, defined as an object of its own so it can
// be scaled on its own: kubectl scale, HorizontalPodAutoscaler and KEDA all work
// through its /scale subresource. Every field of an inline nodeGroups entry applies, with the same
// validation rules; its name is the object's own name.
// +kubebuilder:validation:XValidation:rule="self.clusterRef == oldSelf.clusterRef",message="clusterRef is immutable: create a new NiFiNodeGroup to move nodes to another cluster",reason="FieldValueForbidden"
type NiFiNodeGroupSpec struct {
	// ClusterRef is the name of the NiFiCluster, in this namespace, this node group belongs to.
	// Immutable.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClusterRef string `json:"clusterRef"`

	NodeGroup `json:",inline"`
}

// NiFiNodeGroupStatus is the observed state of a NiFiNodeGroup, written by the NiFiCluster
// controller.
type NiFiNodeGroupStatus struct {
	// Replicas is the number of NiFi pods this group's StatefulSet currently has (observed, not
	// desired; 0 while the group is not reconciled). The /scale subresource reports it as the
	// current replica count.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of this group's NiFi pods that are Ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Selector is the label selector of this group's pods, in string form, for the /scale
	// subresource: a HorizontalPodAutoscaler reads it to find the pods whose metrics it averages.
	// +optional
	Selector string `json:"selector,omitempty"`

	// Conditions: Ready (every node of the group Ready) and Conflict (the group cannot be
	// reconciled: its cluster does not exist, or the cluster already has an inline node group of
	// the same name).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=nng
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NiFiNodeGroup is a node group of a NiFiCluster as an object of its own.
// +kubebuilder:validation:XValidation:rule="self.spec.name == self.metadata.name",message="spec.name must equal metadata.name (the webhook sets it when omitted)"
type NiFiNodeGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NiFiNodeGroupSpec   `json:"spec,omitempty"`
	Status NiFiNodeGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NiFiNodeGroupList contains a list of NiFiNodeGroup.
type NiFiNodeGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NiFiNodeGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NiFiNodeGroup{}, &NiFiNodeGroupList{})
}
