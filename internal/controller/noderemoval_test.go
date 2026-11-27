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
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

type fakeNiFiAPI struct {
	nodes      []nifiNode
	deleted    []string
	listErr    error
	statusSets []string // "<nodeID>:<status>", in call order
	statusErr  error
}

func (f *fakeNiFiAPI) ListNodes(context.Context) ([]nifiNode, error) { return f.nodes, f.listErr }
func (f *fakeNiFiAPI) DeleteNode(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeNiFiAPI) SetNodeStatus(_ context.Context, nodeID, status string) error {
	if f.statusErr != nil {
		return f.statusErr
	}
	f.statusSets = append(f.statusSets, nodeID+":"+status)
	for i := range f.nodes {
		if f.nodes[i].NodeID == nodeID {
			f.nodes[i].Status = status
		}
	}
	return nil
}

func TestRemoveDepartedNodesOnlyDeletesGoneAndDisconnected(t *testing.T) {
	api := &fakeNiFiAPI{nodes: []nifiNode{
		{NodeID: "1", Address: "keep-0", Status: "CONNECTED"},
		{NodeID: "2", Address: "gone-1", Status: "DISCONNECTED"},
		{NodeID: "3", Address: "gone-2", Status: "OFFLOADED"},
		{NodeID: "4", Address: "gone-3", Status: "CONNECTED"},                   // still connected: never deleted
		{NodeID: "5", Address: "keep-1", Status: "DISCONNECTED"},                // still in spec: restarting, not departed
		{NodeID: "6", Address: "gone-2-still-terminating", Status: "OFFLOADED"}, // pod still shutting down
	}}
	gone := func(addr string) bool { return addr != "gone-2-still-terminating" }
	removed, waiting, err := removeDepartedNodes(context.Background(), api, map[string]bool{"keep-0": true, "keep-1": true}, gone)
	if err != nil {
		t.Fatal(err)
	}
	if waiting != 2 {
		t.Errorf("waiting = %d, want 2 (gone-3 still connected, gone-2's pod still terminating)", waiting)
	}
	if !reflect.DeepEqual(api.deleted, []string{"2", "3"}) || !reflect.DeepEqual(removed, []string{"gone-1", "gone-2"}) {
		t.Errorf("deleted %v (%v), want ids [2 3]", api.deleted, removed)
	}
}

// A killed pod rejoins under a new node ID while NiFi can still list its old identity at the same
// address (seen on OKD in an E2E run); the stale one is removed once the new one is connected.
func TestRemoveDepartedNodesDeletesStaleIdentityOfCurrentPod(t *testing.T) {
	api := &fakeNiFiAPI{nodes: []nifiNode{
		{NodeID: "old", Address: "keep-1", Status: "DISCONNECTED"},
		{NodeID: "new", Address: "keep-1", Status: "CONNECTED"},
		{NodeID: "rejoining", Address: "keep-2", Status: "DISCONNECTED"}, // only identity: left alone
		{NodeID: "old-c", Address: "keep-3", Status: "DISCONNECTED"},     // new identity still connecting
		{NodeID: "new-c", Address: "keep-3", Status: "CONNECTING"},
	}}
	current := map[string]bool{"keep-1": true, "keep-2": true, "keep-3": true}
	removed, waiting, err := removeDepartedNodes(context.Background(), api, current, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if waiting != 0 || !reflect.DeepEqual(api.deleted, []string{"old"}) || len(removed) != 1 {
		t.Errorf("deleted %v (removed %v, waiting %d), want only [old]", api.deleted, removed, waiting)
	}
}

func TestCurrentPodFQDNs(t *testing.T) {
	cluster := exposureCluster(false) // groups a (2 replicas) and b (1)
	got := currentPodFQDNs(cluster)
	if len(got) != 3 || !got[PodFQDN(testClusterName+"-a-1", testClusterName, "a", testClusterNamespace)] ||
		got[PodFQDN(testClusterName+"-a-2", testClusterName, "a", testClusterNamespace)] {
		t.Errorf("currentPodFQDNs = %v", got)
	}
}

func TestReconcileNodeRemoval(t *testing.T) {
	r, cluster := fakeClientReconciler(t)
	cluster.Status = nifiv1alpha1.NiFiClusterStatus{Replicas: 2, ReadyReplicas: 2, NodeRemovalPending: true}
	ghost := PodFQDN("nifi-a-1", "nifi", "a", "ns")
	api := &fakeNiFiAPI{nodes: []nifiNode{{NodeID: "g", Address: ghost, Status: "DISCONNECTED"}}}
	r.NewNiFiAPI = func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return api, nil }

	// Not every remaining node ready yet: wait.
	cluster.Status.ReadyReplicas = 1
	if res, err := r.reconcileNodeRemoval(context.Background(), cluster); err != nil || res.RequeueAfter == 0 || len(api.deleted) != 0 {
		t.Fatalf("expected a requeue without deletions while not ready (res=%v err=%v deleted=%v)", res, err, api.deleted)
	}

	// The departed node still looks connected (it was the coordinator): keep waiting.
	api.nodes[0].Status = "CONNECTED"
	cluster.Status.ReadyReplicas = 2
	if res, err := r.reconcileNodeRemoval(context.Background(), cluster); err != nil || res.RequeueAfter == 0 || !cluster.Status.NodeRemovalPending {
		t.Fatalf("expected to keep waiting for a departed node still listed as connected (res=%v err=%v)", res, err)
	}

	api.nodes[0].Status = "DISCONNECTED"
	if _, err := r.reconcileNodeRemoval(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.deleted, []string{"g"}) || cluster.Status.NodeRemovalPending {
		t.Errorf("deleted=%v pending=%v, want the ghost removed and pending cleared", api.deleted, cluster.Status.NodeRemovalPending)
	}

	// No credentials on a TLS cluster: reported, not retried in a loop.
	cluster.Status.NodeRemovalPending = true
	r.NewNiFiAPI = func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return nil, errNoNiFiCredentials }
	if _, err := r.reconcileNodeRemoval(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	if c := apimeta.FindStatusCondition(cluster.Status.Conditions, membershipConditionType); c == nil || c.Reason != "NoCredentials" || c.Status != metav1.ConditionFalse {
		t.Errorf("MembershipUpToDate = %+v, want False/NoCredentials", c)
	}

	// An API failure is reported and retried.
	r.NewNiFiAPI = func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) {
		return &fakeNiFiAPI{listErr: errors.New("boom")}, nil
	}
	if res, err := r.reconcileNodeRemoval(context.Background(), cluster); err != nil || res.RequeueAfter == 0 {
		t.Errorf("expected a requeue on an API error (res=%v err=%v)", res, err)
	}
}

// restNiFiAPI against a stand-in NiFi REST API: logs in, lists nodes, deletes one, and surfaces
// HTTP errors.
func TestRestNiFiAPI(t *testing.T) {
	var deletedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/nifi-api/access/token":
			_ = req.ParseForm()
			if req.Form.Get("username") != "admin" || req.Form.Get("password") != "secret-password" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("TOKEN\n"))
		case req.Header.Get("Authorization") != "Bearer TOKEN":
			w.WriteHeader(http.StatusUnauthorized)
		case req.Method == http.MethodGet && req.URL.Path == "/nifi-api/controller/cluster":
			_, _ = w.Write([]byte(`{"cluster":{"nodes":[{"nodeId":"n1","address":"a","status":"OFFLOADED"}]}}`))
		case req.Method == http.MethodDelete:
			deletedPath = req.URL.Path
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &restNiFiAPI{http: srv.Client(), base: srv.URL + "/nifi-api"}
	if err := c.login(context.Background(), "admin", "wrong"); err == nil {
		t.Error("login with a wrong password succeeded")
	}
	if err := c.login(context.Background(), "admin", "secret-password"); err != nil || c.token != "TOKEN" {
		t.Fatalf("login: token=%q err=%v", c.token, err)
	}
	nodes, err := c.ListNodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].NodeID != "n1" || nodes[0].Status != "OFFLOADED" {
		t.Fatalf("ListNodes = %+v, %v", nodes, err)
	}
	if err := c.DeleteNode(context.Background(), "n1"); err != nil || deletedPath != "/nifi-api/controller/cluster/nodes/n1" {
		t.Errorf("DeleteNode: path=%q err=%v", deletedPath, err)
	}
}

// One node down must not block removal: the next current pod is used.
func TestFirstAnsweringFallsBackToNextNode(t *testing.T) {
	var tried []string
	api, err := firstAnswering([]string{"node-0", "node-1", "node-2"}, func(host string) (nifiAPI, error) {
		tried = append(tried, host)
		if host == "node-0" {
			return nil, errors.New("connection refused")
		}
		return &fakeNiFiAPI{}, nil
	})
	if err != nil || api == nil || !reflect.DeepEqual(tried, []string{"node-0", "node-1"}) {
		t.Errorf("tried %v, err %v; want fallback to node-1", tried, err)
	}

	_, err = firstAnswering([]string{"a", "b"}, func(string) (nifiAPI, error) { return nil, errors.New("down") })
	if err == nil || !strings.Contains(err.Error(), "a: down") || !strings.Contains(err.Error(), "b: down") {
		t.Errorf("err = %v, want both nodes' errors", err)
	}
}
