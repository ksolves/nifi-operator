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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func nodeGroupValidator(t *testing.T, objs ...client.Object) *NiFiNodeGroupCustomValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &NiFiNodeGroupCustomValidator{cluster: NiFiClusterCustomValidator{Client: c, Reader: c}}
}

func ngCluster(tls bool, groups ...string) *nifiv1alpha1.NiFiCluster {
	c := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ns"}}
	for _, g := range groups {
		ng := nifiv1alpha1.NodeGroup{Name: g, Replicas: 1, Storage: "1Gi"}
		if tls {
			ng.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
		}
		c.Spec.NodeGroups = append(c.Spec.NodeGroups, ng)
	}
	return c
}

func ngObject(name string) *nifiv1alpha1.NiFiNodeGroup {
	return &nifiv1alpha1.NiFiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: nifiv1alpha1.NiFiNodeGroupSpec{ClusterRef: "prod",
			NodeGroup: nifiv1alpha1.NodeGroup{Name: name, Replicas: 1, Storage: "1Gi"}},
	}
}

func TestNiFiNodeGroupDefaultsName(t *testing.T) {
	o := ngObject("workers")
	o.Spec.Name = ""
	if err := (&NiFiNodeGroupCustomDefaulter{}).Default(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if o.Spec.Name != "workers" || o.Spec.Resources.Requests == nil {
		t.Errorf("defaulted spec = %+v", o.Spec.NodeGroup)
	}
}

// a NiFiNodeGroup is validated as a node group of its cluster.
func TestNiFiNodeGroupValidation(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		cluster *nifiv1alpha1.NiFiCluster
		group   *nifiv1alpha1.NiFiNodeGroup
		wantErr string
		warn    string
	}{
		{"joins a cluster", ngCluster(false, "main"), ngObject("workers"), "", ""},
		{"name taken by an inline group", ngCluster(false, "main"), ngObject("main"), "Duplicate", ""},
		{"TLS differs from the cluster's", ngCluster(true, "main"), ngObject("workers"), "tls", ""},
		{"cluster not created yet", nil, ngObject("workers"), "", "does not exist"},
		{"cluster at 3 nodes with it", ngCluster(false, "main", "second"), ngObject("workers"), "", ""},
		{"cluster past 3 nodes with it", ngCluster(false, "main", "second", "third"), ngObject("workers"), "at most 3", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var objs []client.Object
			if c.cluster != nil {
				objs = append(objs, c.cluster)
			}
			warnings, err := nodeGroupValidator(t, objs...).validate(ctx, nil, c.group)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("error = %v, want one containing %q", err, c.wantErr)
			}
			if c.warn != "" && !strings.Contains(strings.Join(warnings, " "), c.warn) {
				t.Errorf("warnings = %v, want %q", warnings, c.warn)
			}
		})
	}
}

// A TLS mismatch points at the NiFiNodeGroup's own spec.tls.enabled, not the merged list.
func TestNiFiNodeGroupTLSMismatchPath(t *testing.T) {
	_, err := nodeGroupValidator(t, ngCluster(true, "main")).validate(context.Background(), nil, ngObject("workers"))
	if err == nil || !strings.HasPrefix(err.Error(), "spec.tls.enabled: Invalid value: false") ||
		strings.Contains(err.Error(), "spec.tls[") || strings.Contains(err.Error(), "spec.nodeGroups[") {
		t.Errorf("err = %v, want spec.tls.enabled", err)
	}
}

// A storage change, shrink or grow, points at the NiFiNodeGroup's own spec.storage, not the
// merged list.
func TestNiFiNodeGroupStorageChangePath(t *testing.T) {
	for _, oldSize := range []string{"2Gi", "512Mi"} {
		old, ng := ngObject("workers"), ngObject("workers")
		old.Spec.Storage = oldSize
		_, err := nodeGroupValidator(t, ngCluster(false, "main")).ValidateUpdate(context.Background(), old, ng)
		if err == nil || !strings.Contains(err.Error(), "spec.storage") || strings.Contains(err.Error(), "spec.nodeGroups[") ||
			!strings.Contains(err.Error(), "not supported in the Community edition") {
			t.Errorf("from %s: err = %v, want spec.storage rejected", oldSize, err)
		}
	}
}

// Coordination ServiceAccount warnings cover only the NiFiNodeGroup's own group, not siblings.
func TestNiFiNodeGroupNoSiblingServiceAccountWarning(t *testing.T) {
	cluster := ngCluster(false, "main")
	cluster.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{ServiceAccountName: "sibling-sa"}
	warnings, err := nodeGroupValidator(t, cluster).validate(context.Background(), nil, ngObject("workers"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(strings.Join(warnings, " "), "sibling-sa") {
		t.Errorf("warnings = %v, want none about a sibling's ServiceAccount", warnings)
	}

	own := ngObject("workers")
	own.Spec.PodOverrides = &nifiv1alpha1.PodOverridesSpec{ServiceAccountName: "own-sa"}
	warnings, err = nodeGroupValidator(t, cluster).validate(context.Background(), nil, own)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if joined := strings.Join(warnings, " "); !strings.Contains(joined, "own-sa") || strings.Contains(joined, "sibling-sa") {
		t.Errorf("warnings = %v, want only the own ServiceAccount", warnings)
	}
}

// The cluster's webhook rejects an inline node group named like one of its NiFiNodeGroups, and
// warns when the cluster has no node group at all.
func TestNiFiClusterSeesItsNodeGroupObjects(t *testing.T) {
	v := nodeGroupValidator(t, ngObject("workers"))
	_, err := v.cluster.ValidateCreate(context.Background(), ngCluster(false, "workers"))
	if err == nil || !strings.Contains(err.Error(), "NiFiNodeGroup") {
		t.Errorf("inline group named like a NiFiNodeGroup: err = %v", err)
	}
	empty := ngCluster(false)
	empty.Name = "other"
	warnings, err := v.cluster.ValidateCreate(context.Background(), empty)
	if err != nil || !strings.Contains(strings.Join(warnings, " "), "no node groups") {
		t.Errorf("cluster without node groups: warnings %v, err %v", warnings, err)
	}
}

// a NiFiNodeGroup applied before its NiFiCluster is permission-checked right away; the
// cluster's webhook never re-checks it later.
func TestNiFiNodeGroupBeforeClusterIsAuthorized(t *testing.T) {
	ng := ngObject("workers")
	ng.Namespace = "team-a"
	ng.Spec.PodOverrides = &nifiv1alpha1.PodOverridesSpec{Sidecars: []corev1.Container{{Name: "shell", Image: "busybox"}}}
	ng.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{SecretName: "other-app"}

	v, _ := sarValidator(t)
	warnings, err := (&NiFiNodeGroupCustomValidator{cluster: *v}).validate(requestContext(), nil, ng)
	if err == nil || !strings.Contains(err.Error(), "create pods") || !strings.Contains(err.Error(), `read Secret "other-app"`) {
		t.Errorf("err = %v, want denials for podOverrides and the config Secret", err)
	}
	if err != nil && (!strings.Contains(err.Error(), "spec.podOverrides") || strings.Contains(err.Error(), "nodeGroups[")) {
		t.Errorf("err = %v, want paths in the NiFiNodeGroup's own spec", err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "does not exist") {
		t.Errorf("warnings = %v, want the missing-cluster warning", warnings)
	}

	v, asked := sarValidator(t, "pods/", "secrets/")
	if _, err := (&NiFiNodeGroupCustomValidator{cluster: *v}).validate(requestContext(), nil, ng); err != nil {
		t.Errorf("user allowed to create pods and read the Secret denied: %v", err)
	}
	if len(*asked) != 2 || (*asked)[0].Namespace != "team-a" {
		t.Errorf("reviews asked = %+v, want two in namespace team-a", *asked)
	}

	// An update that leaves those fields alone is not re-checked.
	v, asked = sarValidator(t)
	scaled := ng.DeepCopy()
	scaled.Spec.Replicas = 2
	if _, err := (&NiFiNodeGroupCustomValidator{cluster: *v}).ValidateUpdate(requestContext(), ng, scaled); err != nil || len(*asked) != 0 {
		t.Errorf("unchanged fields re-checked: err = %v, asked = %+v", err, *asked)
	}
}

// A cluster pushed past 3 nodes through /scale (which skips the webhook) can still shrink.
func TestNiFiNodeGroupScaleDownOverNodeLimit(t *testing.T) {
	ctx := context.Background()
	cluster := ngCluster(false, "main", "second")
	old := ngObject("workers")
	old.Spec.Replicas = 3 // 1 + 1 + 3 = 5
	v := nodeGroupValidator(t, cluster, old)
	smaller := old.DeepCopy()
	smaller.Spec.Replicas = 2 // 4: still over, but smaller
	if _, err := v.ValidateUpdate(ctx, old, smaller); err != nil {
		t.Errorf("scale-down over the limit refused: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, smaller, old); err == nil || !strings.Contains(err.Error(), "at most 3") {
		t.Errorf("scale-up over the limit: error = %v, want one containing %q", err, "at most 3")
	}
}

// No NiFiNodeGroup can be created in the Community edition; one from an earlier version can still
// be updated and deleted.
func TestNiFiNodeGroupCreateRejected(t *testing.T) {
	v := nodeGroupValidator(t, ngCluster(false))
	_, err := v.ValidateCreate(context.Background(), ngObject("workers"))
	if err == nil || !strings.Contains(err.Error(), "NiFiNodeGroup is not available in the Community edition") {
		t.Errorf("create: err = %v, want Forbidden", err)
	}
	old, ng := ngObject("workers"), ngObject("workers")
	ng.Spec.Replicas = 2
	if _, err := v.ValidateUpdate(context.Background(), old, ng); err != nil {
		t.Errorf("update: %v", err)
	}
	if _, err := v.ValidateDelete(context.Background(), ng); err != nil {
		t.Errorf("delete: %v", err)
	}
}
