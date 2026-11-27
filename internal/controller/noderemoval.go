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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// membershipConditionType is only present (False) while the operator cannot remove departed nodes
// from the NiFi cluster.
const membershipConditionType = "MembershipUpToDate"

// nodeRemovalRetryInterval is how often pending node removal is retried while the remaining nodes
// are not all ready yet.
const nodeRemovalRetryInterval = 30 * time.Second

// errNoNiFiCredentials means the operator cannot authenticate to a TLS-secured NiFi cluster,
// because the login credentials Secret does not exist (yet).
var errNoNiFiCredentials = errors.New("no credentials to call the NiFi API")

// nifiNode is the part of a NiFi cluster member the operator uses.
type nifiNode struct {
	NodeID            string   `json:"nodeId"`
	Address           string   `json:"address"`
	Status            string   `json:"status"`
	Roles             []string `json:"roles"`
	Heartbeat         string   `json:"heartbeat"`
	ActiveThreadCount int32    `json:"activeThreadCount"`
	Queued            string   `json:"queued"`
}

// nifiAPI is the slice of the NiFi REST API used to remove departed nodes. An interface so tests
// can substitute a fake.
type nifiAPI interface {
	ListNodes(ctx context.Context) ([]nifiNode, error)
	DeleteNode(ctx context.Context, nodeID string) error
	SetNodeStatus(ctx context.Context, nodeID, status string) error
}

// reconcileNodeRemoval deletes from the NiFi cluster every node that is no longer backed by a pod.
//
// NiFi never forgets a member by itself: a node that was decommissioned (preStop) or simply went
// away stays listed as DISCONNECTED or OFFLOADED, the UI keeps reporting it, and the cluster keeps
// expecting it. Once a scale-down has settled - every remaining pod ready - the departed nodes are
// deleted through the REST API (DELETE /controller/cluster/nodes/{id}). Only nodes that are both
// gone from the spec and no longer connected are touched.
func (r *NiFiClusterReconciler) reconcileNodeRemoval(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (ctrl.Result, error) {
	if cluster.Status.ReadyReplicas < cluster.Status.Replicas || cluster.Status.Replicas == 0 {
		return ctrl.Result{RequeueAfter: nodeRemovalRetryInterval}, nil
	}

	api, err := r.newNiFiAPI(ctx, cluster)
	if errors.Is(err, errNoNiFiCredentials) {
		_, cerr := r.setCondition(ctx, cluster, membershipConditionType, metav1.ConditionFalse, "NoCredentials",
			"Departed nodes cannot be removed from the NiFi cluster yet: its login credentials Secret ("+
				effectiveAuthSecretName(cluster)+") does not exist.")
		return ctrl.Result{}, cerr
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	removed, waiting, err := removeDepartedNodes(ctx, api, currentPodFQDNs(cluster), r.podGone(ctx, cluster.Namespace))
	if err != nil {
		if _, cerr := r.setCondition(ctx, cluster, membershipConditionType, metav1.ConditionFalse, "NiFiAPIError",
			fmt.Sprintf("Removing departed nodes from the NiFi cluster failed: %v", err)); cerr != nil {
			return ctrl.Result{}, cerr
		}
		return ctrl.Result{RequeueAfter: nodeRemovalRetryInterval}, nil
	}
	for _, n := range removed {
		r.event(cluster, corev1.EventTypeNormal, reasonNodeRemoved, fmt.Sprintf("removed departed node %s from the NiFi cluster", n))
	}
	if len(removed) > 0 {
		log.FromContext(ctx).Info("removed departed NiFi nodes", "nodes", removed)
	}
	if waiting > 0 {
		// A departed node can still be listed as connected for a while: when the node that left was
		// the Cluster Coordinator, the new coordinator only notices once its heartbeats time out
		// (8 missed heartbeats, about 40 seconds by default). Keep the removal pending until then.
		return ctrl.Result{RequeueAfter: nodeRemovalRetryInterval}, nil
	}

	if err := r.removeCondition(ctx, cluster, membershipConditionType); err != nil {
		return ctrl.Result{}, err
	}
	cluster.Status.NodeRemovalPending = false
	return ctrl.Result{}, r.updateClusterStatus(ctx, cluster)
}

// removeDepartedNodes deletes every listed node whose address is not a current pod, which is not
// connected, and whose pod no longer exists, plus every disconnected stale identity of a current
// pod that is connected under another node ID, returning the addresses removed and how many departed
// nodes are not deletable yet. A node is only deleted once its pod is gone: a pod that is still
// shutting down can report its status to the coordinator once more and re-register a node that
// was just deleted (seen live, seconds after decommission).
func removeDepartedNodes(
	ctx context.Context, api nifiAPI, current map[string]bool, podGone func(address string) bool,
) (removed []string, waiting int, err error) {
	nodes, err := api.ListNodes(ctx)
	if err != nil {
		return nil, 0, err
	}
	connected := map[string]bool{}
	for _, n := range nodes {
		if n.Status == nifiStateConnected {
			connected[n.Address] = true
		}
	}
	for _, n := range nodes {
		if current[n.Address] {
			// A restarted pod rejoins under a new node ID; NiFi can keep listing its old identity,
			// disconnected, at the same address. Delete that one once the new one is connected.
			// The disconnected entry cannot be the live identity going through its transient
			// DISCONNECTED phase: removal only runs with every pod Ready, and readiness requires the
			// pod's own current identity to be connected (readinessProbeHandler). Were it ever
			// deleted anyway, the live node re-registers on its next heartbeat.
			if connected[n.Address] && (n.Status == nifiStateDisconnected || n.Status == nifiStateOffloaded) {
				if err := api.DeleteNode(ctx, n.NodeID); err != nil {
					return removed, waiting, fmt.Errorf("deleting stale identity of node %s: %w", n.Address, err)
				}
				removed = append(removed, n.Address+" (stale identity "+n.NodeID+")")
			}
			continue
		}
		if (n.Status != nifiStateDisconnected && n.Status != nifiStateOffloaded) || !podGone(n.Address) {
			waiting++
			continue
		}
		if err := api.DeleteNode(ctx, n.NodeID); err != nil {
			return removed, waiting, fmt.Errorf("deleting node %s: %w", n.Address, err)
		}
		removed = append(removed, n.Address)
	}
	return removed, waiting, nil
}

// podGone returns a check reporting whether the pod behind a NiFi node address (its FQDN, whose
// first label is the pod name) no longer exists. Read straight from the API server: the operator
// does not cache Pods.
func (r *NiFiClusterReconciler) podGone(ctx context.Context, namespace string) func(address string) bool {
	return func(address string) bool {
		name, _, _ := strings.Cut(address, ".")
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &corev1.Pod{})
		return apierrors.IsNotFound(err)
	}
}

// currentPodFQDNs returns the addresses NiFi knows the cluster's current pods by.
func currentPodFQDNs(cluster *nifiv1alpha1.NiFiCluster) map[string]bool {
	out := map[string]bool{}
	for i := range cluster.Spec.NodeGroups {
		ng := &cluster.Spec.NodeGroups[i]
		for ordinal := int32(0); ordinal < ng.Replicas; ordinal++ {
			pod := fmt.Sprintf("%s-%d", statefulSetName(cluster, ng), ordinal)
			out[PodFQDN(pod, cluster.Name, ng.Name, cluster.Namespace)] = true
		}
	}
	return out
}

// newNiFiAPI returns a client for the cluster's REST API through the first current pod that
// answers (pods tried in a stable order), so one node being down - or restarting - does not block
// the removal. With TLS, node certificates are verified against the cluster CA and the operator
// logs in with the single-user credentials from spec.authentication; login tokens are signed per
// node, so it logs in on the very node it then talks to.
func (r *NiFiClusterReconciler) newNiFiAPI(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (nifiAPI, error) {
	if r.NewNiFiAPI != nil {
		return r.NewNiFiAPI(ctx, cluster)
	}
	hosts := make([]string, 0, len(currentPodFQDNs(cluster)))
	for h := range currentPodFQDNs(cluster) {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	if !clusterTLSEnabled(cluster) {
		return firstAnswering(hosts, func(host string) (nifiAPI, error) {
			c := &restNiFiAPI{http: &http.Client{Timeout: 15 * time.Second}, base: fmt.Sprintf("http://%s:%d/nifi-api", host, nifiHTTPPort)}
			return c, canListNodes(ctx, c)
		})
	}

	// The same Secret the pods log in with (user-supplied, synthesized from literal fields, or the
	// operator-generated default when spec.authentication is unset).
	creds := &corev1.Secret{}
	err := r.secretReader().Get(ctx, client.ObjectKey{Name: effectiveAuthSecretName(cluster), Namespace: cluster.Namespace}, creds)
	if apierrors.IsNotFound(err) {
		return nil, errNoNiFiCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("getting NiFi credentials Secret: %w", err)
	}
	ca := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: tlsCASecretName(cluster), Namespace: cluster.Namespace}, ca); err != nil {
		return nil, fmt.Errorf("getting TLS CA Secret: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.Data[corev1.TLSCertKey]) {
		return nil, errors.New("TLS CA Secret holds no usable certificate")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}

	return firstAnswering(hosts, func(host string) (nifiAPI, error) {
		c := &restNiFiAPI{
			http: &http.Client{Timeout: 15 * time.Second, Transport: transport},
			base: fmt.Sprintf("https://%s:%d/nifi-api", host, nifiHTTPSPort),
		}
		if err := c.login(ctx, string(creds.Data[authSecretUsernameKey]), string(creds.Data[authSecretPasswordKey])); err != nil {
			return nil, err
		}
		return c, canListNodes(ctx, c)
	})
}

// canListNodes checks that c can actually serve the cluster view. A node that answers but is
// itself disconnected fails /controller/cluster, and must not be picked over a healthy one.
func canListNodes(ctx context.Context, c nifiAPI) error {
	if _, err := c.ListNodes(ctx); err != nil {
		return fmt.Errorf("reading the cluster view: %w", err)
	}
	return nil
}

// firstAnswering returns the client of the first host for which connect succeeds, or every
// host's error.
func firstAnswering(hosts []string, connect func(host string) (nifiAPI, error)) (nifiAPI, error) {
	var errs []error
	for _, host := range hosts {
		c, err := connect(host)
		if err == nil {
			return c, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", host, err))
	}
	return nil, fmt.Errorf("no NiFi node answered: %w", errors.Join(errs...))
}

// nifiHTTPError is a non-2xx answer from the NiFi REST API.
type nifiHTTPError struct {
	Method, Path string
	Code         int
}

func (e *nifiHTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Code)
}

// restNiFiAPI is nifiAPI over NiFi's REST API.
type restNiFiAPI struct {
	http  *http.Client
	base  string
	token string
}

func (c *restNiFiAPI) login(ctx context.Context, user, password string) error {
	form := url.Values{"username": {user}, "password": {password}}
	body, err := c.do(ctx, http.MethodPost, "/access/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("logging in to NiFi: %w", err)
	}
	c.token = strings.TrimSpace(string(body))
	return nil
}

func (c *restNiFiAPI) ListNodes(ctx context.Context) ([]nifiNode, error) {
	body, err := c.do(ctx, http.MethodGet, "/controller/cluster", "", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Cluster struct {
			Nodes []nifiNode `json:"nodes"`
		} `json:"cluster"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding cluster nodes: %w", err)
	}
	return resp.Cluster.Nodes, nil
}

func (c *restNiFiAPI) DeleteNode(ctx context.Context, nodeID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/controller/cluster/nodes/"+url.PathEscape(nodeID), "", nil)
	return err
}

// SetNodeStatus requests a node connection state change - DISCONNECTING or OFFLOADING, the two
// states NIFI-248 B4 needs (confirmed live against NiFi 2.12: no revision is needed on this
// endpoint, unlike most of NiFi's REST API). DISCONNECTING completes synchronously; OFFLOADING
// only starts offloading and must be polled (ListNodes) until the node's Status is OFFLOADED.
func (c *restNiFiAPI) SetNodeStatus(ctx context.Context, nodeID, status string) error {
	type nodeStatusUpdate struct {
		Node struct {
			NodeID string `json:"nodeId"`
			Status string `json:"status"`
		} `json:"node"`
	}
	var req nodeStatusUpdate
	req.Node.NodeID = nodeID
	req.Node.Status = status
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, "/controller/cluster/nodes/"+url.PathEscape(nodeID), "application/json", bytes.NewReader(body))
	return err
}

func (c *restNiFiAPI) do(ctx context.Context, method, path, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &nifiHTTPError{Method: method, Path: path, Code: resp.StatusCode}
	}
	return data, nil
}
