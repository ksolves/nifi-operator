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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func TestEffectivePVCRetention(t *testing.T) {
	retain, del := nifiv1alpha1.PVCRetain, nifiv1alpha1.PVCDelete
	policy := func(d, s nifiv1alpha1.PVCRetentionType) *nifiv1alpha1.PVCRetentionPolicy {
		return &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: d, WhenScaled: s}
	}
	cases := []struct {
		name           string
		cluster, group *nifiv1alpha1.PVCRetentionPolicy
		want           nifiv1alpha1.PVCRetentionPolicy
	}{
		{"nothing set keeps everything", nil, nil, nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: retain, WhenScaled: retain}},
		{"cluster default applies", policy(del, del), nil, nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: del, WhenScaled: del}},
		{"group overrides the cluster", policy(del, del), policy(retain, retain), nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: retain, WhenScaled: retain}},
		{"group overrides one field, inherits the other", policy(del, retain), policy("", del), nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: del, WhenScaled: del}},
		{"group only", nil, policy(del, ""), nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: del, WhenScaled: retain}},
		{"empty structs mean Retain", policy("", ""), policy("", ""), nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: retain, WhenScaled: retain}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{PersistentVolumeClaimRetentionPolicy: tc.cluster}}
			group := &nifiv1alpha1.NodeGroup{Name: "g", PersistentVolumeClaimRetentionPolicy: tc.group}
			if got := EffectivePVCRetention(cluster, group); got != tc.want {
				t.Fatalf("EffectivePVCRetention = %+v, want %+v", got, tc.want)
			}
			sts := statefulSetPVCRetention(cluster, group)
			wantSTS := func(p nifiv1alpha1.PVCRetentionType) appsv1.PersistentVolumeClaimRetentionPolicyType {
				return appsv1.PersistentVolumeClaimRetentionPolicyType(p)
			}
			if sts.WhenDeleted != wantSTS(tc.want.WhenDeleted) || sts.WhenScaled != wantSTS(tc.want.WhenScaled) {
				t.Fatalf("StatefulSet policy = %+v, want %+v", *sts, tc.want)
			}
		})
	}
}

// the key and component state are kept while any of the cluster's PVCs will outlive it,
// whatever the spec says; a PVC Kubernetes will garbage-collect with its StatefulSet or Pod does
// not count.
func TestKeepsDataOnDelete(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ns"},
		Spec:       nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default"}}},
	}
	labelled := map[string]string{labelAppInstance: "prod", labelAppManagedBy: managedByValue}
	stsOwner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "prod-default", UID: "u1"}
	podOwner := metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: "prod-default-2", UID: "u2"}
	pvc := func(name string, labels map[string]string, owners ...metav1.OwnerReference) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", Labels: labels, OwnerReferences: owners,
		}}
	}
	cases := []struct {
		name string
		pvcs []*corev1.PersistentVolumeClaim
		want bool
	}{
		{"no PVCs", nil, false},
		{"a PVC without owner stays", []*corev1.PersistentVolumeClaim{pvc("data-prod-default-0", labelled)}, true},
		{"PVCs deleted with their StatefulSet or Pod", []*corev1.PersistentVolumeClaim{
			pvc("data-prod-default-0", labelled, stsOwner), pvc("data-prod-default-2", labelled, podOwner),
		}, false},
		{"one PVC stays among deleted ones", []*corev1.PersistentVolumeClaim{
			pvc("data-prod-default-0", labelled, stsOwner), pvc("data-prod-removed-0", labelled),
		}, true},
		{"unlabelled claim of a node group, by name", []*corev1.PersistentVolumeClaim{pvc("content-repository-prod-default-12", nil)}, true},
		// A name prefix cannot tell "prod" from "prod-2": unlabelled claims count only by the exact
		// names of the current node groups.
		{"unlabelled claims of cluster prod-2 or of a removed group", []*corev1.PersistentVolumeClaim{
			pvc("data-prod-2-default-0", nil), pvc("data-prod-removed-0", nil),
		}, false},
		{"other clusters' PVCs", []*corev1.PersistentVolumeClaim{
			pvc("data-prodx-default-0", nil), pvc("data-prod-default-x", nil),
			pvc("data-other-0", map[string]string{labelAppInstance: "other", labelAppManagedBy: managedByValue}),
			pvc("data-mine-0", map[string]string{labelAppInstance: "prod"}),
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			b := fake.NewClientBuilder().WithScheme(scheme)
			for _, p := range tc.pvcs {
				b = b.WithObjects(p)
			}
			c := b.Build()
			r := &NiFiClusterReconciler{Client: c, APIReader: c}
			got, err := r.keepsDataOnDelete(context.Background(), cluster)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("keepsDataOnDelete = %v, want %v", got, tc.want)
			}
		})
	}
}

// A changed retention policy alone must reach an existing StatefulSet.
func TestStatefulSetUpToDateSeesRetentionPolicy(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	group := &nifiv1alpha1.NodeGroup{Name: "g"}
	existing := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{PersistentVolumeClaimRetentionPolicy: statefulSetPVCRetention(cluster, group)}}
	desired := existing.DeepCopy()
	if !statefulSetUpToDate(existing, desired) {
		t.Fatal("identical StatefulSets reported as drifted")
	}
	group.PersistentVolumeClaimRetentionPolicy = &nifiv1alpha1.PVCRetentionPolicy{WhenScaled: nifiv1alpha1.PVCDelete}
	desired.Spec.PersistentVolumeClaimRetentionPolicy = statefulSetPVCRetention(cluster, group)
	if statefulSetUpToDate(existing, desired) {
		t.Fatal("a changed PVC retention policy was not seen as drift")
	}
}

// A retained key is adopted only for the cluster's own PVCs, matched by exact claim name: cluster
// "x" with node group "foo-default" owns "data-x-foo-default-0", which must not count for cluster
// "foo" with node group "default" (suffix "-foo-default-0").
func TestRetainedSensitivePropsSecretMatchesExactPVCNames(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "ns"},
		Spec:       nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default"}}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "foo-sensitive-props", Namespace: "ns", Annotations: map[string]string{retainedForAnnotation: "foo"},
	}}
	pvc := func(name string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	}
	for _, tc := range []struct {
		pvc  string
		want bool
	}{
		{"data-x-foo-default-0", false},
		{"data-foo-2-default-0", false},
		{"data-foo-default-x", false},
		{"data-foo-default-1", true},
		{"data-foo-default-0", true},
		{"logs-foo-default-0", true},
		{"content-repository-foo-default-0", true},
	} {
		r := &NiFiClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc(tc.pvc)).Build()}
		got, err := r.retainedSensitivePropsSecret(context.Background(), cluster, secret)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("PVC %s: adopted = %v, want %v", tc.pvc, got, tc.want)
		}
	}
}

func TestComponentIDPattern(t *testing.T) {
	for id, want := range map[string]bool{
		"f5fc97b3-01a0-1000-ffff-ffff81295497":                  true,
		"x-nifi-component-f5fc97b3-01a0-1000-ffff-ffff81295497": false,
		"1": false,
	} {
		if got := componentIDPattern.MatchString(id); got != want {
			t.Errorf("componentIDPattern(%q) = %v, want %v", id, got, want)
		}
	}
}

// a key an earlier delete pass released is deleted once nothing is left to decrypt; a
// key another cluster controls, or one released for another name, is left alone.
func TestDeleteReleasedSensitivePropsSecret(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ns", UID: "c1"}}
	secret := func(annotation string, controlled bool) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "prod-sensitive-props", Namespace: "ns", UID: "s1", Annotations: map[string]string{retainedForAnnotation: annotation},
		}}
		if controlled {
			s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "nifi.ksolves.com/v1alpha1", Kind: "NiFiCluster", Name: "prod", UID: "c2", Controller: ptr.To(true)}}
		}
		return s
	}
	for _, tc := range []struct {
		name   string
		secret *corev1.Secret
		gone   bool
	}{
		{"released for this cluster", secret("prod", false), true},
		{"released for another name", secret("other", false), false},
		{"controlled by a cluster", secret("prod", true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.secret).Build()
			r := &NiFiClusterReconciler{Client: c, APIReader: c}
			if err := r.deleteReleasedSensitivePropsSecret(context.Background(), cluster); err != nil {
				t.Fatal(err)
			}
			err := c.Get(context.Background(), client.ObjectKeyFromObject(tc.secret), &corev1.Secret{})
			if gone := apierrors.IsNotFound(err); gone != tc.gone {
				t.Fatalf("deleted = %v, want %v (err %v)", gone, tc.gone, err)
			}
		})
	}
}
