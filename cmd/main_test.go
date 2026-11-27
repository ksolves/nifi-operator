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

package main

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// The certificate watchers must run on standby replicas too: they serve the webhook.
func TestEveryReplicaRunsWithoutLeadership(t *testing.T) {
	var cw *certwatcher.CertWatcher // the watcher type main wraps
	var r manager.Runnable = everyReplica{manager.RunnableFunc(func(context.Context) error { return nil })}
	ler, ok := r.(manager.LeaderElectionRunnable)
	if !ok || ler.NeedLeaderElection() {
		t.Fatal("everyReplica must report NeedLeaderElection() == false")
	}
	// The bare watcher does not opt out, which is why main wraps it.
	if _, optsOut := any(cw).(manager.LeaderElectionRunnable); optsOut {
		t.Log("certwatcher.CertWatcher now declares NeedLeaderElection itself; the wrapper is still harmless")
	}
}

func TestNiFiPodFlagsRunAsUser(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  *int64
		bad   bool
	}{
		{value: "1000", want: ptr.To(int64(1000))},
		{value: "", want: nil},
		{value: "0", bad: true},
		{value: "nifi", bad: true},
		{value: "-1", bad: true},
	} {
		_, got, err := nifiPodFlags("0", tc.value, "", "RuntimeDefault")
		if tc.bad {
			if err == nil {
				t.Errorf("--nifi-run-as-user=%q accepted, want an error", tc.value)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("--nifi-run-as-user=%q = %v, %v; want %v", tc.value, got, err, tc.want)
		}
	}
}

// The ready check lists NiFiClusters cluster-wide, which the operator's ClusterRole allows.
func TestReadyCheckListsClusterWide(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var listed []string
	reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listed = append(listed, (&client.ListOptions{}).ApplyOptions(opts).Namespace)
			return c.List(ctx, list, opts...)
		},
	}).Build()
	req, _ := http.NewRequest(http.MethodGet, "/readyz", nil)
	if err := readyCheck(reader)(req); err != nil {
		t.Errorf("ready check failed: %v", err)
	}
	if !reflect.DeepEqual(listed, []string{""}) {
		t.Errorf("listed in %q, want one cluster-wide list", listed)
	}
}
