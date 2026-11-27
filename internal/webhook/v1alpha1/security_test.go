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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func securityCluster() *nifiv1alpha1.NiFiCluster {
	return &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{
		NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
	}}
}

func assertWarnings(t *testing.T, warnings []string, paths ...string) {
	t.Helper()
	if len(warnings) != len(paths) {
		t.Fatalf("warnings = %v, want %d", warnings, len(paths))
	}
	for _, p := range paths {
		found := false
		for _, w := range warnings {
			if strings.Contains(w, p+":") {
				found = true
			}
		}
		if !found {
			t.Errorf("no warning mentions %s: %v", p, warnings)
		}
	}
}

// each override that leaves the restricted Pod Security profile draws one warning
// naming its field; harmless overrides draw none.
func TestSecurityContextWarnings(t *testing.T) {
	yes, no := true, false
	root, uid, fsGroup := int64(0), int64(1001), int64(2000)
	unconfined := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}

	if w := securityContextWarnings(securityCluster()); len(w) != 0 {
		t.Errorf("no overrides: warnings = %v, want none", w)
	}

	c := securityCluster()
	c.Spec.ContainerSecurityContext = &corev1.SecurityContext{
		Privileged:               &yes,
		AllowPrivilegeEscalation: &yes,
		ReadOnlyRootFilesystem:   &no,
		Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
		SeccompProfile:           unconfined,
		RunAsUser:                &root,
		RunAsNonRoot:             &no,
	}
	assertWarnings(t, securityContextWarnings(c),
		"spec.containerSecurityContext.privileged",
		"spec.containerSecurityContext.allowPrivilegeEscalation",
		"spec.containerSecurityContext.readOnlyRootFilesystem",
		"spec.containerSecurityContext.capabilities.add",
		"spec.containerSecurityContext.capabilities.drop",
		"spec.containerSecurityContext.seccompProfile.type",
		"spec.containerSecurityContext.runAsUser",
		"spec.containerSecurityContext.runAsNonRoot")

	c = securityCluster()
	c.Spec.PodSecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &no, RunAsUser: &root, SeccompProfile: unconfined}
	assertWarnings(t, securityContextWarnings(c),
		"spec.podSecurityContext.runAsNonRoot",
		"spec.podSecurityContext.runAsUser",
		"spec.podSecurityContext.seccompProfile.type")

	c = securityCluster()
	c.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{
		ContainerSecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: &no},
	}
	assertWarnings(t, securityContextWarnings(c),
		"spec.nodeGroups[0].podOverrides.containerSecurityContext.readOnlyRootFilesystem")

	c = securityCluster()
	c.Spec.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: &uid, FSGroup: &fsGroup}
	c.Spec.ContainerSecurityContext = &corev1.SecurityContext{RunAsUser: &uid, ReadOnlyRootFilesystem: &yes}
	if w := securityContextWarnings(c); len(w) != 0 {
		t.Errorf("harmless overrides: warnings = %v, want none", w)
	}
}

func TestUserContainerSecurityContextWarnings(t *testing.T) {
	yes := true
	c := securityCluster()
	c.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{
		InitContainers: []corev1.Container{{Name: "a"}},
		Sidecars: []corev1.Container{{Name: "b", SecurityContext: &corev1.SecurityContext{
			Privileged:   &yes,
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}}},
	}
	assertWarnings(t, securityContextWarnings(c),
		"spec.nodeGroups[0].podOverrides.initContainers[0]",
		"spec.nodeGroups[0].podOverrides.sidecars[0].securityContext.privileged")
}

func TestCapabilitiesWithoutDropAllWarns(t *testing.T) {
	c := securityCluster()
	c.Spec.ContainerSecurityContext = &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"NET_RAW"}},
	}
	assertWarnings(t, securityContextWarnings(c), "spec.containerSecurityContext.capabilities.drop")

	c.Spec.ContainerSecurityContext.Capabilities.Drop = []corev1.Capability{"ALL"}
	if w := securityContextWarnings(c); len(w) != 0 {
		t.Errorf("drop ALL: warnings = %v, want none", w)
	}
}

// the operator's init container name is reserved, like the main container's.
func TestPodOverridesForbidInitContainerName(t *testing.T) {
	path := field.NewPath("spec", "nodeGroups").Index(0).Child("podOverrides")
	for _, overrides := range []*nifiv1alpha1.PodOverridesSpec{
		{InitContainers: []corev1.Container{{Name: "nifi-init", Image: "evil"}}},
		{Sidecars: []corev1.Container{{Name: "nifi-init", Image: "evil"}}},
	} {
		errs := validatePodOverrides(path, overrides)
		if len(errs) != 1 || errs[0].Type != field.ErrorTypeForbidden {
			t.Errorf("overrides %+v: errs = %v, want one Forbidden", overrides, errs)
		}
	}
}
