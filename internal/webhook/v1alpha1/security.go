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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// securityContextWarnings warns about each security context override that takes the NiFi pods
// out of the "restricted" Pod Security Standard. Only a warning: the override is
// deliberate, and setting it already needs permission to create pods (podShapingGrants).
func securityContextWarnings(cluster *nifiv1alpha1.NiFiCluster) admission.Warnings {
	spec := field.NewPath("spec")
	warnings := podSecurityContextWarnings(spec.Child("podSecurityContext"), cluster.Spec.PodSecurityContext)
	warnings = append(warnings,
		containerSecurityContextWarnings(spec.Child("containerSecurityContext"), cluster.Spec.ContainerSecurityContext)...)
	for i := range cluster.Spec.NodeGroups {
		warnings = append(warnings, nodeGroupSecurityContextWarnings(
			spec.Child("nodeGroups").Index(i).Child("podOverrides"), &cluster.Spec.NodeGroups[i])...)
	}
	return warnings
}

// nodeGroupSecurityContextWarnings is securityContextWarnings for one node group's podOverrides.
func nodeGroupSecurityContextWarnings(path *field.Path, nodeGroup *nifiv1alpha1.NodeGroup) admission.Warnings {
	if nodeGroup.PodOverrides == nil {
		return nil
	}
	o := nodeGroup.PodOverrides
	w := append(
		podSecurityContextWarnings(path.Child("podSecurityContext"), o.PodSecurityContext),
		containerSecurityContextWarnings(path.Child("containerSecurityContext"), o.ContainerSecurityContext)...)
	for _, group := range []struct {
		kind       string
		containers []corev1.Container
	}{{"initContainers", o.InitContainers}, {"sidecars", o.Sidecars}} {
		kind := group.kind
		for i, c := range group.containers {
			if c.SecurityContext == nil {
				w = append(w, fmt.Sprintf("%s: no securityContext, so it runs with the restricted defaults "+
					"(non-root, read-only root filesystem, no capabilities); set one if it needs more",
					path.Child(kind).Index(i)))
			} else {
				scPath := path.Child(kind).Index(i).Child("securityContext")
				w = append(w, containerSecurityContextWarnings(scPath, c.SecurityContext)...)
				// Not merged over the defaults: without capabilities the container keeps the
				// runtime's default ones.
				if c.SecurityContext.Capabilities == nil {
					w = append(w, restrictedWarning(scPath.Child("capabilities"), "unset (no drop ALL)"))
				}
			}
		}
	}
	return w
}

func podSecurityContextWarnings(path *field.Path, sc *corev1.PodSecurityContext) admission.Warnings {
	if sc == nil {
		return nil
	}
	var w admission.Warnings
	if sc.RunAsNonRoot != nil && !*sc.RunAsNonRoot {
		w = append(w, restrictedWarning(path.Child("runAsNonRoot"), "false"))
	}
	if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
		w = append(w, restrictedWarning(path.Child("runAsUser"), "0 (root)"))
	}
	if sc.SeccompProfile != nil && sc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
		w = append(w, restrictedWarning(path.Child("seccompProfile", "type"), "Unconfined"))
	}
	return w
}

func containerSecurityContextWarnings(path *field.Path, sc *corev1.SecurityContext) admission.Warnings {
	if sc == nil {
		return nil
	}
	var w admission.Warnings
	if sc.Privileged != nil && *sc.Privileged {
		w = append(w, restrictedWarning(path.Child("privileged"), "true"))
	}
	if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation {
		w = append(w, restrictedWarning(path.Child("allowPrivilegeEscalation"), "true"))
	}
	if sc.RunAsNonRoot != nil && !*sc.RunAsNonRoot {
		w = append(w, restrictedWarning(path.Child("runAsNonRoot"), "false"))
	}
	if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
		w = append(w, restrictedWarning(path.Child("runAsUser"), "0 (root)"))
	}
	if sc.ReadOnlyRootFilesystem != nil && !*sc.ReadOnlyRootFilesystem {
		w = append(w, restrictedWarning(path.Child("readOnlyRootFilesystem"), "false"))
	}
	if sc.SeccompProfile != nil && sc.SeccompProfile.Type == corev1.SeccompProfileTypeUnconfined {
		w = append(w, restrictedWarning(path.Child("seccompProfile", "type"), "Unconfined"))
	}
	if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
		w = append(w, restrictedWarning(path.Child("capabilities", "add"), fmt.Sprint(sc.Capabilities.Add)))
	}
	// capabilities replaces the default as a whole, so one without "drop: [ALL]" gives the default
	// capabilities back.
	if sc.Capabilities != nil && !slices.Contains(sc.Capabilities.Drop, "ALL") {
		w = append(w, restrictedWarning(path.Child("capabilities", "drop"), "without ALL"))
	}
	return w
}

func restrictedWarning(path *field.Path, value string) string {
	return fmt.Sprintf("%s: %s takes the NiFi pods out of the restricted Pod Security profile; "+
		"a namespace that enforces it (or OpenShift's restricted-v2 SCC) rejects them", path, value)
}
