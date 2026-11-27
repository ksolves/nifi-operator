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
	"reflect"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// Regression guard: these are exactly the permissions NiFi's KubernetesLeaderElection-
// Manager and KubernetesConfigMapStateProvider use. Dropping one breaks coordination at runtime
// with nothing failing at deploy time, so this test pins them.
func TestNiFiCoordinationRulesRegression(t *testing.T) {
	want := map[string][]string{
		"coordination.k8s.io/leases": {"create", "get", "update", "patch"},
		"/configmaps":                {"create", "delete", "get", "list", "patch", "update"},
	}
	got := map[string][]string{}
	for _, r := range nifiCoordinationRules() {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				got[g+"/"+res] = r.Verbs
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coordination Role rules = %v, want %v", got, want)
	}
}

func coordinationCluster() *nifiv1alpha1.NiFiCluster {
	return &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-c", Generation: 1},
		Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
			{Name: "a"},
			{Name: "b", PodOverrides: &nifiv1alpha1.PodOverridesSpec{ServiceAccountName: "team-sa"}},
			{Name: "c", PodOverrides: &nifiv1alpha1.PodOverridesSpec{ServiceAccountName: "team-sa"}},
			{Name: "d", PodOverrides: &nifiv1alpha1.PodOverridesSpec{ServiceAccountName: "c-nifi"}}, // the cluster's own
		}},
	}
}

func TestRoleBindingSubjectsNeedBothOptIns(t *testing.T) {
	names := func(rb *rbacv1.RoleBinding) []string {
		out := make([]string, 0, len(rb.Subjects))
		for _, s := range rb.Subjects {
			out = append(out, s.Name)
		}
		return out
	}
	cluster := coordinationCluster()
	if got := NodeGroupServiceAccounts(cluster); !reflect.DeepEqual(got, []string{"team-sa"}) {
		t.Errorf("node group ServiceAccounts = %v, want [team-sa] (deduplicated, cluster's own left out)", got)
	}
	for _, c := range []struct {
		admin, field bool
		want         []string
	}{
		{false, false, []string{"c-nifi"}},
		{true, false, []string{"c-nifi"}},
		{false, true, []string{"c-nifi"}},
		{true, true, []string{"c-nifi", "team-sa"}},
	} {
		cluster.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{BindPodServiceAccounts: c.field}
		r := &NiFiClusterReconciler{AllowCustomServiceAccountBinding: c.admin}
		if got := names(buildNiFiRoleBinding(cluster, r.bindsNodeGroupServiceAccounts(cluster))); !reflect.DeepEqual(got, c.want) {
			t.Errorf("admin=%v field=%v: subjects %v, want %v", c.admin, c.field, got, c.want)
		}
	}
}

// rbacConditionReconciler answers every SubjectAccessReview with allowed(user, verb, resource).
func rbacConditionReconciler(t *testing.T, cluster *nifiv1alpha1.NiFiCluster, allowed func(user, verb, resource string) bool) (*NiFiClusterReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).WithStatusSubresource(cluster).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if sar, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
				a := sar.Spec.ResourceAttributes
				sar.Status.Allowed = allowed(sar.Spec.User, a.Verb, a.Resource)
				return nil
			}
			return cl.Create(ctx, obj, opts...)
		}}).Build()
	recorder := record.NewFakeRecorder(10)
	return &NiFiClusterReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: recorder}, recorder
}

func TestCoordinationRBACCondition(t *testing.T) {
	ctx := context.Background()
	condition := func(cl *nifiv1alpha1.NiFiCluster) *metav1.Condition {
		return apimeta.FindStatusCondition(cl.Status.Conditions, coordinationRBACConditionType)
	}

	t.Run("every ServiceAccount allowed", func(t *testing.T) {
		cluster := coordinationCluster()
		r, _ := rbacConditionReconciler(t, cluster, func(string, string, string) bool { return true })
		if _, err := r.reconcileCoordinationRBACCondition(ctx, cluster); err != nil {
			t.Fatal(err)
		}
		if c := condition(cluster); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("condition = %+v, want True", c)
		}
	})

	t.Run("a node group's ServiceAccount lacks lease update", func(t *testing.T) {
		cluster := coordinationCluster()
		r, recorder := rbacConditionReconciler(t, cluster, func(user, verb, resource string) bool {
			return !strings.HasSuffix(user, ":team-sa") || verb != "update" || resource != "leases"
		})
		if _, err := r.reconcileCoordinationRBACCondition(ctx, cluster); err != nil {
			t.Fatal(err)
		}
		if c := condition(cluster); c != nil {
			t.Fatalf("condition after a single failed check = %+v, want none yet (authorizer lag)", c)
		}
		// The second check, once the recheck interval has passed.
		v, _ := r.lastRBACCheck.Load(cluster.UID)
		prev := v.(rbacCheck)
		prev.at = prev.at.Add(-rbacRecheckInterval)
		r.lastRBACCheck.Store(cluster.UID, prev)
		if _, err := r.reconcileCoordinationRBACCondition(ctx, cluster); err != nil {
			t.Fatal(err)
		}
		c := condition(cluster)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonCoordinationRBACMissing {
			t.Fatalf("condition = %+v, want False/%s", c, reasonCoordinationRBACMissing)
		}
		for _, want := range []string{`"team-sa"`, "b, c", "update leases", "create rolebinding c-nifi-team-sa --role=c-nifi --serviceaccount=ns:team-sa", "bindPodServiceAccounts"} {
			if !strings.Contains(c.Message, want) {
				t.Errorf("message %q lacks %q", c.Message, want)
			}
		}
		if strings.Contains(c.Message, `"c-nifi" (`) {
			t.Errorf("message blames the cluster's own ServiceAccount: %q", c.Message)
		}
		select {
		case e := <-recorder.Events:
			if !strings.Contains(e, "Warning "+reasonCoordinationRBACMissing) {
				t.Errorf("event = %q", e)
			}
		default:
			t.Error("no Warning event")
		}
	})

	t.Run("ZooKeeper coordination removes the condition", func(t *testing.T) {
		cluster := coordinationCluster()
		cluster.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{Backend: nifiv1alpha1.CoordinationBackendZooKeeper}
		cluster.Status.Conditions = []metav1.Condition{{Type: coordinationRBACConditionType, Status: metav1.ConditionFalse,
			Reason: reasonCoordinationRBACMissing, LastTransitionTime: metav1.Now()}}
		r, _ := rbacConditionReconciler(t, cluster, func(string, string, string) bool {
			t.Error("no SubjectAccessReview expected for ZooKeeper coordination")
			return false
		})
		if _, err := r.reconcileCoordinationRBACCondition(ctx, cluster); err != nil {
			t.Fatal(err)
		}
		if c := condition(cluster); c != nil {
			t.Errorf("condition = %+v, want removed", c)
		}
	})
}
