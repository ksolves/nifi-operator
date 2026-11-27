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
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// scaleRequest builds the admission.Request a real /scale PATCH produces: Name/Namespace name the
// NiFiNodeGroup, and Object is the generic autoscaling/v1 Scale the specpath/statuspath markers
// map the NiFiNodeGroup's own fields to and from - never a NiFiNodeGroup itself.
func scaleRequest(t *testing.T, name string, replicas int32) admission.Request {
	t.Helper()
	scale := &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: replicas}}
	raw, err := json.Marshal(scale)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Name: name, Namespace: "ns", SubResource: "scale",
		Object: runtime.RawExtension{Raw: raw},
	}}
}

func scaleValidator(t *testing.T, objs ...client.Object) *NiFiNodeGroupScaleValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &NiFiNodeGroupScaleValidator{Client: c, decoder: admission.NewDecoder(scheme)}
}

// NIFI-248 B1(b): neither the main NiFiNodeGroup webhook (never invoked for a subresource write)
// nor the CRD's own per-field CEL rule on spec.replicas (which only ever sees this one object's
// own value) can catch a /scale that pushes the whole NiFiCluster's total past MaxClusterNodes.
func TestNiFiNodeGroupScaleValidatorChecksClusterTotal(t *testing.T) {
	tests := []struct {
		name         string
		otherGroups  int32 // "main" (inline) + "workers" (sibling), each this many replicas
		targetBefore int32 // "ingest"'s (the one being scaled) currently stored replicas
		replicas     int32 // the new value this /scale request asks for
		wantAllowed  bool
	}{
		{"within the limit (1+1+1=3)", 1, 1, 1, true},
		{"an increase past the limit (1+1+2=4)", 1, 1, 2, false},
		{"a decrease, even while over the limit (2+2+1=5, down from 6)", 2, 2, 1, true},
		{"a no-op while over the limit from other groups (2+2+2=6, unchanged)", 2, 2, 2, true},
		{"an increase that makes an already over-limit total worse (2+2+3=7, up from 6)", 2, 2, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := ngCluster(false, "main")
			cluster.Spec.NodeGroups[0].Replicas = tt.otherGroups
			sibling := ngObject("workers")
			sibling.Spec.Replicas = tt.otherGroups
			target := ngObject("ingest")
			target.Spec.Replicas = tt.targetBefore

			v := scaleValidator(t, cluster, sibling, target)
			resp := v.Handle(context.Background(), scaleRequest(t, "ingest", tt.replicas))
			if resp.Allowed != tt.wantAllowed {
				t.Errorf("Handle() allowed = %v, want %v (response: %+v)", resp.Allowed, tt.wantAllowed, resp.Result)
			}
		})
	}
}

// Scaling a NiFiNodeGroup to replace its own prior contribution, not add to it, must not double
// count: 1 (main) + 2 (ingest's new value) must come out to 3, not 4, even though ingest's OLD
// value (1) is still what is stored when this request is evaluated.
func TestNiFiNodeGroupScaleValidatorSubstitutesOwnOldValue(t *testing.T) {
	cluster := ngCluster(false, "main") // 1 inline replica
	target := ngObject("ingest")        // stored at 1 replica
	v := scaleValidator(t, cluster, target)

	resp := v.Handle(context.Background(), scaleRequest(t, "ingest", 2)) // 1 + 2 = 3: within the limit
	if !resp.Allowed {
		t.Errorf("Handle() = %+v, want allowed (1 + 2 = 3, within the limit)", resp.Result)
	}
}

func TestNiFiNodeGroupScaleValidatorMissingNodeGroupOrCluster(t *testing.T) {
	v := scaleValidator(t) // nothing stored
	resp := v.Handle(context.Background(), scaleRequest(t, "ghost", 2))
	if !resp.Allowed {
		t.Errorf("Handle() for a deleted NiFiNodeGroup = %+v, want allowed (nothing left to protect)", resp.Result)
	}

	orphan := ngObject("orphan") // ClusterRef "prod" does not exist
	v = scaleValidator(t, orphan)
	resp = v.Handle(context.Background(), scaleRequest(t, "orphan", 5))
	if !resp.Allowed {
		t.Errorf("Handle() for an orphaned NiFiNodeGroup = %+v, want allowed (no cluster to sum against)", resp.Result)
	}
}
