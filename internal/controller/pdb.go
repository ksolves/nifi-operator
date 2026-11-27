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

package controller

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// pdbEnabled reports whether a PodDisruptionBudget should exist for this node group. Enabled by
// default; only an explicit PodDisruptionBudget.Enabled=false turns it off.
func pdbEnabled(nodeGroup *nifiv1alpha1.NodeGroup) bool {
	if nodeGroup.PodDisruptionBudget == nil || nodeGroup.PodDisruptionBudget.Enabled == nil {
		return true
	}
	return *nodeGroup.PodDisruptionBudget.Enabled
}

// pdbMinAvailable returns the effective minAvailable for a node group's PodDisruptionBudget.
// Defaults to Replicas-1 so at least one voluntary disruption is always tolerated; clamped to
// never reach or exceed Replicas, which would block all voluntary evictions outright.
func pdbMinAvailable(nodeGroup *nifiv1alpha1.NodeGroup) int32 {
	minAvailable := nodeGroup.Replicas - 1
	if nodeGroup.PodDisruptionBudget != nil && nodeGroup.PodDisruptionBudget.MinAvailable != nil {
		minAvailable = *nodeGroup.PodDisruptionBudget.MinAvailable
	}
	if minAvailable < 0 {
		minAvailable = 0
	}
	if minAvailable >= nodeGroup.Replicas {
		minAvailable = nodeGroup.Replicas - 1
		if minAvailable < 0 {
			minAvailable = 0
		}
	}
	return minAvailable
}

// buildPodDisruptionBudget renders the PodDisruptionBudget for a node group.
func buildPodDisruptionBudget(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt32(pdbMinAvailable(nodeGroup))
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:        statefulSetName(cluster, nodeGroup),
			Namespace:   cluster.Namespace,
			Labels:      commonLabels(cluster, nodeGroup),
			Annotations: mergedAnnotations(cluster, nodeGroup),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					labelAppName:     appNameValue,
					labelAppInstance: cluster.Name,
					labelNodeGroup:   nodeGroup.Name,
				},
			},
		},
	}
}
