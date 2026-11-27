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
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// TestNewNiFiAPIUsesDefaultLoginOnTLS is a regression test: with TLS and no
// spec.authentication, node removal must log in with the operator-generated default login Secret,
// not give up with errNoNiFiCredentials. The node hostnames don't resolve here, so success means
// newNiFiAPI got past the credential and CA lookup and tried to reach a node.
func TestNewNiFiAPIUsesDefaultLoginOnTLS(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "h5", Namespace: "ns"},
		Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
			{Name: "default", Replicas: 1, TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
		}},
	}
	caPEM, caKey, err := GenerateCA(1)
	if err != nil {
		t.Fatal(err)
	}
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tlsCASecretName(cluster), Namespace: "ns"},
		Data:       map[string][]byte{corev1.TLSCertKey: caPEM, corev1.TLSPrivateKeyKey: caKey},
	}
	login, err := buildDefaultAuthSecret(cluster)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		objects []client.Object
		wantErr string
	}{
		{"default login present", []client.Object{ca, login}, "no NiFi node answered"},
		{"no login Secret yet", []client.Object{ca}, errNoNiFiCredentials.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			r := &NiFiClusterReconciler{Client: c}
			_, err := r.newNiFiAPI(context.Background(), cluster)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("newNiFiAPI() err = %v, want it to contain %q", err, tc.wantErr)
			}
			if tc.wantErr != errNoNiFiCredentials.Error() && errors.Is(err, errNoNiFiCredentials) {
				t.Fatalf("newNiFiAPI() gave up with errNoNiFiCredentials although the default login exists")
			}
		})
	}
}

// a node that answers but can't serve the cluster view (it is itself disconnected)
// must not be picked; firstAnswering then moves on to the next node.
func TestCanListNodesRejectsDisconnectedNode(t *testing.T) {
	disconnected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/nifi-api/controller/cluster" {
			w.WriteHeader(http.StatusConflict) // what NiFi answers while this node is disconnected
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer disconnected.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cluster":{"nodes":[{"nodeId":"n1","address":"a","status":"CONNECTED"}]}}`))
	}))
	defer healthy.Close()

	urls := map[string]string{"node-0": disconnected.URL, "node-1": healthy.URL}
	api, err := firstAnswering([]string{"node-0", "node-1"}, func(host string) (nifiAPI, error) {
		c := &restNiFiAPI{http: http.DefaultClient, base: urls[host] + "/nifi-api"}
		return c, canListNodes(context.Background(), c)
	})
	if err != nil {
		t.Fatalf("firstAnswering() error = %v, want the healthy node", err)
	}
	if got := api.(*restNiFiAPI).base; got != healthy.URL+"/nifi-api" {
		t.Errorf("picked %q, want the healthy node %q", got, healthy.URL)
	}
}

// NIFI-248 B4: confirmOffloaded must tell "pod 4 is gone because nifi.sh decommission offloaded
// its data" apart from "pod 4 is gone because decommission failed and it was stopped (or
// force-killed at the end of its grace period) anyway" - stepScaleDown used to be unable to (see
// git history of this test), which meant whenScaled: Delete could delete a node's PVC before
// confirming its data actually left. This drives disconnect+offload itself and only reports true
// once NiFi's own state says OFFLOADED.
// readyNotReadyPod builds a minimal Pod with its PodReady condition set as requested.
func readyNotReadyPod(name string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

// newConfirmOffloadedFixture builds a fresh reconciler/cluster/fake-API triple shared by the
// TestConfirmOffloaded_* functions below, split out of one large table so each stays under
// gocyclo's complexity threshold without losing any of the original coverage.
func newConfirmOffloadedFixture(t *testing.T) (*NiFiClusterReconciler, *nifiv1alpha1.NiFiCluster, *fakeNiFiAPI) {
	t.Helper()
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	api := &fakeNiFiAPI{nodes: []nifiNode{
		{NodeID: "n4", Address: "c-default-4.c-default-headless.ns.svc.cluster.local", Status: "CONNECTED", Queued: "10 / 1 MB"},
	}}
	scheme := runtime.NewScheme()
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).WithStatusSubresource(cluster).Build()
	r := &NiFiClusterReconciler{
		Client:     c,
		NewNiFiAPI: func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return api, nil },
	}
	return r, cluster, api
}

// TestConfirmOffloaded_NodeStates covers the CONNECTED -> OFFLOADING -> OFFLOADED progression for
// a node confirmOffloaded can actually see in NiFi's own node list.
func TestConfirmOffloaded_NodeStates(t *testing.T) {
	r, cluster, api := newConfirmOffloadedFixture(t)
	readyPod := readyNotReadyPod("c-default-4", true)

	// CONNECTED: disconnects then requests offload, and holds - never removes the pod on the
	// strength of a request it only just made. confirmOffloaded calls the API itself (ListNodes),
	// not a cached status, so this is a live call against the fake every time.
	offloaded, err := r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod)
	if err != nil || offloaded {
		t.Fatalf("confirmOffloaded() on CONNECTED = %v, %v; want false, nil", offloaded, err)
	}
	if want := []string{"n4:" + nifiRequestDisconnect, "n4:" + nifiStateOffloading}; !slices.Equal(api.statusSets, want) {
		t.Errorf("API calls = %v, want %v", api.statusSets, want)
	}

	// OFFLOADING (still in progress, e.g. the next reconcile after the request above): holds,
	// warns, makes no further request - idempotent re-requests are harmless but pointless here.
	api.statusSets = nil
	api.nodes[0].Status = "OFFLOADING"
	offloaded, err = r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod)
	if err != nil || offloaded {
		t.Fatalf("confirmOffloaded() on OFFLOADING = %v, %v; want false, nil", offloaded, err)
	}
	if len(api.statusSets) != 0 {
		t.Errorf("API calls while OFFLOADING = %v, want none", api.statusSets)
	}

	// OFFLOADED: only now does it report true, letting stepScaleDown lower replicas.
	api.nodes[0].Status = "OFFLOADED"
	offloaded, err = r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod)
	if err != nil || !offloaded {
		t.Fatalf("confirmOffloaded() on OFFLOADED = %v, %v; want true, nil", offloaded, err)
	}
}

// TestConfirmOffloadedResetsStaleOffloadStartedCacheEntry is the regression test for the second
// real bug found live during PR #210 review verification: offloadStartedCache was never reset
// when a fresh offload attempt starts, so a stale timestamp left over from an earlier, cancelled
// attempt against the same pod made the very next attempt look instantly stuck - a spurious
// OffloadStuck Warning on the first check, not after offloadStuckTimeout has genuinely elapsed.
func TestConfirmOffloadedResetsStaleOffloadStartedCacheEntry(t *testing.T) {
	r, cluster, api := newConfirmOffloadedFixture(t)
	readyPod := readyNotReadyPod("c-default-4", true)
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: "c-default-4"}

	// A stale entry from an earlier, cancelled attempt against this same pod - old enough that,
	// if left in place, the very next check below would immediately look stuck.
	r.offloadStartedCache.Store(key, time.Now().Add(-2*offloadStuckTimeout))

	api.nodes[0].Status = "CONNECTED"
	if offloaded, err := r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod); err != nil || offloaded {
		t.Fatalf("confirmOffloaded() on CONNECTED = %v, %v; want false, nil", offloaded, err)
	}

	api.statusSets = nil
	api.nodes[0].Status = "OFFLOADING"
	recorder := record.NewFakeRecorder(2)
	r.Recorder = recorder
	if offloaded, err := r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod); err != nil || offloaded {
		t.Fatalf("confirmOffloaded() on OFFLOADING right after a fresh start = %v, %v; want false, nil", offloaded, err)
	}
	select {
	case msg := <-recorder.Events:
		if !strings.Contains(msg, corev1.EventTypeNormal) || !strings.Contains(msg, reasonOffloadInProgress) {
			t.Errorf("event right after a fresh start = %q, want Normal/%s, not a stale-cache false OffloadStuck", msg, reasonOffloadInProgress)
		}
	default:
		t.Fatal("confirmOffloaded recorded no event on the first OFFLOADING check after a fresh start")
	}
}

// TestConfirmOffloaded_UnlistedPod covers a target ordinal confirmOffloaded cannot find in NiFi's
// own node list, with its pod in each of the states that matter (NIFI-248 PR #210 review:
// scale-down must not hang forever on a node that never joined, e.g. after scaling back down
// while a replacement pod is stuck Pending).
func TestConfirmOffloaded_UnlistedPod(t *testing.T) {
	r, cluster, api := newConfirmOffloadedFixture(t)

	// Not in NiFi's own view at all, but its pod is currently Ready: the listing may simply not
	// have caught up yet with this node joining - hold rather than guess either way, and never
	// call the API for a node it cannot identify.
	offloaded, err := r.confirmOffloaded(context.Background(), cluster, "c-default-9", readyNotReadyPod("c-default-9", true))
	if err != nil || offloaded || len(api.statusSets) != 0 {
		t.Fatalf("confirmOffloaded() for a ready-but-unlisted pod = %v, %v, calls=%v; want false, nil, none", offloaded, err, api.statusSets)
	}

	// Not in NiFi's own view, and its pod does not exist: it could never have joined, so there is
	// nothing to offload.
	offloaded, err = r.confirmOffloaded(context.Background(), cluster, "c-default-9", nil)
	if err != nil || !offloaded || len(api.statusSets) != 0 {
		t.Fatalf("confirmOffloaded() for a nonexistent pod = %v, %v, calls=%v; want true, nil, none", offloaded, err, api.statusSets)
	}

	// Not in NiFi's own view, and its pod exists but has never become Ready (stuck Pending with no
	// PV, or crash-looping): same as nonexistent - nothing to offload.
	offloaded, err = r.confirmOffloaded(context.Background(), cluster, "c-default-9", readyNotReadyPod("c-default-9", false))
	if err != nil || !offloaded || len(api.statusSets) != 0 {
		t.Fatalf("confirmOffloaded() for a never-ready pod = %v, %v, calls=%v; want true, nil, none", offloaded, err, api.statusSets)
	}
}

// TestConfirmOffloaded_ScaleDownBlockedConditions covers every error path confirmOffloaded can
// take while trying to reach the API, confirming each sets a specific ScaleDownBlocked reason
// instead of failing silently (NIFI-248 PR #210 review), and that recovery clears it.
func TestConfirmOffloaded_ScaleDownBlockedConditions(t *testing.T) {
	r, cluster, api := newConfirmOffloadedFixture(t)
	readyPod := readyNotReadyPod("c-default-4", true)

	// No NiFi credentials yet: holds, never forces the scale-down through, and the reason is now
	// visible as a condition rather than only a transient reconcile error.
	r.NewNiFiAPI = func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return nil, errNoNiFiCredentials }
	offloaded, err := r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod)
	if err != nil || offloaded {
		t.Fatalf("confirmOffloaded() with no credentials = %v, %v; want false, nil", offloaded, err)
	}
	if cond := apimeta.FindStatusCondition(cluster.Status.Conditions, scaleDownBlockedConditionType); cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != "NoCredentials" {
		t.Errorf("ScaleDownBlocked = %+v, want True/NoCredentials", cond)
	}

	// Cannot reach any node to even list them: holds, surfaces no error (expected/transient), and
	// updates the condition's reason.
	r.NewNiFiAPI = func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) {
		return nil, errors.New("no host answered")
	}
	offloaded, err = r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod)
	if err != nil || offloaded {
		t.Fatalf("confirmOffloaded() unreachable = %v, %v; want false, nil", offloaded, err)
	}
	if cond := apimeta.FindStatusCondition(cluster.Status.Conditions, scaleDownBlockedConditionType); cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != "APIUnreachable" {
		t.Errorf("ScaleDownBlocked = %+v, want True/APIUnreachable", cond)
	}

	// The node itself never responds to the offload request: holds, still returns the original
	// error for the caller to retry/back off on, and records why.
	r.NewNiFiAPI = func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return api, nil }
	api.statusErr = errors.New("connection reset")
	offloaded, err = r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod)
	if offloaded || err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("confirmOffloaded() unresponsive node = %v, %v; want false, an error naming the cause", offloaded, err)
	}
	if cond := apimeta.FindStatusCondition(cluster.Status.Conditions, scaleDownBlockedConditionType); cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != "NodeUnresponsive" {
		t.Errorf("ScaleDownBlocked = %+v, want True/NodeUnresponsive", cond)
	}

	// Recovered: the next successful step clears the condition.
	api.statusErr = nil
	api.nodes[0].Status = "OFFLOADED"
	offloaded, err = r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod)
	if err != nil || !offloaded {
		t.Fatalf("confirmOffloaded() after recovery = %v, %v; want true, nil", offloaded, err)
	}
	if cond := apimeta.FindStatusCondition(cluster.Status.Conditions, scaleDownBlockedConditionType); cond != nil {
		t.Errorf("ScaleDownBlocked = %+v, want cleared once offload succeeds", cond)
	}
}

// TestConfirmOffloadedProgressEventSeverity is the regression test for PR #210's review:
// OffloadStuck used to fire as a Warning on every single requeue of a normal, still-progressing
// offload. It must stay Normal (OffloadInProgress) until offloadStuckTimeout has genuinely
// elapsed, only then escalating to a Warning (OffloadStuck).
func TestConfirmOffloadedProgressEventSeverity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nifiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).WithStatusSubresource(cluster).Build()
	api := &fakeNiFiAPI{nodes: []nifiNode{
		{NodeID: "n4", Address: "c-default-4.c-default-headless.ns.svc.cluster.local", Status: "OFFLOADING", Queued: "10 / 1 MB"},
	}}
	recorder := record.NewFakeRecorder(2)
	r := &NiFiClusterReconciler{
		Client:     c,
		Recorder:   recorder,
		NewNiFiAPI: func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return api, nil },
	}
	readyPod := readyNotReadyPod("c-default-4", true)

	if _, err := r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-recorder.Events:
		if !strings.Contains(msg, corev1.EventTypeNormal) || !strings.Contains(msg, reasonOffloadInProgress) {
			t.Errorf("first check event = %q, want Normal/%s", msg, reasonOffloadInProgress)
		}
	default:
		t.Fatal("confirmOffloaded recorded no event on the first OFFLOADING check")
	}

	// Backdate the cached start time past offloadStuckTimeout to simulate a node that has
	// genuinely been offloading too long, without waiting in real time.
	r.offloadStartedCache.Store(types.NamespacedName{Namespace: cluster.Namespace, Name: "c-default-4"}, time.Now().Add(-offloadStuckTimeout-time.Second))
	if _, err := r.confirmOffloaded(context.Background(), cluster, "c-default-4", readyPod); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-recorder.Events:
		if !strings.Contains(msg, corev1.EventTypeWarning) || !strings.Contains(msg, reasonOffloadStuck) {
			t.Errorf("event after the timeout = %q, want Warning/%s", msg, reasonOffloadStuck)
		}
	default:
		t.Fatal("confirmOffloaded recorded no event once genuinely stuck")
	}
}

// TestStepScaleDownNeverJoinedPod is the regression test for PR #210's review: scale 1 -> 3, pod
// -2 stays Pending (no PV) or keeps crashing and never joins NiFi, then the user scales back down
// to 1. Before the fix, stepScaleDown held forever because the node was never in NiFi's own view.
func TestStepScaleDownNeverJoinedPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	selector := map[string]string{"app": "nifi-test"}
	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "c-default", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptrTo(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
	}
	stuckPod := readyNotReadyPod("c-default-2", false)
	stuckPod.Namespace = "ns"
	stuckPod.Labels = selector

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing, stuckPod).Build()
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	r := &NiFiClusterReconciler{
		Client:    c,
		APIReader: c,
		NewNiFiAPI: func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) {
			return &fakeNiFiAPI{}, nil // c-default-2 never joined: empty node list.
		},
	}

	desired := existing.DeepCopy()
	desired.Spec.Replicas = ptrTo(int32(1))
	waiting, err := r.stepScaleDown(context.Background(), cluster, desired)
	if err != nil {
		t.Fatal(err)
	}
	if waiting {
		t.Fatal("stepScaleDown held on a pod that never joined NiFi and was never Ready; want it to proceed")
	}
	if got := *desired.Spec.Replicas; got != 2 {
		t.Errorf("Replicas = %d, want 2 (lowered past the never-joined node in one step)", got)
	}
}

// TestStepScaleDownReconnectsStrandedNodeOnCancelledScaleDown is the regression test for PR
// #210's review: scale 3 -> 2, then back to 3 while node -2 is still OFFLOADING. Before the fix,
// stepScaleDown's early return (desired >= current) left that node disconnected from NiFi
// forever even though its pod was back inside the desired range.
func TestStepScaleDownReconnectsStrandedNodeOnCancelledScaleDown(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	selector := map[string]string{"app": "nifi-test"}
	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "c-default", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptrTo(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
	}
	pods := make([]client.Object, 0, 3)
	for i := range 3 {
		pod := readyNotReadyPod(fmt.Sprintf("c-default-%d", i), i != 2) // -2 is the stranded one
		pod.Namespace = "ns"
		pod.Labels = selector
		pods = append(pods, pod)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithObjects(pods...).Build()
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	api := &fakeNiFiAPI{nodes: []nifiNode{
		{NodeID: "n2", Address: "c-default-2.c-default-headless.ns.svc.cluster.local", Status: "DISCONNECTED"},
	}}
	r := &NiFiClusterReconciler{
		Client:     c,
		APIReader:  c,
		NewNiFiAPI: func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return api, nil },
	}
	// This operator's own earlier scale-down step did put c-default-2 into offload before it was
	// cancelled - reconnectStrandedNodes only ever acts on a pod in this cache (NIFI-248 PR #210
	// review: it must never touch a node disconnected for any other reason, e.g. a
	// coordinator-loss event - see TestStepScaleDownNeverTouchesAnUntrackedDisconnectedNode).
	r.offloadStartedCache.Store(types.NamespacedName{Namespace: "ns", Name: "c-default-2"}, time.Now().Add(-time.Minute))

	desired := existing.DeepCopy() // cancelled: back to the original 3, same as existing
	waiting, err := r.stepScaleDown(context.Background(), cluster, desired)
	if err != nil {
		t.Fatal(err)
	}
	if waiting {
		t.Error("stepScaleDown should not report waiting when nothing is being removed")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "c-default-2", Namespace: "ns"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("stranded pod c-default-2 err = %v, want NotFound (deleted so it rejoins NiFi)", err)
	}
	for _, name := range []string{"c-default-0", "c-default-1"} {
		if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "ns"}, &corev1.Pod{}); err != nil {
			t.Errorf("healthy pod %s err = %v, want it left alone", name, err)
		}
	}
	if _, tracked := r.offloadStartedCache.Load(types.NamespacedName{Namespace: "ns", Name: "c-default-2"}); tracked {
		t.Error("offloadStartedCache still tracks c-default-2 after reconnecting it, want it cleared")
	}
}

// TestStepScaleDownNeverTouchesAnUntrackedDisconnectedNode is the regression test for the second
// real bug found live during PR #210 review verification of the first version of
// reconnectStrandedNodes: it scanned NiFi's own node list unconditionally, so a node DISCONNECTED
// for any reason at all - not just one this operator's own scale-down step had put into offload -
// got force-deleted once its pod passed reconnectStrandedNodeGracePeriod. That is exactly what a
// coordinator-loss event looks like (every node DISCONNECTED, every pod NotReady together), so the
// unconditional version would have force-restarted the entire cluster in one pass, bypassing its
// PodDisruptionBudget - the same mechanism would also repeatedly kill a merely slow-starting node,
// or silently revert an admin's own deliberate manual disconnect. None of those nodes was ever put
// into offload by this operator, so this test's node (DISCONNECTED, old pod, no desired-replica
// change in flight at all - not even a cancelled scale-down) must be left completely alone.
func TestStepScaleDownNeverTouchesAnUntrackedDisconnectedNode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	selector := map[string]string{"app": "nifi-test"}
	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "c-default", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptrTo(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
	}
	pods := make([]client.Object, 0, 3)
	for i := range 3 {
		// Every node DISCONNECTED and every pod NotReady together, as in a real coordinator-loss
		// event - not one isolated "stranded" node.
		pod := readyNotReadyPod(fmt.Sprintf("c-default-%d", i), false)
		pod.Namespace = "ns"
		pod.Labels = selector
		pods = append(pods, pod)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithObjects(pods...).Build()
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	api := &fakeNiFiAPI{nodes: []nifiNode{
		{NodeID: "n0", Address: "c-default-0.c-default-headless.ns.svc.cluster.local", Status: "DISCONNECTED"},
		{NodeID: "n1", Address: "c-default-1.c-default-headless.ns.svc.cluster.local", Status: "DISCONNECTED"},
		{NodeID: "n2", Address: "c-default-2.c-default-headless.ns.svc.cluster.local", Status: "DISCONNECTED"},
	}}
	r := &NiFiClusterReconciler{
		Client:     c,
		APIReader:  c,
		NewNiFiAPI: func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return api, nil },
	}
	// offloadStartedCache is deliberately left empty: this operator never put any of these nodes
	// into offload - no scale-down of any kind is in flight.

	desired := existing.DeepCopy() // replicas unchanged: nothing being scaled
	if _, err := r.stepScaleDown(context.Background(), cluster, desired); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		name := fmt.Sprintf("c-default-%d", i)
		if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "ns"}, &corev1.Pod{}); err != nil {
			t.Errorf("pod %s err = %v, want it left alone (never tracked as offloading)", name, err)
		}
	}
}

// TestStepScaleDownDoesNotRestartARecentlyRestartedStrandedNode is the regression test for a bug
// found live (PR #210 review verification): without a grace period, the freshly-restarted pod is
// deleted again on every next reconcile - NiFi's own node list keeps showing the old disconnected
// identity at that address until the fresh pod actually boots and reconnects, which takes longer
// than one reconcile. Confirmed live: 305 repeats of the delete in under a minute before this fix.
func TestStepScaleDownDoesNotRestartARecentlyRestartedStrandedNode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	selector := map[string]string{"app": "nifi-test"}
	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "c-default", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptrTo(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
	}
	pods := make([]client.Object, 0, 3)
	for i := range 3 {
		pod := readyNotReadyPod(fmt.Sprintf("c-default-%d", i), i != 2) // -2 is the stranded one
		pod.Namespace = "ns"
		pod.Labels = selector
		if i == 2 {
			pod.CreationTimestamp = metav1.NewTime(time.Now()) // just restarted
		}
		pods = append(pods, pod)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).WithObjects(pods...).Build()
	cluster := &nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
	api := &fakeNiFiAPI{nodes: []nifiNode{
		{NodeID: "n2", Address: "c-default-2.c-default-headless.ns.svc.cluster.local", Status: "DISCONNECTED"},
	}}
	r := &NiFiClusterReconciler{
		Client:     c,
		APIReader:  c,
		NewNiFiAPI: func(context.Context, *nifiv1alpha1.NiFiCluster) (nifiAPI, error) { return api, nil },
	}
	r.offloadStartedCache.Store(types.NamespacedName{Namespace: "ns", Name: "c-default-2"}, time.Now().Add(-time.Minute))

	desired := existing.DeepCopy()
	if _, err := r.stepScaleDown(context.Background(), cluster, desired); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "c-default-2", Namespace: "ns"}, &corev1.Pod{}); err != nil {
		t.Errorf("recently-restarted pod c-default-2 err = %v, want it left alone within the grace period", err)
	}
}

// the Secret cache keeps data only for the operator's own Secrets.
func TestStripForeignSecretData(t *testing.T) {
	data := map[string][]byte{"k": []byte("v")}
	owned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
		APIVersion: nifiv1alpha1.GroupVersion.String(), Kind: "NiFiCluster", Name: "c", Controller: ptrTo(true),
	}}}, Data: data}
	labelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelAppManagedBy: managedByValue}}, Data: data}
	foreign := &corev1.Secret{Data: data, StringData: map[string]string{"k": "v"}}
	for name, tc := range map[string]struct {
		secret   *corev1.Secret
		wantData bool
	}{"owned": {owned, true}, "labelled": {labelled, true}, "foreign": {foreign, false}} {
		out, err := stripForeignSecretData(tc.secret)
		if err != nil {
			t.Fatal(err)
		}
		s := out.(*corev1.Secret)
		if (len(s.Data) > 0) != tc.wantData || (!tc.wantData && len(s.StringData) > 0) {
			t.Errorf("%s: data kept = %v, want %v", name, len(s.Data) > 0, tc.wantData)
		}
	}
}
