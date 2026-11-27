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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// Volume sizes and the StorageClass of an existing node group can't change.
func TestValidateStorageChanges(t *testing.T) {
	base := func() *nifiv1alpha1.NiFiCluster {
		return &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
			Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{
				Name: "default", Storage: "1Gi", LogStorage: "1Gi", StorageClassName: ptr.To("standard"),
				SplitRepositoryStorage: &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true, ContentRepositoryStorage: "5Gi"},
			}}}}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*nifiv1alpha1.NiFiCluster)
		wantErr string // substring of the single expected error; "" for none
	}{
		{"unchanged", func(*nifiv1alpha1.NiFiCluster) {}, ""},
		{"same size, other unit", func(c *nifiv1alpha1.NiFiCluster) { c.Spec.NodeGroups[0].Storage = "1024Mi" }, ""},
		{"replicas only", func(c *nifiv1alpha1.NiFiCluster) { c.Spec.NodeGroups[0].Replicas = 3 }, ""},
		{"new node group", func(c *nifiv1alpha1.NiFiCluster) {
			c.Spec.NodeGroups = append(c.Spec.NodeGroups, nifiv1alpha1.NodeGroup{Name: "extra", Storage: "9Gi"})
		}, ""},
		{"shrink", func(c *nifiv1alpha1.NiFiCluster) { c.Spec.NodeGroups[0].LogStorage = "512Mi" },
			"spec.nodeGroups[0].logStorage: Forbidden: cannot change from 1Gi to 512Mi"},
		{"grow", func(c *nifiv1alpha1.NiFiCluster) { c.Spec.NodeGroups[0].Storage = "2Gi" },
			"spec.nodeGroups[0].storage: Forbidden: cannot change from 1Gi to 2Gi: storage size changes are not supported"},
		{"grow a split repository", func(c *nifiv1alpha1.NiFiCluster) {
			c.Spec.NodeGroups[0].SplitRepositoryStorage.ContentRepositoryStorage = "10Gi"
		}, "splitRepositoryStorage.contentRepositoryStorage: Forbidden: cannot change from 5Gi to 10Gi"},
		{"storage class", func(c *nifiv1alpha1.NiFiCluster) { c.Spec.NodeGroups[0].StorageClassName = ptr.To("fast") },
			"spec.nodeGroups[0].storageClassName"},
		{"rename, same sizes", func(c *nifiv1alpha1.NiFiCluster) { c.Spec.NodeGroups[0].Name = "renamed" }, ""},
		{"rename and grow", func(c *nifiv1alpha1.NiFiCluster) {
			c.Spec.NodeGroups[0].Name = "renamed"
			c.Spec.NodeGroups[0].Storage = "100Gi"
		}, `spec.nodeGroups[0].storage: Forbidden: cannot change from 1Gi to 100Gi: storage size changes are not supported in the Community edition; a renamed node group keeps the sizes of "default"`},
		{"rename with another storage class", func(c *nifiv1alpha1.NiFiCluster) {
			c.Spec.NodeGroups[0].Name = "renamed"
			c.Spec.NodeGroups[0].StorageClassName = ptr.To("fast")
		}, ""},
		{"rename and grow the logs", func(c *nifiv1alpha1.NiFiCluster) {
			c.Spec.NodeGroups[0].Name = "renamed"
			c.Spec.NodeGroups[0].LogStorage = "2Gi"
		}, "spec.nodeGroups[0].logStorage: Forbidden: cannot change from 1Gi to 2Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := base()
			tc.mutate(updated)
			errs := (&NiFiClusterCustomValidator{}).validateStorageChanges(context.Background(), base(), updated)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("errs = %v, want none", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs.ToAggregate().Error(), tc.wantErr) {
				t.Fatalf("errs = %v, want one error containing %q", errs, tc.wantErr)
			}
		})
	}
}

// extraSANs are DNS names on every node certificate, so only plain hostnames pass.
func TestValidateNodeGroupExtraSANs(t *testing.T) {
	v := &NiFiClusterCustomValidator{}
	for san, wantOK := range map[string]bool{
		"nifi.example.com":   true,
		"nifi-0.svc.local":   true,
		"*.example.com":      false,
		"10.0.0.5":           false,
		"Upper.example.com":  false,
		"bad_name.example":   false,
		"ends-with-dot.com.": false,
	} {
		ng := &nifiv1alpha1.NodeGroup{Name: "default", Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true, ExtraSANs: []string{san}}}
		errs := v.validateNodeGroup(context.Background(), &nifiv1alpha1.NiFiCluster{}, field.NewPath("spec").Child("nodeGroups").Index(0), ng)
		if gotOK := len(errs) == 0; gotOK != wantOK {
			t.Errorf("extraSANs %q: errs = %v, want ok=%v", san, errs, wantOK)
		}
	}
}

// changing the ZooKeeper ensemble of a running cluster draws a warning.
func TestZooKeeperConnectStringWarnings(t *testing.T) {
	withZK := func(connect string) *nifiv1alpha1.NiFiCluster {
		return &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{Coordination: &nifiv1alpha1.CoordinationSpec{
			Backend: nifiv1alpha1.CoordinationBackendZooKeeper, ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: connect},
		}}}
	}
	if w := zooKeeperConnectStringWarnings(withZK("zk-a:2181"), withZK("zk-a:2181")); len(w) != 0 {
		t.Errorf("unchanged connect string warned: %v", w)
	}
	if w := zooKeeperConnectStringWarnings(withZK("zk-a:2181"), withZK("zk-b:2181")); len(w) != 1 || !strings.Contains(w[0], "separate clusters") {
		t.Errorf("changed connect string: warnings = %v, want one", w)
	}
}

// the reconcile-paused annotation no longer pauses anything; setting it draws a warning, on
// create and on update.
func TestPausedAnnotationWarning(t *testing.T) {
	v := &NiFiClusterCustomValidator{}
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}}}}
	if w := pausedAnnotationWarnings(cluster); len(w) != 0 {
		t.Errorf("no annotation: warnings = %v, want none", w)
	}
	paused := cluster.DeepCopy()
	paused.Annotations = map[string]string{"nifi.ksolves.com/reconcile-paused": "true"}
	for name, validate := range map[string]func() (admission.Warnings, error){
		"create": func() (admission.Warnings, error) { return v.ValidateCreate(context.Background(), paused) },
		"update": func() (admission.Warnings, error) { return v.ValidateUpdate(context.Background(), cluster, paused) },
	} {
		w, err := validate()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(strings.Join(w, " "), "reconcile-paused] is ignored") {
			t.Errorf("%s: warnings = %v, want the annotation flagged as ignored", name, w)
		}
	}
}
