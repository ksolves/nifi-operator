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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func TestOverrideKeyError(t *testing.T) {
	for key, wantOK := range map[string]bool{
		"nifi.ui.banner.text":        true,
		"java.arg.20":                true,
		"":                           false,
		"nifi.cluster.is.node ":      false, // trailing space ends the key early in Java Properties
		" nifi.cluster.is.node":      false,
		"nifi\\u002ecluster.is.node": false, // \u escape decodes to a reserved key
		"a=b":                        false,
		"a:b":                        false,
		"a\nnifi.cluster.is.node":    false,
		"#comment":                   false,
		"!comment":                   false,
	} {
		if got := OverrideKeyError(key) == ""; got != wantOK {
			t.Errorf("OverrideKeyError(%q) ok = %v, want %v", key, got, wantOK)
		}
	}
}

func TestOverrideValueError(t *testing.T) {
	for value, wantOK := range map[string]bool{
		`C:\data`:                       true,
		"60 secs":                       true,
		"x\nnifi.cluster.is.node=false": false,
		"x\rjava.arg.99=-Devil":         false,
	} {
		if got := OverrideValueError(value) == ""; got != wantOK {
			t.Errorf("OverrideValueError(%q) ok = %v, want %v", value, got, wantOK)
		}
	}
}

// TestRenderPropertiesEscapesAndDropsInjection: backslashes are escaped so Java Properties loads
// them verbatim, and entries that would inject a second property line are dropped
// even when they bypassed the webhook (e.g. via a ConfigOverrides Secret).
func TestRenderPropertiesEscapesAndDropsInjection(t *testing.T) {
	got := renderProperties(map[string]string{
		"nifi.ui.banner.text":  `C:\data`,
		"nifi.web.max.header":  "16 KB",
		"evil.value":           "x\njava.arg.99=-Devil",
		"nifi\\u002ecluster.x": "true",
	})
	want := "nifi.ui.banner.text=C:\\\\data\nnifi.web.max.header=16 KB\n"
	if got != want {
		t.Errorf("renderProperties() = %q, want %q", got, want)
	}
}

// TestApplyConfigOverrideSecretsPrecedence pins the four-layer order: cluster plain
// < cluster Secret < node-group plain < node-group Secret.
func TestApplyConfigOverrideSecretsPrecedence(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := func(name string, data map[string]string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Data: map[string][]byte{}}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		secret("cluster-secret", map[string]string{"a": "cluster-secret", "b": "cluster-secret", "c": "cluster-secret"}),
		secret("group-secret", map[string]string{"c": "group-secret"}),
	).Build()
	r := &NiFiClusterReconciler{Client: c}

	cluster := minimalCluster()
	cluster.Namespace = "ns"
	cluster.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
		NifiProperties: map[string]string{"a": "cluster-plain", "z": "cluster-plain"},
		SecretName:     "cluster-secret",
	}
	nodeGroup := &nifiv1alpha1.NodeGroup{Name: "default", ConfigOverrides: &nifiv1alpha1.ConfigOverridesSpec{
		NifiProperties: map[string]string{"b": "group-plain", "c": "group-plain"},
		SecretName:     "group-secret",
	}}

	layers, err := r.applyConfigOverrideSecrets(context.Background(), cluster, nodeGroup)
	if err != nil {
		t.Fatalf("applyConfigOverrideSecrets() error = %v", err)
	}
	got := map[string]string{}
	for k, v := range effectiveConfigOverrides(layers.cluster, layers.nodeGroup) {
		got[k] = "plain:" + v
	}
	for k, v := range layers.secret {
		if _, dup := got[k]; dup {
			t.Errorf("override %q is in both the ConfigMap and the config Secret", k)
		}
		got[k] = "secret:" + v
	}
	want := map[string]string{
		"z": "plain:cluster-plain",   // only layer
		"a": "secret:cluster-secret", // cluster Secret beats cluster plain
		"b": "plain:group-plain",     // node-group plain beats cluster Secret
		"c": "secret:group-secret",   // node-group Secret beats everything
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("override %q = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("overrides = %v, want exactly %v", got, want)
	}
	// no Secret-sourced value may reach the ConfigMap.
	if cm := buildConfigMap(layers.cluster, layers.nodeGroup); strings.Contains(cm.Data[nifiPropertiesOverridesKey], "secret") {
		t.Errorf("ConfigMap overrides contain a Secret-sourced value:\n%s", cm.Data[nifiPropertiesOverridesKey])
	}
	if layers.version == "" {
		t.Error("layers.version is empty, want the referenced Secrets' version hash")
	}
	if nodeGroup.ConfigOverrides.NifiProperties["c"] != "group-plain" || cluster.Spec.ConfigOverrides.NifiProperties["a"] != "cluster-plain" {
		t.Error("applyConfigOverrideSecrets mutated its inputs")
	}
}
