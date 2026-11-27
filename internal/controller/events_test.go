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
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestRecordReconcileErrorEmitsWarningEvent(t *testing.T) {
	recorder := record.NewFakeRecorder(1)
	r := &NiFiClusterReconciler{Recorder: recorder}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo", Namespace: "ns"}}

	r.recordReconcileError(req, fmt.Errorf("getting NiFiCluster: boom"))

	select {
	case msg := <-recorder.Events:
		if !strings.Contains(msg, corev1.EventTypeWarning) || !strings.Contains(msg, reasonReconcileError) || !strings.Contains(msg, "boom") {
			t.Fatalf("recorded event = %q, want it to contain %q, reason %q and the error text", msg, corev1.EventTypeWarning, reasonReconcileError)
		}
	default:
		t.Fatal("recordReconcileError recorded no event")
	}
}

func TestContainerImage(t *testing.T) {
	sts := &appsv1.StatefulSet{}
	sts.Spec.Template.Spec.Containers = []corev1.Container{
		{Name: "sidecar", Image: "sidecar:1.0"},
		{Name: NifiContainerName, Image: "apache/nifi:2.12.0"},
	}
	if got := containerImage(sts); got != "apache/nifi:2.12.0" {
		t.Errorf("containerImage() = %q, want %q", got, "apache/nifi:2.12.0")
	}

	if got := containerImage(&appsv1.StatefulSet{}); got != "" {
		t.Errorf("containerImage() on a StatefulSet with no nifi container = %q, want \"\"", got)
	}
}
