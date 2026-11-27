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
	"testing"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func int32Ptr(v int32) *int32 { return &v }
func boolPtr(v bool) *bool    { return &v }

func TestPdbMinAvailable(t *testing.T) {
	cases := []struct {
		name     string
		replicas int32
		pdb      *nifiv1alpha1.PodDisruptionBudgetSpec
		want     int32
	}{
		{name: "default is replicas-1", replicas: 3, want: 2},
		{name: "single replica defaults to 0", replicas: 1, want: 0},
		{name: "explicit value under replicas is honored", replicas: 5, pdb: &nifiv1alpha1.PodDisruptionBudgetSpec{MinAvailable: int32Ptr(3)}, want: 3},
		{name: "explicit value equal to replicas is clamped down", replicas: 3, pdb: &nifiv1alpha1.PodDisruptionBudgetSpec{MinAvailable: int32Ptr(3)}, want: 2},
		{name: "explicit value above replicas is clamped down", replicas: 3, pdb: &nifiv1alpha1.PodDisruptionBudgetSpec{MinAvailable: int32Ptr(99)}, want: 2},
		{name: "negative value is clamped to zero", replicas: 3, pdb: &nifiv1alpha1.PodDisruptionBudgetSpec{MinAvailable: int32Ptr(-1)}, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodeGroup := &nifiv1alpha1.NodeGroup{Replicas: c.replicas, PodDisruptionBudget: c.pdb}
			if got := pdbMinAvailable(nodeGroup); got != c.want {
				t.Errorf("pdbMinAvailable() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestPdbEnabled(t *testing.T) {
	if !pdbEnabled(&nifiv1alpha1.NodeGroup{}) {
		t.Error("pdbEnabled() should default to true when PodDisruptionBudget is unset")
	}
	if pdbEnabled(&nifiv1alpha1.NodeGroup{PodDisruptionBudget: &nifiv1alpha1.PodDisruptionBudgetSpec{Enabled: boolPtr(false)}}) {
		t.Error("pdbEnabled() should be false when explicitly disabled")
	}
	if !pdbEnabled(&nifiv1alpha1.NodeGroup{PodDisruptionBudget: &nifiv1alpha1.PodDisruptionBudgetSpec{Enabled: boolPtr(true)}}) {
		t.Error("pdbEnabled() should be true when explicitly enabled")
	}
}
