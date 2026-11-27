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

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// fakeClientReconciler builds a reconciler on a fake client holding a two-group TLS cluster
// "nifi" in namespace "ns" and its CA Secret.
func fakeClientReconciler(t *testing.T, objs ...client.Object) (*NiFiClusterReconciler, *nifiv1alpha1.NiFiCluster) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, nifiv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	mapper := apimeta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		mapper.Add(gvk, apimeta.RESTScopeNamespace)
	}

	tls := &nifiv1alpha1.TLSSpec{Enabled: true}
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "nifi", Namespace: "ns", UID: "uid-nifi"},
		Spec: nifiv1alpha1.NiFiClusterSpec{
			NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "a", Replicas: 1, TLS: tls}, {Name: "b", Replicas: 1, TLS: tls}},
		},
	}
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tlsCASecretName(cluster), Namespace: "ns"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("CA-PEM")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(append(objs, cluster, ca)...).WithStatusSubresource(cluster).Build()
	return &NiFiClusterReconciler{Client: c, APIReader: c, Scheme: scheme}, cluster
}

func TestReconcileExposureReportsServiceNameConflict(t *testing.T) {
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "nifi", Namespace: "ns"}}
	r, cluster := fakeClientReconciler(t, foreign)
	if err := r.reconcileExposure(context.Background(), cluster); err == nil {
		t.Fatal("expected an error for a foreign Service named like the cluster")
	}
	c := apimeta.FindStatusCondition(cluster.Status.Conditions, serviceConditionType)
	if c == nil || c.Reason != "ServiceNameConflict" {
		t.Errorf("ServiceReady = %+v, want False/ServiceNameConflict", c)
	}
}
