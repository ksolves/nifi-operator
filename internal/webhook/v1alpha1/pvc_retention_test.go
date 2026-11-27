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

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// each node group whose PVCs are deleted is named, nothing is said for Retain.
func TestPVCDeleteWarnings(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "a"}, {Name: "b"}}}}
	if w := pvcDeleteWarnings(cluster); len(w) != 0 {
		t.Fatalf("default policy warned: %v", w)
	}

	cluster.Spec.PersistentVolumeClaimRetentionPolicy = &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCDelete}
	cluster.Spec.NodeGroups[1].PersistentVolumeClaimRetentionPolicy = &nifiv1alpha1.PVCRetentionPolicy{
		WhenDeleted: nifiv1alpha1.PVCRetain, WhenScaled: nifiv1alpha1.PVCDelete,
	}
	w := pvcDeleteWarnings(cluster)
	if len(w) != 2 {
		t.Fatalf("want 2 warnings, got %v", w)
	}
	if !strings.Contains(w[0], `"a"`) || !strings.Contains(w[0], "whenDeleted") {
		t.Errorf("group a's whenDeleted not named: %q", w[0])
	}
	if !strings.Contains(w[1], `"b"`) || !strings.Contains(w[1], "whenScaled") {
		t.Errorf("group b's whenScaled not named: %q", w[1])
	}
}

// TLS off warns that NiFi serves plain HTTP with no way to log in (NIFI-248 A3); TLS on, or no
// node groups at all, stays quiet.
func TestTLSDisabledWarnings(t *testing.T) {
	noGroups := &nifiv1alpha1.NiFiCluster{}
	if w := tlsDisabledWarnings(noGroups); len(w) != 0 {
		t.Fatalf("no node groups warned: %v", w)
	}

	tlsOn := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
		{Name: "a", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
	}}}
	if w := tlsDisabledWarnings(tlsOn); len(w) != 0 {
		t.Fatalf("tls.enabled: true warned: %v", w)
	}

	tlsOff := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
		{Name: "a", TLS: &nifiv1alpha1.TLSSpec{Enabled: false}},
	}}}
	w := tlsDisabledWarnings(tlsOff)
	if len(w) != 1 {
		t.Fatalf("want 1 warning for tls.enabled: false, got %v", w)
	}
	if !strings.Contains(w[0], "anonymous") {
		t.Errorf("warning doesn't explain the anonymous-access risk: %q", w[0])
	}

	noTLSBlock := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "a"}}}}
	if w := tlsDisabledWarnings(noTLSBlock); len(w) != 1 {
		t.Fatalf("want 1 warning when no tls block is set (defaults to disabled here), got %v", w)
	}
}
