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
	"os"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// stock apache/nifi images run the operator's entrypoint, shipped in its own Secret - not the
// ConfigMap, which a NiFi pod from a different NiFiCluster in the same namespace can read and
// write (NIFI-248 B2).
func TestNiFiContainerRunsTheOperatorEntrypoint(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	cm := buildConfigMap(cluster, nodeGroup)
	if _, ok := cm.Data[entrypointKey]; ok {
		t.Fatalf("ConfigMap carries the entrypoint under %q; it must live only in the entrypoint Secret", entrypointKey)
	}
	secret := buildEntrypointSecret(cluster, nodeGroup)
	if string(secret.Data[entrypointKey]) != nifiEntrypoint || nifiEntrypoint == "" {
		t.Fatalf("entrypoint Secret %s does not carry the embedded entrypoint", entrypointKey)
	}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster))
	if err != nil {
		t.Fatal(err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	if want := []string{"sh", "/opt/nifi/entrypoint/entrypoint.sh"}; !slices.Equal(c.Command, want) {
		t.Errorf("nifi container command = %v, want %v", c.Command, want)
	}
	if c.Args != nil {
		t.Errorf("nifi container args = %v, want none", c.Args)
	}
}

// The custom node image bakes in the same file (images/nifi): one entrypoint, two copies.
func TestEntrypointCopiesMatch(t *testing.T) {
	image, err := os.ReadFile("../../images/nifi/scripts/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if string(image) != nifiEntrypoint {
		t.Error("images/nifi/scripts/entrypoint.sh differs from internal/controller/nifi-entrypoint.sh; keep them identical")
	}
}

func TestBuildStatefulSetRunAsUser(t *testing.T) {
	cluster := minimalCluster()
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}

	sts, err := buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster),
		statefulSetOptions{RunAsUser: ptr.To(int64(1000))})

	if err != nil {
		t.Fatal(err)
	}
	sc := sts.Spec.Template.Spec.SecurityContext
	if ptr.Deref(sc.RunAsUser, -1) != 1000 || ptr.Deref(sc.RunAsGroup, -1) != 0 {
		t.Errorf("runAsUser = %v, runAsGroup = %v, want 1000 and 0", sc.RunAsUser, sc.RunAsGroup)
	}
	if !slices.Equal(sc.SupplementalGroups, []int64{1000}) {
		t.Errorf("supplementalGroups = %v, want [1000]", sc.SupplementalGroups)
	}

	// Unset (OpenShift restricted-v2 assigns the UID): neither field.
	sts, err = buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster), statefulSetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sc = sts.Spec.Template.Spec.SecurityContext
	if sc.RunAsUser != nil || sc.RunAsGroup != nil {
		t.Errorf("runAsUser = %v, runAsGroup = %v, want both unset", sc.RunAsUser, sc.RunAsGroup)
	}
	// An assigned UID still reads the stock image's lib/ (770) and runs bin/nifi.sh (750).
	if !slices.Equal(sc.SupplementalGroups, []int64{1000}) {
		t.Errorf("supplementalGroups = %v, want [1000] with an assigned UID too", sc.SupplementalGroups)
	}

	// A cluster's own podSecurityContext still wins.
	cluster.Spec.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(2000))}
	sts, err = buildStatefulSet(cluster, nodeGroup, "hash", "", nifiPodServiceAccount(cluster),
		statefulSetOptions{RunAsUser: ptr.To(int64(1000))})

	if err != nil {
		t.Fatal(err)
	}
	if got := ptr.Deref(sts.Spec.Template.Spec.SecurityContext.RunAsUser, -1); got != 2000 {
		t.Errorf("runAsUser = %d, want the cluster's 2000", got)
	}

	// supplementalGroups set by the user are added to 1000, not put in its place.
	cluster.Spec.PodSecurityContext = &corev1.PodSecurityContext{SupplementalGroups: []int64{5000}}
	if got := effectivePodSecurityContext(cluster, nodeGroup, statefulSetOptions{}).SupplementalGroups; !slices.Equal(got, []int64{1000, 5000}) {
		t.Errorf("supplementalGroups = %v, want [1000 5000]", got)
	}
	cluster.Spec.PodSecurityContext = &corev1.PodSecurityContext{SupplementalGroups: []int64{5000, 1000}}
	if got := effectivePodSecurityContext(cluster, nodeGroup, statefulSetOptions{}).SupplementalGroups; !slices.Equal(got, []int64{5000, 1000}) {
		t.Errorf("supplementalGroups = %v, want [5000 1000] unchanged", got)
	}
}
