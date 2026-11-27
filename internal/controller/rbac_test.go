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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func rbacTestReconciler(t *testing.T, scc string, objs ...client.Object) (*NiFiClusterReconciler, *nifiv1alpha1.NiFiCluster) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "rbac", Namespace: "ns", UID: "uid-1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, cluster)...).Build()
	return &NiFiClusterReconciler{Client: c, APIReader: c, Scheme: scheme, NiFiSCC: scc}, cluster
}

func getRoleBinding(t *testing.T, r *NiFiClusterReconciler, name string) (*rbacv1.RoleBinding, error) {
	t.Helper()
	rb := &rbacv1.RoleBinding{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, rb)
	return rb, err
}

func TestReconcileNiFiRBACBindsSCCToOperatorServiceAccount(t *testing.T) {
	r, cluster := rbacTestReconciler(t, "anyuid")
	if err := r.reconcileNiFiRBAC(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	rb, err := getRoleBinding(t, r, nifiSCCRoleBindingName(cluster))
	if err != nil {
		t.Fatalf("SCC RoleBinding not created: %v", err)
	}
	if want := (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:openshift:scc:anyuid"}); rb.RoleRef != want {
		t.Errorf("roleRef = %+v, want %+v", rb.RoleRef, want)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != nifiServiceAccountName(cluster) {
		t.Errorf("subjects = %+v, want the operator-managed ServiceAccount", rb.Subjects)
	}
	if !metav1.IsControlledBy(rb, cluster) {
		t.Error("SCC RoleBinding not controlled by the NiFiCluster")
	}
}

// A user-supplied ServiceAccount must never be bound to an SCC by the operator (confused deputy):
// any existing operator-made SCC binding is removed, the coordination Role is still bound.
func TestReconcileNiFiRBACNeverBindsSCCToUserServiceAccount(t *testing.T) {
	r, cluster := rbacTestReconciler(t, "anyuid")
	if err := r.reconcileNiFiRBAC(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}

	cluster.Spec.ServiceAccountName = "someone-elses-sa"
	if err := r.reconcileNiFiRBAC(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	if _, err := getRoleBinding(t, r, nifiSCCRoleBindingName(cluster)); !apierrors.IsNotFound(err) {
		t.Errorf("SCC RoleBinding still present for a user-supplied ServiceAccount (err=%v)", err)
	}
	rb, err := getRoleBinding(t, r, nifiRoleName(cluster))
	if err != nil {
		t.Fatal(err)
	}
	if rb.Subjects[0].Name != "someone-elses-sa" {
		t.Errorf("coordination RoleBinding subject = %q, want the user-supplied ServiceAccount", rb.Subjects[0].Name)
	}
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: nifiServiceAccountName(cluster)}, &corev1.ServiceAccount{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("operator-managed ServiceAccount not removed after switching to a user-supplied one (err=%v)", err)
	}
}

// roleRef is immutable, so a changed SCC must replace the binding rather than update it.
func TestReconcileRoleBindingReplacesBindingWhenRoleRefChanges(t *testing.T) {
	r, cluster := rbacTestReconciler(t, "anyuid")
	if err := r.reconcileNiFiRBAC(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	r.NiFiSCC = "nonroot-v2"
	if err := r.reconcileNiFiRBAC(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	rb, err := getRoleBinding(t, r, nifiSCCRoleBindingName(cluster))
	if err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Name != "system:openshift:scc:nonroot-v2" {
		t.Errorf("roleRef = %q after SCC change, want system:openshift:scc:nonroot-v2", rb.RoleRef.Name)
	}
}

func TestReconcileRoleBindingRefusesForeignBindingWithDifferentRoleRef(t *testing.T) {
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-nifi", Namespace: "ns"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"},
	}
	r, cluster := rbacTestReconciler(t, "", foreign)
	err := r.reconcileNiFiRBAC(context.Background(), cluster)
	if err == nil || !errors.Is(err, errNotOwned) {
		t.Fatalf("expected refusal to replace a foreign RoleBinding, got %v", err)
	}
	rb, err := getRoleBinding(t, r, "rbac-nifi")
	if err != nil || rb.RoleRef.Name != "admin" {
		t.Errorf("foreign RoleBinding was modified or deleted (err=%v, roleRef=%+v)", err, rb.RoleRef)
	}
}

func TestDeleteOwnedLeavesForeignObjects(t *testing.T) {
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "rbac-nifi", Namespace: "ns"}}
	r, cluster := rbacTestReconciler(t, "", foreign)
	if err := r.deleteOwned(context.Background(), cluster, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-nifi", Namespace: "ns"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.ServiceAccount{}); err != nil {
		t.Errorf("deleteOwned removed an object the NiFiCluster does not control: %v", err)
	}
}

// A same-named object the NiFiCluster does not control is never adopted or mutated - even when
// its roleRef already matches, and even when it lacks the managed-by label the cache filters on.
func TestReconcileNiFiRBACNeverAdoptsForeignObjects(t *testing.T) {
	foreignRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-nifi", Namespace: "ns",
			Labels: map[string]string{labelAppManagedBy: managedByValue}},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"*"}}},
	}
	r, cluster := rbacTestReconciler(t, "", foreignRole)
	if err := r.reconcileNiFiRBAC(context.Background(), cluster); !errors.Is(err, errNotOwned) {
		t.Fatalf("expected errNotOwned for a foreign Role, got %v", err)
	}
	got := &rbacv1.Role{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreignRole), got); err != nil {
		t.Fatal(err)
	}
	if metav1.IsControlledBy(got, cluster) || got.Rules[0].Resources[0] != "secrets" {
		t.Errorf("foreign Role was adopted or rewritten: %+v", got)
	}

	foreignBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-nifi", Namespace: "ns"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "rbac-nifi"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "attacker", Namespace: "ns"}},
	}
	r, cluster = rbacTestReconciler(t, "", foreignBinding)
	if err := r.reconcileNiFiRBAC(context.Background(), cluster); !errors.Is(err, errNotOwned) {
		t.Fatalf("expected errNotOwned for a foreign RoleBinding with a matching roleRef, got %v", err)
	}
}

// A same-named object still controlled by a deleted NiFiCluster of the same name is reported as
// waiting for garbage collection, not adopted and not reported as foreign.
func TestGetOwnedReportsStaleOwnerFromPreviousCluster(t *testing.T) {
	stale := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name: "rbac-nifi", Namespace: "ns",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: nifiv1alpha1.GroupVersion.String(), Kind: "NiFiCluster", Name: "rbac",
			UID: "uid-of-deleted-cluster", Controller: ptrTo(true),
		}},
	}}
	r, cluster := rbacTestReconciler(t, "", stale)
	err := r.reconcileNiFiRBAC(context.Background(), cluster)
	if !errors.Is(err, errStaleOwner) {
		t.Fatalf("expected errStaleOwner, got %v", err)
	}
}

func ptrTo[T any](v T) *T { return &v }
