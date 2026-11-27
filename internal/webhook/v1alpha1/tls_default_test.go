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

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func admissionContext(op admissionv1.Operation) context.Context {
	return admission.NewContextWithRequest(context.Background(),
		admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: op}})
}

// TLS is on by default for node groups created without a tls block; an explicit choice and
// existing node groups are left alone.
func TestNiFiClusterDefaultsTLSOnCreateOnly(t *testing.T) {
	newCluster := func() *nifiv1alpha1.NiFiCluster {
		return &nifiv1alpha1.NiFiCluster{Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
			{Name: "a", Replicas: 1, Storage: "1Gi"},
			{Name: "b", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: false}},
		}}}
	}
	d := &NiFiClusterCustomDefaulter{}

	c := newCluster()
	if err := d.Default(admissionContext(admissionv1.Create), c); err != nil {
		t.Fatal(err)
	}
	if tls := c.Spec.NodeGroups[0].TLS; tls == nil || !tls.Enabled {
		t.Errorf("create: nodeGroups[0].tls = %+v, want enabled", tls)
	}
	if tls := c.Spec.NodeGroups[1].TLS; tls == nil || tls.Enabled {
		t.Errorf("create: an explicit tls.enabled: false was changed to %+v", tls)
	}

	for name, ctx := range map[string]context.Context{
		"update":     admissionContext(admissionv1.Update),
		"no request": context.Background(),
	} {
		c := newCluster()
		if err := d.Default(ctx, c); err != nil {
			t.Fatal(err)
		}
		if c.Spec.NodeGroups[0].TLS != nil {
			t.Errorf("%s: nodeGroups[0].tls = %+v, want nil (an existing node group keeps TLS off)", name, c.Spec.NodeGroups[0].TLS)
		}
	}
}

func TestNiFiNodeGroupDefaultsTLSOnCreateOnly(t *testing.T) {
	o := ngObject("workers")
	if err := (&NiFiNodeGroupCustomDefaulter{}).Default(admissionContext(admissionv1.Create), o); err != nil {
		t.Fatal(err)
	}
	if o.Spec.TLS == nil || !o.Spec.TLS.Enabled {
		t.Errorf("create: tls = %+v, want enabled", o.Spec.TLS)
	}
	o = ngObject("workers")
	if err := (&NiFiNodeGroupCustomDefaulter{}).Default(admissionContext(admissionv1.Update), o); err != nil {
		t.Fatal(err)
	}
	if o.Spec.TLS != nil {
		t.Errorf("update: tls = %+v, want nil", o.Spec.TLS)
	}
}

// serviceType LoadBalancer needs TLS: rejected on create and when serviceType or TLS changes,
// never for an unrelated edit or a cluster being deleted.
func TestLoadBalancerNeedsTLS(t *testing.T) {
	cluster := func(serviceType corev1.ServiceType, tls bool) *nifiv1alpha1.NiFiCluster {
		c := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
			Spec: nifiv1alpha1.NiFiClusterSpec{ServiceType: serviceType,
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}}}}
		if tls {
			c.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
		}
		return c
	}
	v := &NiFiClusterCustomValidator{}
	ctx := context.Background()
	const msg = "serviceType LoadBalancer needs TLS"

	if _, err := v.ValidateCreate(ctx, cluster(corev1.ServiceTypeLoadBalancer, false)); err == nil ||
		!strings.Contains(err.Error(), "spec.serviceType") || !strings.Contains(err.Error(), msg) {
		t.Errorf("create LoadBalancer without TLS: err = %v, want spec.serviceType rejected", err)
	}
	if _, err := v.ValidateCreate(ctx, cluster(corev1.ServiceTypeLoadBalancer, true)); err != nil {
		t.Errorf("create LoadBalancer with TLS: %v", err)
	}
	if _, err := v.ValidateCreate(ctx, cluster(corev1.ServiceTypeClusterIP, false)); err != nil {
		t.Errorf("create ClusterIP without TLS: %v", err)
	}

	// Switching to LoadBalancer, or turning TLS off under one, is a change.
	if _, err := v.ValidateUpdate(ctx, cluster(corev1.ServiceTypeClusterIP, false), cluster(corev1.ServiceTypeLoadBalancer, false)); err == nil ||
		!strings.Contains(err.Error(), msg) {
		t.Errorf("ClusterIP to LoadBalancer without TLS: err = %v, want rejected", err)
	}
	if _, err := v.ValidateUpdate(ctx, cluster(corev1.ServiceTypeLoadBalancer, true), cluster(corev1.ServiceTypeLoadBalancer, false)); err == nil ||
		!strings.Contains(err.Error(), msg) {
		t.Errorf("TLS off under a LoadBalancer: err = %v, want rejected", err)
	}

	// A cluster stored that way may still be edited, and deleted.
	old := cluster(corev1.ServiceTypeLoadBalancer, false)
	updated := old.DeepCopy()
	updated.Spec.Labels = map[string]string{"team": "a"}
	if _, err := v.ValidateUpdate(ctx, old, updated); err != nil {
		t.Errorf("unrelated edit of a stored LoadBalancer cluster without TLS: %v", err)
	}
	deleting := cluster(corev1.ServiceTypeLoadBalancer, false)
	deleting.DeletionTimestamp = &metav1.Time{}
	deleting.Finalizers = []string{"x"}
	if _, err := v.ValidateUpdate(ctx, cluster(corev1.ServiceTypeClusterIP, false), deleting); err != nil {
		t.Errorf("cluster being deleted: %v", err)
	}
}

// The first node group of a LoadBalancer cluster, as a NiFiNodeGroup object, needs TLS too.
func TestNiFiNodeGroupLoadBalancerNeedsTLS(t *testing.T) {
	lb := ngCluster(false)
	lb.Spec.ServiceType = corev1.ServiceTypeLoadBalancer
	for _, tc := range []struct {
		name    string
		tls     *nifiv1alpha1.TLSSpec
		wantErr bool
	}{
		{"without TLS", nil, true},
		{"TLS off", &nifiv1alpha1.TLSSpec{Enabled: false}, true},
		{"TLS on", &nifiv1alpha1.TLSSpec{Enabled: true}, false},
	} {
		ng := ngObject("workers")
		ng.Spec.TLS = tc.tls
		_, err := nodeGroupValidator(t, lb.DeepCopy()).validate(context.Background(), nil, ng)
		if gotErr := err != nil && strings.Contains(err.Error(), "spec.tls.enabled") &&
			strings.Contains(err.Error(), "serviceType LoadBalancer needs TLS"); gotErr != tc.wantErr {
			t.Errorf("%s: err = %v, want rejected = %v", tc.name, err, tc.wantErr)
		}
	}
}
