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
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// NiFi's own view of the cluster in NiFiCluster status: version, members and their
// connection state, coordinator and primary node, and the ClusterHealthy condition.
const (
	clusterHealthyConditionType = "ClusterHealthy"

	// DefaultNiFiStatusInterval is how often the operator polls each cluster's NiFi REST API.
	// NiFi marks a dead node DISCONNECTED after about 40s of missed heartbeats, so polling
	// every 20s reports it within a minute.
	DefaultNiFiStatusInterval = 20 * time.Second

	// nifiStatusCallTimeout bounds one whole poll, so a hung NiFi cannot stall reconciles.
	nifiStatusCallTimeout = 10 * time.Second

	// nifiVolatileRefresh is how often heartbeat, thread and queue figures are rewritten when
	// nothing else about the nodes changed - they change on every poll, and writing status every
	// 20s for them alone would only churn the API server.
	nifiVolatileRefresh = 5 * time.Minute

	nifiRoleCoordinator   = "Cluster Coordinator"
	nifiRolePrimary       = "Primary Node"
	nifiStateConnected    = "CONNECTED"
	nifiStateDisconnected = "DISCONNECTED"
	nifiStateOffloading   = "OFFLOADING"
	nifiStateOffloaded    = "OFFLOADED"

	// nifiRequestDisconnect is the value PUT to .../controller/cluster/nodes/{id} to disconnect a
	// node - confirmed live (NiFi 2.12) to apply synchronously, the response already reporting
	// nifiStateDisconnected. OFFLOADING can only be requested for an already-disconnected node.
	nifiRequestDisconnect = "DISCONNECTING"
)

// nifiVersionAPI is the optional part of nifiAPI that reports the NiFi version; fakes may omit it.
type nifiVersionAPI interface {
	Version(ctx context.Context) (string, error)
}

// Version returns the version from GET /flow/about.
func (c *restNiFiAPI) Version(ctx context.Context) (string, error) {
	var resp struct {
		About struct {
			Version string `json:"version"`
		} `json:"about"`
	}
	body, err := c.do(ctx, http.MethodGet, "/flow/about", "", nil)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decoding /flow/about: %w", err)
	}
	return resp.About.Version, nil
}

// nifiPoll is the outcome of one poll, applied to status by updateStatus.
type nifiPoll struct {
	at        time.Time
	version   string
	nodes     []nifiv1alpha1.NiFiNodeStatus
	condition metav1.Condition
}

// pollNiFiStatus asks the cluster's NiFi for its view of the cluster, at most once per
// NiFiStatusInterval per cluster; it returns nil when it is not yet time. It never fails: every
// error becomes the ClusterHealthy condition, so the reconcile itself is unaffected.
func (r *NiFiClusterReconciler) pollNiFiStatus(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, readyReplicas int32,
) *nifiPoll {
	interval := r.nifiStatusInterval()
	now := time.Now()
	if last, ok := r.lastNiFiPoll.Load(cluster.UID); ok && now.Sub(last.(time.Time)) < interval {
		return nil
	}
	r.lastNiFiPoll.Store(cluster.UID, now)

	poll := &nifiPoll{at: now}
	if readyReplicas == 0 {
		poll.condition = healthCondition(metav1.ConditionUnknown, "NotReady", "No NiFi node is ready yet.")
		return poll
	}

	pollCtx, cancel := context.WithTimeout(ctx, nifiStatusCallTimeout)
	defer cancel()
	api, err := r.newNiFiAPI(pollCtx, cluster)
	var nodes []nifiNode
	if err == nil {
		nodes, err = api.ListNodes(pollCtx)
	}
	if err != nil {
		log.FromContext(ctx).V(1).Info("polling NiFi cluster status failed", "error", err.Error())
		poll.condition = pollErrorCondition(err)
		return poll
	}
	if v, ok := api.(nifiVersionAPI); ok {
		if poll.version, err = v.Version(pollCtx); err != nil {
			log.FromContext(ctx).V(1).Info("reading the NiFi version failed", "error", err.Error())
		}
	}
	poll.nodes = nodeStatuses(cluster, nodes)
	poll.condition = clusterHealth(currentPodFQDNs(cluster), poll.nodes)
	return poll
}

// nifiStatusInterval returns the configured poll interval.
func (r *NiFiClusterReconciler) nifiStatusInterval() time.Duration {
	if r.NiFiStatusInterval > 0 {
		return r.NiFiStatusInterval
	}
	return DefaultNiFiStatusInterval
}

// pollErrorCondition turns a poll failure into the ClusterHealthy condition.
func pollErrorCondition(err error) metav1.Condition {
	var httpErr *nifiHTTPError
	switch {
	case errors.As(err, &httpErr) && isAuthRejection(httpErr):
		return healthCondition(metav1.ConditionUnknown, "AuthFailed",
			fmt.Sprintf("The NiFi REST API refused the operator's credentials (%v); check the login Secret.", err))
	case errors.Is(err, errNoNiFiCredentials):
		return healthCondition(metav1.ConditionUnknown, "AuthFailed", "The NiFi login Secret does not exist.")
	case errors.As(err, &httpErr):
		return healthCondition(metav1.ConditionUnknown, "NiFiAPIError", fmt.Sprintf("Reading the NiFi cluster view failed: %v", err))
	default:
		return healthCondition(metav1.ConditionUnknown, "NiFiUnreachable", fmt.Sprintf("No NiFi node answered: %v", err))
	}
}

// isAuthRejection reports whether NiFi refused the operator's credentials: 401/403 on any call, or
// 400 on the login itself - NiFi 2.x answers a wrong single-user password with 400.
func isAuthRejection(e *nifiHTTPError) bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden ||
		(e.Code == http.StatusBadRequest && e.Path == "/access/token")
}

// nodeStatuses maps NiFi's node list to status, sorted by address, each node attributed to the
// node group whose pod name prefixes its address.
func nodeStatuses(cluster *nifiv1alpha1.NiFiCluster, nodes []nifiNode) []nifiv1alpha1.NiFiNodeStatus {
	out := make([]nifiv1alpha1.NiFiNodeStatus, 0, len(nodes))
	for _, n := range nodes {
		roles := slices.Clone(n.Roles)
		sort.Strings(roles)
		out = append(out, nifiv1alpha1.NiFiNodeStatus{
			NodeID:        n.NodeID,
			Address:       n.Address,
			NodeGroup:     nodeGroupOf(cluster, n.Address),
			State:         n.Status,
			Roles:         roles,
			LastHeartbeat: n.Heartbeat,
			ActiveThreads: n.ActiveThreadCount,
			Queued:        n.Queued,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Address != out[j].Address {
			return out[i].Address < out[j].Address
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

// nodeGroupOf returns the node group whose pods are named <cluster>-<group>-<ordinal>, matching
// the first DNS label of address; "" when none does.
func nodeGroupOf(cluster *nifiv1alpha1.NiFiCluster, address string) string {
	pod, _, _ := strings.Cut(address, ".")
	for i := range cluster.Spec.NodeGroups {
		ng := &cluster.Spec.NodeGroups[i]
		ordinal, ok := strings.CutPrefix(pod, statefulSetName(cluster, ng)+"-")
		if ok && ordinal != "" && strings.Trim(ordinal, "0123456789") == "" {
			return ng.Name
		}
	}
	return ""
}

// clusterHealth is True when every expected node is CONNECTED, else False listing the others. An
// address listed more than once (a restarted node's stale identity next to its new one) counts as
// connected when any of its entries is.
func clusterHealth(expected map[string]bool, nodes []nifiv1alpha1.NiFiNodeStatus) metav1.Condition {
	state := map[string]string{}
	for _, n := range nodes {
		if state[n.Address] != nifiStateConnected {
			state[n.Address] = n.State
		}
	}
	var notConnected []string
	for addr := range expected {
		switch s := state[addr]; s {
		case nifiStateConnected:
		case "":
			notConnected = append(notConnected, addr+" (not in the NiFi cluster)")
		default:
			notConnected = append(notConnected, addr+" ("+s+")")
		}
	}
	if len(notConnected) == 0 {
		return healthCondition(metav1.ConditionTrue, "AllNodesConnected",
			fmt.Sprintf("All %d NiFi nodes are connected.", len(expected)))
	}
	sort.Strings(notConnected)
	return healthCondition(metav1.ConditionFalse, "NodesNotConnected",
		"Not connected: "+strings.Join(notConnected, ", "))
}

func healthCondition(status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: clusterHealthyConditionType, Status: status, Reason: reason, Message: message}
}

// applyNiFiPoll writes poll into status. Node heartbeat, thread and queue figures change on every
// poll; when only they changed, they are refreshed at most every nifiVolatileRefresh.
func applyNiFiPoll(status *nifiv1alpha1.NiFiClusterStatus, poll *nifiPoll, generation int64) {
	if poll == nil {
		return
	}
	poll.condition.ObservedGeneration = generation
	apimeta.SetStatusCondition(&status.Conditions, poll.condition)
	if poll.condition.Status == metav1.ConditionUnknown && poll.nodes == nil {
		// No fresh view of the nodes: keep the last one rather than blanking it.
		return
	}

	if poll.version != "" {
		status.NiFiVersion = poll.version
	}
	status.ClusterCoordinator, status.PrimaryNode = "", ""
	for _, n := range poll.nodes {
		if slices.Contains(n.Roles, nifiRoleCoordinator) {
			status.ClusterCoordinator = n.Address
		}
		if slices.Contains(n.Roles, nifiRolePrimary) {
			status.PrimaryNode = n.Address
		}
	}

	refreshDue := status.LastNiFiPoll == nil || poll.at.Sub(status.LastNiFiPoll.Time) >= nifiVolatileRefresh
	if !refreshDue && apiequality.Semantic.DeepEqual(stableNodeView(status.Nodes), stableNodeView(poll.nodes)) {
		return
	}
	status.Nodes = poll.nodes
	status.LastNiFiPoll = &metav1.Time{Time: poll.at}
}

// dropNodesWithoutID forgets status.nodes written before entries carried a nodeId (operator 0.6.5).
// nodeId is now the list's key and required, so the API server would reject every status write
// that still carried such an entry; the next poll writes the list afresh.
func dropNodesWithoutID(status *nifiv1alpha1.NiFiClusterStatus) {
	for _, n := range status.Nodes {
		if n.NodeID == "" {
			status.Nodes, status.LastNiFiPoll = nil, nil
			return
		}
	}
}

// stableNodeView drops the figures that change on every poll.
func stableNodeView(nodes []nifiv1alpha1.NiFiNodeStatus) []nifiv1alpha1.NiFiNodeStatus {
	out := make([]nifiv1alpha1.NiFiNodeStatus, len(nodes))
	for i, n := range nodes {
		n.LastHeartbeat, n.ActiveThreads, n.Queued = "", 0, ""
		out[i] = n
	}
	return out
}

// parseQueuedBytes returns the byte size in NiFi's queued figure, formatted "<count> / <size>"
// with size like "0 bytes", "512 bytes", "4 KB", "1.5 MB" (binary multiples). ok is false when
// the figure cannot be read.
func parseQueuedBytes(queued string) (bytes int64, ok bool) {
	_, size, found := strings.Cut(queued, "/")
	if !found {
		return 0, false
	}
	fields := strings.Fields(strings.ReplaceAll(size, ",", ""))
	if len(fields) != 2 {
		return 0, false
	}
	n, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || n < 0 {
		return 0, false
	}
	unit := map[string]float64{
		"bytes": 1, "byte": 1, "B": 1,
		"KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40,
	}[fields[1]]
	if unit == 0 {
		return 0, false
	}
	return int64(n * unit), true
}

// markStaleIdentityRemoval sets NodeRemovalPending when poll shows a stale identity: a restarted
// pod (rolling restart, pod kill, eviction) rejoined under a new node ID and NiFi still lists its
// old one. The node-removal pass then deletes it, not only after a scale-down.
func markStaleIdentityRemoval(cluster *nifiv1alpha1.NiFiCluster, poll *nifiPoll) {
	if poll != nil && hasStaleIdentity(poll.nodes) {
		cluster.Status.NodeRemovalPending = true
	}
}

// hasStaleIdentity reports whether NiFi lists an address both as CONNECTED and, under another node
// ID, as DISCONNECTED or OFFLOADED: the old identity of a pod that restarted and rejoined, which
// removeDepartedNodes deletes.
func hasStaleIdentity(nodes []nifiv1alpha1.NiFiNodeStatus) bool {
	connected, gone := map[string]bool{}, map[string]bool{}
	for _, n := range nodes {
		switch n.State {
		case nifiStateConnected:
			connected[n.Address] = true
		case nifiStateDisconnected, nifiStateOffloaded:
			gone[n.Address] = true
		}
	}
	for addr := range gone {
		if connected[addr] {
			return true
		}
	}
	return false
}

// readyReplicas sums the ready replicas of every node group.
func readyReplicas(statuses []nifiv1alpha1.NodeGroupStatus) int32 {
	var ready int32
	for _, st := range statuses {
		ready += st.ReadyReplicas
	}
	return ready
}
