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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// controllerClusterFixture is a trimmed GET /nifi-api/controller/cluster response (NiFi 2.x).
const controllerClusterFixture = `{"cluster":{"nodes":[
 {"nodeId":"a1","address":"st-a-1.st-a-headless.ns.svc.cluster.local","apiPort":8443,"status":"CONNECTED",
  "heartbeat":"10/07/2026 10:00:05 UTC","roles":["Primary Node"],"activeThreadCount":3,"queued":"12 / 4 KB"},
 {"nodeId":"a0","address":"st-a-0.st-a-headless.ns.svc.cluster.local","apiPort":8443,"status":"CONNECTED",
  "heartbeat":"10/07/2026 10:00:04 UTC","roles":["Cluster Coordinator"],"activeThreadCount":1,"queued":"0 / 0 bytes"},
 {"nodeId":"b0","address":"st-b-0.st-b-headless.ns.svc.cluster.local","apiPort":8443,"status":"DISCONNECTED",
  "heartbeat":"10/07/2026 09:58:00 UTC","roles":[],"activeThreadCount":0,"queued":"0 / 0 bytes"}]}}`

func statusCluster() *nifiv1alpha1.NiFiCluster {
	return &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "st", Namespace: "ns", UID: "st-uid", Generation: 4},
		Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
			{Name: "a", Replicas: 2}, {Name: "b", Replicas: 1},
		}},
	}
}

func fixtureNodes(t *testing.T) []nifiNode {
	t.Helper()
	var resp struct {
		Cluster struct {
			Nodes []nifiNode `json:"nodes"`
		} `json:"cluster"`
	}
	if err := json.Unmarshal([]byte(controllerClusterFixture), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Cluster.Nodes
}

// statusFakeAPI serves a fixed node list and version.
type statusFakeAPI struct {
	nodes   []nifiNode
	err     error
	version string
	calls   int
}

func (f *statusFakeAPI) ListNodes(context.Context) ([]nifiNode, error) {
	f.calls++
	return f.nodes, f.err
}
func (f *statusFakeAPI) DeleteNode(context.Context, string) error            { return nil }
func (f *statusFakeAPI) SetNodeStatus(context.Context, string, string) error { return nil }
func (f *statusFakeAPI) Version(context.Context) (string, error)             { return f.version, nil }

func reconcilerWith(api *statusFakeAPI, apiErr error) *NiFiClusterReconciler {
	return &NiFiClusterReconciler{
		NiFiStatusInterval: time.Hour,
		NewNiFiAPI: func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) {
			if apiErr != nil {
				return nil, apiErr
			}
			return api, nil
		},
	}
}

func TestNiFiStatusFromFixture(t *testing.T) {
	cluster := statusCluster()
	api := &statusFakeAPI{nodes: fixtureNodes(t), version: "2.12.0"}
	poll := reconcilerWith(api, nil).pollNiFiStatus(context.Background(), cluster, 2)
	if poll == nil {
		t.Fatal("no poll")
	}
	applyNiFiPoll(&cluster.Status, poll, cluster.Generation)

	st := cluster.Status
	if st.NiFiVersion != "2.12.0" {
		t.Errorf("version = %q", st.NiFiVersion)
	}
	if st.ClusterCoordinator != "st-a-0.st-a-headless.ns.svc.cluster.local" || st.PrimaryNode != "st-a-1.st-a-headless.ns.svc.cluster.local" {
		t.Errorf("coordinator %q primary %q", st.ClusterCoordinator, st.PrimaryNode)
	}
	if len(st.Nodes) != 3 || st.Nodes[0].Address != "st-a-0.st-a-headless.ns.svc.cluster.local" {
		t.Fatalf("nodes not sorted by address: %+v", st.Nodes)
	}
	if st.Nodes[2].NodeGroup != "b" || st.Nodes[2].State != "DISCONNECTED" || st.Nodes[1].ActiveThreads != 3 {
		t.Errorf("node mapping wrong: %+v", st.Nodes)
	}
	c := apimeta.FindStatusCondition(st.Conditions, clusterHealthyConditionType)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NodesNotConnected" ||
		!strings.Contains(c.Message, "st-b-0.st-b-headless.ns.svc.cluster.local (DISCONNECTED)") || c.ObservedGeneration != 4 {
		t.Errorf("ClusterHealthy = %+v", c)
	}
}

func TestNiFiStatusHealthyAndMissingNodes(t *testing.T) {
	cluster := statusCluster()
	nodes := fixtureNodes(t)
	nodes[2].Status = "CONNECTED"
	got := clusterHealth(currentPodFQDNs(cluster), nodeStatuses(cluster, nodes))
	if got.Status != metav1.ConditionTrue || got.Reason != "AllNodesConnected" {
		t.Errorf("all connected: %+v", got)
	}
	got = clusterHealth(currentPodFQDNs(cluster), nodeStatuses(cluster, nodes[:2]))
	if got.Status != metav1.ConditionFalse || !strings.Contains(got.Message, "not in the NiFi cluster") {
		t.Errorf("missing node: %+v", got)
	}
}

// A restarted node's stale identity next to its new one: both kept (status is keyed by nodeId),
// and the address counts as connected.
func TestNiFiStatusDuplicateAddress(t *testing.T) {
	cluster := statusCluster()
	nodes := fixtureNodes(t)
	nodes[2].Status = "CONNECTED"
	stale := nifiNode{NodeID: "a1-old", Address: nodes[0].Address, Status: "DISCONNECTED"}
	statuses := nodeStatuses(cluster, append(nodes, stale))
	if len(statuses) != 4 {
		t.Fatalf("nodes = %+v", statuses)
	}
	ids := map[string]bool{}
	for _, n := range statuses {
		if ids[n.NodeID] {
			t.Errorf("duplicate nodeId %q", n.NodeID)
		}
		ids[n.NodeID] = true
	}
	if got := clusterHealth(currentPodFQDNs(cluster), statuses); got.Status != metav1.ConditionTrue {
		t.Errorf("stale identity beside a connected one made the cluster unhealthy: %+v", got)
	}
}

func TestNiFiStatusErrorsBecomeConditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		apiErr error
		reason string
	}{
		{"wrong credentials", &nifiHTTPError{Method: "POST", Path: "/access/token", Code: http.StatusUnauthorized}, "AuthFailed"},
		// What NiFi 2.12 actually answers to a wrong single-user password.
		{"wrong password, NiFi 2.x", &nifiHTTPError{Method: "POST", Path: "/access/token", Code: http.StatusBadRequest}, "AuthFailed"},
		{"bad request elsewhere", &nifiHTTPError{Method: "GET", Path: "/controller/cluster", Code: http.StatusBadRequest}, "NiFiAPIError"},
		{"forbidden", &nifiHTTPError{Method: "GET", Path: "/controller/cluster", Code: http.StatusForbidden}, "AuthFailed"},
		{"no login Secret", errNoNiFiCredentials, "AuthFailed"},
		{"server error", &nifiHTTPError{Method: "GET", Path: "/controller/cluster", Code: http.StatusInternalServerError}, "NiFiAPIError"},
		{"nothing answers", errors.New("dial tcp: connection refused"), "NiFiUnreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := statusCluster()
			cluster.Status.Nodes = []nifiv1alpha1.NiFiNodeStatus{{Address: "kept", State: "CONNECTED"}}
			poll := reconcilerWith(nil, tc.apiErr).pollNiFiStatus(context.Background(), cluster, 1)
			applyNiFiPoll(&cluster.Status, poll, 1)
			c := apimeta.FindStatusCondition(cluster.Status.Conditions, clusterHealthyConditionType)
			if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != tc.reason {
				t.Errorf("condition = %+v, want Unknown/%s", c, tc.reason)
			}
			if len(cluster.Status.Nodes) != 1 {
				t.Error("a failed poll blanked the last known nodes")
			}
		})
	}
}

func TestNiFiStatusNotReadySkipsTheAPI(t *testing.T) {
	api := &statusFakeAPI{}
	poll := reconcilerWith(api, nil).pollNiFiStatus(context.Background(), statusCluster(), 0)
	if api.calls != 0 || poll.condition.Reason != "NotReady" {
		t.Errorf("calls=%d condition=%+v", api.calls, poll.condition)
	}
}

func TestNiFiStatusThrottle(t *testing.T) {
	api := &statusFakeAPI{nodes: fixtureNodes(t)}
	r := reconcilerWith(api, nil)
	cluster := statusCluster()
	if r.pollNiFiStatus(context.Background(), cluster, 1) == nil {
		t.Fatal("first poll skipped")
	}
	if r.pollNiFiStatus(context.Background(), cluster, 1) != nil || api.calls != 1 {
		t.Errorf("second poll within the interval ran (calls=%d)", api.calls)
	}
}

func TestNiFiStatusNoChurnOnHeartbeatsAlone(t *testing.T) {
	cluster := statusCluster()
	nodes := fixtureNodes(t)
	t0 := time.Now()
	applyNiFiPoll(&cluster.Status, &nifiPoll{at: t0, nodes: nodeStatuses(cluster, nodes),
		condition: clusterHealth(currentPodFQDNs(cluster), nodeStatuses(cluster, nodes))}, 1)
	before := cluster.Status.DeepCopy()

	// A minute later only heartbeats, threads and queues moved: status stays as it was.
	nodes[0].Heartbeat, nodes[0].ActiveThreadCount, nodes[0].Queued = "later", 9, "99 / 1 MB"
	later := &nifiPoll{at: t0.Add(time.Minute), nodes: nodeStatuses(cluster, nodes),
		condition: clusterHealth(currentPodFQDNs(cluster), nodeStatuses(cluster, nodes))}
	applyNiFiPoll(&cluster.Status, later, 1)
	if cluster.Status.LastNiFiPoll.Time != before.LastNiFiPoll.Time || cluster.Status.Nodes[1].ActiveThreads == 9 {
		t.Error("volatile-only change was written before the refresh interval")
	}

	// After the refresh interval they are written.
	later.at = t0.Add(nifiVolatileRefresh + time.Second)
	applyNiFiPoll(&cluster.Status, later, 1)
	if cluster.Status.Nodes[1].ActiveThreads != 9 {
		t.Error("volatile figures not refreshed after the refresh interval")
	}

	// A state change is written at once.
	nodes[2].Status = "CONNECTED"
	applyNiFiPoll(&cluster.Status, &nifiPoll{at: later.at.Add(time.Second), nodes: nodeStatuses(cluster, nodes),
		condition: clusterHealth(currentPodFQDNs(cluster), nodeStatuses(cluster, nodes))}, 1)
	if cluster.Status.Nodes[2].State != "CONNECTED" {
		t.Error("state change not written immediately")
	}
}

func TestNodeGroupOf(t *testing.T) {
	cluster := statusCluster()
	for addr, want := range map[string]string{
		"st-a-0.st-a-headless.ns.svc.cluster.local":  "a",
		"st-b-12.st-b-headless.ns.svc.cluster.local": "b",
		"st-c-0.st-c-headless.ns.svc.cluster.local":  "",
		"st-a-x.st-a-headless.ns.svc.cluster.local":  "",
	} {
		if got := nodeGroupOf(cluster, addr); got != want {
			t.Errorf("nodeGroupOf(%s) = %q, want %q", addr, got, want)
		}
	}
}

func TestDropNodesWithoutID(t *testing.T) {
	st := &nifiv1alpha1.NiFiClusterStatus{
		Nodes:        []nifiv1alpha1.NiFiNodeStatus{{NodeID: "a", Address: "x"}, {Address: "y"}},
		LastNiFiPoll: &metav1.Time{Time: time.Now()},
	}
	dropNodesWithoutID(st)
	if st.Nodes != nil || st.LastNiFiPoll != nil {
		t.Errorf("0.6.5 node entries kept: %+v", st)
	}
	st.Nodes = []nifiv1alpha1.NiFiNodeStatus{{NodeID: "a", Address: "x"}}
	dropNodesWithoutID(st)
	if len(st.Nodes) != 1 {
		t.Error("current node entries dropped")
	}
}

// Found in an E2E run on kind: a rolling restart left pod-0's old identity DISCONNECTED beside its new one,
// and with removal only running after a scale-down it stayed forever.
func TestHasStaleIdentity(t *testing.T) {
	n := func(id, addr, state string) nifiv1alpha1.NiFiNodeStatus {
		return nifiv1alpha1.NiFiNodeStatus{NodeID: id, Address: addr, State: state}
	}
	for _, c := range []struct {
		name  string
		nodes []nifiv1alpha1.NiFiNodeStatus
		want  bool
	}{
		{"healthy", []nifiv1alpha1.NiFiNodeStatus{n("a", "x", "CONNECTED"), n("b", "y", "CONNECTED")}, false},
		{"stale identity", []nifiv1alpha1.NiFiNodeStatus{n("a", "x", "CONNECTED"), n("old", "x", "DISCONNECTED")}, true},
		{"offloaded stale identity", []nifiv1alpha1.NiFiNodeStatus{n("a", "x", "CONNECTED"), n("old", "x", "OFFLOADED")}, true},
		{"restarting, no new identity yet", []nifiv1alpha1.NiFiNodeStatus{n("old", "x", "DISCONNECTED"), n("b", "y", "CONNECTED")}, false},
		{"new identity still connecting", []nifiv1alpha1.NiFiNodeStatus{n("new", "x", "CONNECTING"), n("old", "x", "DISCONNECTED")}, false},
	} {
		if got := hasStaleIdentity(c.nodes); got != c.want {
			t.Errorf("%s: hasStaleIdentity = %v, want %v", c.name, got, c.want)
		}
	}
}
