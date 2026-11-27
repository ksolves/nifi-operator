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
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/certmetrics"
)

// nifiClusterFinalizer ensures the reconciler gets a final pass on deletion. Owner-reference
// garbage collection removes every generated object, but NiFi's own coordinator and primary-node
// Leases carry no owner, so reconcileDelete removes them. Component-state
// ConfigMaps are kept on purpose - see cleanupCoordinationState.
const nifiClusterFinalizer = "nificluster.nifi.ksolves.com/finalizer"

// degradedConditionType is True while any node group fails to reconcile.
const degradedConditionType = "Degraded"

// scaleDownBlockedConditionType is only present (True) while a scale-down step cannot confirm a
// node has offloaded its data - otherwise invisible except as a transient reconcile error or a
// silent hold (NIFI-248 PR #210 review).
const scaleDownBlockedConditionType = "ScaleDownBlocked"

// reasonNodeLimitExceeded is the Degraded reason while the cluster asks for more than
// MaxClusterNodes nodes.
const reasonNodeLimitExceeded = "NodeLimitExceeded"

// NiFiClusterReconciler reconciles a NiFiCluster object.
type NiFiClusterReconciler struct {
	client.Client
	// APIReader reads directly from the API server, bypassing the manager's informer cache.
	// reconcileStatefulSet uses it (never the cached client.Client) specifically for its
	// existing-vs-desired comparison: the cached client can briefly lag behind a write this
	// same reconciler just made (informer propagation delay), and reading a stale pre-write
	// snapshot right after writing produces a self-sustaining update loop - confirmed live via
	// debug logging, oscillating between "has the hash" and "doesn't" every few milliseconds
	// even though nothing was actually changing.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// Recorder emits Kubernetes Events on NiFiClusters (e.g. for a pending coordination
	// migration). Optional: nil disables Events.
	Recorder record.EventRecorder
	// NiFiSCC is the OpenShift SecurityContextConstraints the NiFi pods' ServiceAccount is bound
	// to (via its built-in system:openshift:scc:<name> ClusterRole). Empty disables the binding,
	// e.g. on non-OpenShift clusters.
	NiFiSCC string
	// NoFSGroup leaves the NiFi pods' fsGroup unset, for platforms that assign one themselves and
	// reject a fixed one (e.g. OpenShift's restricted-v2). Otherwise they get FSGroup.
	NoFSGroup bool
	// FSGroup is the NiFi pods' fsGroup unless NoFSGroup is set; 0 (the root group) by default.
	FSGroup int64
	// RunAsUser is the NiFi pods' UID, with group 0 (--nifi-run-as-user); nil sets neither, for
	// platforms that assign the UID themselves.
	RunAsUser *int64
	// CSIDriverServiceAccount is the TLS CSI driver's ServiceAccount. For every TLS cluster the
	// operator grants it `get` on that cluster's CA Secret only (see reconcileCSICAAccess), so the
	// driver needs no cluster-wide Secret access. An empty Name disables the grant.
	CSIDriverServiceAccount types.NamespacedName
	// NewNiFiAPI, when set, replaces the NiFi REST client used to remove departed nodes (tests).
	NewNiFiAPI func(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (nifiAPI, error)

	// templateHashCache remembers, per StatefulSet, the templateHashAnnotation value and replica
	// count we last successfully wrote - so a reconcile can decide "is this up to date" from our
	// own memory of what we wrote, instead of reading the StatefulSet back and comparing. Reading
	// it back races with Kubernetes' own built-in StatefulSet controller, which independently
	// writes to the same object (confirmed live via .metadata.managedFields showing two distinct
	// field managers, kube-controller-manager and ours) - a read landing between its writes can
	// look like our own last update never took effect, causing us to update again forever. The
	// cache is best-effort: it starts empty after an operator restart, so that first reconcile
	// for a given StatefulSet still falls back to comparing against the server; every reconcile
	// after that trusts its own memory instead.
	//
	// Keyed by name+namespace only, which is NOT enough on its own: a NiFiCluster deleted and
	// recreated with the same name/namespace and identical config (so the same computed template
	// hash/replicas) would make a stale cache entry from before the delete look like a match,
	// even though the real StatefulSet is gone. stsCacheEntry.clusterUID (below) is what actually
	// prevents this - a recreated CR always gets a new UID, so the entry's stored UID stops
	// matching and the cache correctly falls through to a real check instead of a false "already
	// done". Confirmed live: without the UID check, the operator looped forever logging
	// "StatefulSet not found" after exactly this delete+recreate sequence, since the cache's
	// early-return skipped ever creating the replacement - only clearable by restarting the
	// operator (this cache is pure in-memory, so a fresh process starts empty).
	templateHashCache sync.Map // client.ObjectKey -> stsCacheEntry

	// offloadStartedCache remembers, per pod (namespace/name - not name alone, which two
	// NiFiClusters in different namespaces could share), when confirmOffloaded first started
	// driving that pod's node through disconnect+offload. Two jobs: (1) a Warning only fires once
	// a node has genuinely been stuck OFFLOADING past offloadStuckTimeout, not on every requeue of
	// a normal, still-progressing offload (NIFI-248 PR #210 review); (2) membership in this cache
	// is reconnectStrandedNodes' own safety scope - it only ever touches a pod this operator
	// itself put into offload, never any other DISCONNECTED/OFFLOADING/OFFLOADED pod NiFi happens
	// to report (a coordinator-loss event, a slow-starting node, or an admin's own manual
	// disconnect are never in this cache, so none of them are ever touched - PR #210 review).
	// Reset to time.Now() every time a fresh offload attempt starts (confirmOffloaded, at
	// reasonOffloadStarted) so a stale entry from an earlier, cancelled attempt against the same
	// pod can never make a brand new attempt look instantly stuck, and deleted once that pod is
	// reconnected (reconnectStrandedNodes) or its offload finishes or the pod stops existing
	// (confirmOffloaded). Best-effort like templateHashCache: starts empty after an operator
	// restart, which only delays the first Warning for an offload already running at that point,
	// and means a node already stranded before the restart is not auto-reconnected afterward -
	// safer than guessing, given nothing then distinguishes it from a coordinator-loss event.
	offloadStartedCache sync.Map // types.NamespacedName -> time.Time

	// NiFiStatusInterval is how often each cluster's NiFi REST API is polled for status;
	// 0 means DefaultNiFiStatusInterval.
	NiFiStatusInterval time.Duration

	// OffloadThroughputBytesPerSecond is the offload rate assumed when warning that a scale-down
	// may not offload a node within its grace period; 0 means
	// DefaultOffloadThroughputBytesPerSecond.
	OffloadThroughputBytesPerSecond int64

	// AllowCustomServiceAccountBinding is the operator admin's half of the opt-in that binds the
	// coordination Role to node groups' own ServiceAccounts; the cluster's
	// coordination.bindPodServiceAccounts is the other half.
	AllowCustomServiceAccountBinding bool
	// lastRBACCheck holds, per cluster UID, the last coordination RBAC check (rbacCheck).
	lastRBACCheck sync.Map
	// lastNiFiPoll throttles those polls per cluster.
	lastNiFiPoll sync.Map // types.UID -> time.Time
}

// stsCacheEntry is the cached record of a StatefulSet update reconcileStatefulSet knows it
// applied. clusterUID ties the entry to the specific NiFiCluster that produced it - see the
// templateHashCache field comment for why this is required, not just a nice-to-have. generation
// is the StatefulSet's metadata.generation right after that write: the API server bumps it on
// every spec change, so a different value means someone else changed the spec since.
// Status writes by the StatefulSet controller leave it alone.
type stsCacheEntry struct {
	hash       string
	metaHash   string // labels and annotations, see metadataHash
	replicas   int32
	clusterUID types.UID
	generation int64
	// pvcRetention is outside the pod template and metadata, so it needs its own key: otherwise a
	// changed retention policy alone would hit the cache and never reach the StatefulSet.
	pvcRetention appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy
}

// delete on nificlusters is never called: like delete on nifinodegroups below, it is there for
// OpenShift's OwnerReferencesPermissionEnforcement, as every owned object references the cluster.
// +kubebuilder:rbac:groups=nifi.ksolves.com,resources=nificlusters,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=nifi.ksolves.com,resources=nificlusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nifi.ksolves.com,resources=nificlusters/finalizers,verbs=update
// delete on nifinodegroups is never used, but OpenShift's OwnerReferencesPermissionEnforcement
// admission plugin only lets a client set a blocking owner reference on an object it may delete.
// +kubebuilder:rbac:groups=nifi.ksolves.com,resources=nifinodegroups,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=nifi.ksolves.com,resources=nifinodegroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//
// The leases rule and the configmaps/serviceaccounts verbs above are what the operator grants the
// NiFi pods through their Role (see nifiCoordinationRules); Kubernetes only lets it create that
// Role because it holds every permission in it itself. Likewise binding NiFi pods to
// system:openshift:scc:<name> (see buildNiFiSCCRoleBinding) needs "use" on that SCC: the chart
// adds that rule only for the SCC named in nifiPods.openshiftSCC, so it has no marker here.

// Reconcile drives a NiFiCluster towards its desired state: for every node group it ensures a
// ConfigMap, a headless Service, a client Service and a StatefulSet exist and match the spec,
// then reflects observed StatefulSet status back onto NiFiCluster.Status.
// Reconcile delegates to reconcile and, on error, also emits a Warning Event on the NiFiCluster
// - every return path inside reconcile wraps its own error with fmt.Errorf, so this one
// wrapper covers all of them rather than hand-adding an r.event call at each return site. The
// object passed to r.event is built from req alone (not a freshly re-Get'd NiFiCluster), since the
// error may be from the Get itself; the Event is still correctly attributed by namespace/name.
func (r *NiFiClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if err != nil {
		r.recordReconcileError(req, err)
	}
	return result, err
}

// recordReconcileError emits reasonReconcileError for a failed Reconcile. Takes req rather than a
// *NiFiCluster so it works even when the failure was Reconcile's own initial Get - the Event is
// still correctly attributed by namespace/name without needing the object to have been fetched.
func (r *NiFiClusterReconciler) recordReconcileError(req ctrl.Request, err error) {
	r.event(
		&nifiv1alpha1.NiFiCluster{ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: req.Namespace}},
		corev1.EventTypeWarning, reasonReconcileError, err.Error(),
	)
}

func (r *NiFiClusterReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &nifiv1alpha1.NiFiCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// NiFiNodeGroup events map here too (clusterOfNodeGroup): tell their authors.
			return ctrl.Result{}, r.markNodeGroupsWithoutCluster(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, fmt.Errorf("getting NiFiCluster: %w", err)
	}
	dropNodesWithoutID(&cluster.Status)

	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster)
	}

	if !controllerutil.ContainsFinalizer(cluster, nifiClusterFinalizer) {
		controllerutil.AddFinalizer(cluster, nifiClusterFinalizer)
		if err := r.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// The Update itself emits a watch event, which triggers the next reconcile.
		return ctrl.Result{}, nil
	}

	// NiFiNodeGroup objects join the inline node groups, in memory only - every step below
	// sees one list. Nothing below writes the NiFiCluster's spec, and its status writes go through
	// updateClusterStatus, which keeps the merged list.
	nodeGroupObjects, err := r.listNodeGroupObjects(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	conflicts := mergeNodeGroups(cluster, nodeGroupObjects)

	overLimit := r.enforceNodeLimit(cluster)

	pruneResult, err := r.pruneRemovedNodeGroups(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("pruning removed node groups: %w", err)
	}

	if clusterNeedsTLS(cluster) {
		if err := r.reconcileTLSCA(ctx, cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling TLS CA: %w", err)
		}
		if err := r.reconcileTLSCAExpiry(ctx, cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling TLS CA expiry: %w", err)
		}
	}
	if err := r.reconcileCSICAAccess(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling CSI driver CA access: %w", err)
	}

	if err := r.reconcileLoginSecrets(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileNiFiRBAC(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling NiFi pod RBAC: %w", err)
	}

	if err := r.reconcileSensitivePropsSecret(ctx, cluster); err != nil {
		if errors.Is(err, errConflictingLegacySensitiveKeys) {
			msg := "The node groups of this cluster were created as separate NiFi clusters with different " +
				"sensitive-props keys, so they cannot be merged into one cluster automatically. Remove all but " +
				"one node group (or recreate the others with empty data) before the coordination migration."
			if changed, err := r.setCondition(ctx, cluster, coordinationConditionType, metav1.ConditionFalse,
				reasonConflictingSensitivePropKey, msg); err != nil {
				return ctrl.Result{}, err
			} else if changed {
				r.event(cluster, corev1.EventTypeWarning, reasonConflictingSensitivePropKey, msg)
			}
			return pruneResult, nil
		}
		return ctrl.Result{}, fmt.Errorf("reconciling sensitive-props Secret: %w", err)
	}

	proceed, result, err := r.reconcileCoordinationMigration(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling coordination migration: %w", err)
	}
	if !proceed {
		return soonerResult(result, pruneResult), nil
	}

	statuses, failed, reconcileErrs, waitingOffload := r.reconcileNodeGroups(ctx, cluster, overLimit)

	if err := r.reconcileExposure(ctx, cluster); err != nil {
		reconcileErrs = append(reconcileErrs, fmt.Errorf("reconciling exposure: %w", err))
	}

	// NiFi's own view of the cluster; a failed poll only sets a condition.
	poll := r.pollNiFiStatus(ctx, cluster, readyReplicas(statuses))
	markStaleIdentityRemoval(cluster, poll)

	reconcileErrs = append(reconcileErrs, r.writeStatuses(ctx, cluster, statuses, failed, poll)...)

	var removalResult ctrl.Result
	if cluster.Status.NodeRemovalPending && len(reconcileErrs) == 0 {
		var err error
		if removalResult, err = r.reconcileNodeRemoval(ctx, cluster); err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("removing departed NiFi nodes: %w", err))
		}
	}

	if len(reconcileErrs) == 0 {
		followUpResult, errs := r.reconcileFollowUps(ctx, cluster)
		reconcileErrs = append(reconcileErrs, errs...)
		removalResult = soonerResult(removalResult, followUpResult)
	}
	// Last, so a failure here (an owner reference or status write on a NiFiNodeGroup) never holds
	// up node removal or the follow-ups above.
	if err := r.syncNodeGroupObjects(ctx, cluster, nodeGroupObjects, conflicts, statuses); err != nil {
		reconcileErrs = append(reconcileErrs, err)
	}

	if err := kerrors.NewAggregate(reconcileErrs); err != nil {
		return ctrl.Result{}, err
	}

	removalResult = soonerResult(removalResult, pruneResult)
	if waitingOffload {
		removalResult = soonerResult(removalResult, ctrl.Result{RequeueAfter: offloadRequeueInterval})
	}
	logger.V(1).Info("reconciled NiFiCluster", "name", cluster.Name)
	// Keep polling NiFi's status even when nothing else changes.
	return soonerResult(removalResult, ctrl.Result{RequeueAfter: r.nifiStatusInterval()}), nil
}

// reconcileNodeGroups reconciles every node group. Fail-safe, not fail-fast: one broken node group
// must not block status from reflecting every other node group that reconciled fine, nor stop them
// from being reconciled at all. Every group gets a chance; errors are collected and returned
// together (which still requeues this NiFiCluster via controller-runtime's normal error-retry
// path).
func (r *NiFiClusterReconciler) reconcileNodeGroups(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, overLimit bool,
) (statuses []nifiv1alpha1.NodeGroupStatus, failed []string, errs []error, waitingOffload bool) {
	statuses = make([]nifiv1alpha1.NodeGroupStatus, 0, len(cluster.Spec.NodeGroups))
	for i := range cluster.Spec.NodeGroups {
		nodeGroup := &cluster.Spec.NodeGroups[i]

		sts, waiting, err := r.reconcileNodeGroup(ctx, cluster, nodeGroup, overLimit)
		if err != nil {
			errs = append(errs, fmt.Errorf("reconciling node group %q: %w", nodeGroup.Name, err))
			// A failed node group still counts, as 0 ready, so status never reports the cluster
			// as healthy or the group's nodes as gone.
			statuses = append(statuses, nifiv1alpha1.NodeGroupStatus{Name: nodeGroup.Name, Replicas: nodeGroup.Replicas})
			failed = append(failed, nodeGroup.Name)
			continue
		}
		waitingOffload = waitingOffload || waiting

		status := nodeGroupStatus(nodeGroup, sts)
		// Over the limit, this group's StatefulSet was capped at its live replica count rather
		// than the (over-limit) desired one (capReplicasOverLimit): report that real count, not
		// the desired one the operator is already refusing to honor (NIFI-248 B8(a)/B1(b)).
		if overLimit && sts.Spec.Replicas != nil {
			status.Replicas = *sts.Spec.Replicas
		}
		statuses = append(statuses, status)
	}
	return statuses, failed, errs, waitingOffload
}

// soonerResult returns whichever of a and b asks to be requeued sooner (no requeue counts as never).
// writeStatuses writes the cluster's status, returning what failed.
func (r *NiFiClusterReconciler) writeStatuses(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, statuses []nifiv1alpha1.NodeGroupStatus, failed []string,
	poll *nifiPoll,
) []error {
	var errs []error
	if err := r.updateStatus(ctx, cluster, statuses, failed, poll); err != nil {
		errs = append(errs, fmt.Errorf("updating status: %w", err))
		// The retry (usually a conflict) must poll again, not wait out the interval with the
		// result of this poll lost.
		r.lastNiFiPoll.Delete(cluster.UID)
	}
	return errs
}

// enforceNodeLimit reports whether the merged node groups ask for more than MaxClusterNodes nodes
// in total. The webhook and the CRD keep a cluster within the limit, but /scale on a NiFiNodeGroup
// (kubectl scale, an autoscaler) bypasses the webhook, and the webhook can be disabled.
//
// Reconciliation keeps running even over the limit (NIFI-248 B1(b)): every managed object (TLS,
// RBAC, ConfigMaps, Secrets, the NiFiNodeGroups' own status) stays in sync with its own spec, and
// a scale-down is still stepped one node at a time. The one thing that never happens while over
// the limit is any node group's StatefulSet growing past its current live replica count
// (capReplicasOverLimit, called from reconcileNodeGroup) - so the limit still holds without
// freezing every other part of reconciliation too, which used to leave an unrelated manual
// StatefulSet or ConfigMap edit unreverted for as long as the cluster stayed over the limit.
// updateStatus sets the Degraded(NodeLimitExceeded) condition itself, from the same total.
func (r *NiFiClusterReconciler) enforceNodeLimit(cluster *nifiv1alpha1.NiFiCluster) (overLimit bool) {
	prev := apimeta.FindStatusCondition(cluster.Status.Conditions, degradedConditionType)
	wasOver := prev != nil && prev.Reason == reasonNodeLimitExceeded
	total := clusterReplicas(cluster)
	overLimit = total > nifiv1alpha1.MaxClusterNodes
	if overLimit && !wasOver {
		r.event(cluster, corev1.EventTypeWarning, reasonNodeLimitExceeded, nodeLimitMessage(total))
	}
	return overLimit
}

// capReplicasOverLimit keeps a node group's StatefulSet from growing while the cluster is over
// MaxClusterNodes (NIFI-248 B1(b)). A decrease - a legitimate step down, even starting from over
// the limit - is never touched, since stepScaleDown (called right after this) still needs to see
// it to pace it one node at a time. An increase, including a brand new node group's first-ever
// StatefulSet (which has no live replicas yet), is held at the StatefulSet's current live replica
// count (0 for one that does not exist yet), so no node this group does not already run ever
// starts while the cluster is over the limit.
func (r *NiFiClusterReconciler) capReplicasOverLimit(ctx context.Context, sts *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(sts), existing); err != nil {
		if apierrors.IsNotFound(err) {
			*sts.Spec.Replicas = 0
			return nil
		}
		return fmt.Errorf("getting StatefulSet to cap it at the node limit: %w", err)
	}
	current := int32(0)
	if existing.Spec.Replicas != nil {
		current = *existing.Spec.Replicas
	}
	if *sts.Spec.Replicas > current {
		*sts.Spec.Replicas = current
	}
	return nil
}

// nodeLimitMessage is the Degraded message while the cluster asks for total nodes, over the limit.
func nodeLimitMessage(total int32) string {
	return fmt.Sprintf("The node groups ask for %d NiFi nodes in total; at most %d are allowed. No node group's "+
		"StatefulSet is scaled up any further until the total is back within the limit; everything else "+
		"keeps reconciling normally.", total, nifiv1alpha1.MaxClusterNodes)
}

// reconcileFollowUps runs the steps that need everything else reconciled first and only set
// conditions: the coordination RBAC check.
func (r *NiFiClusterReconciler) reconcileFollowUps(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (ctrl.Result, []error) {
	var result ctrl.Result
	var errs []error
	rbacResult, err := r.reconcileCoordinationRBACCondition(ctx, cluster)
	if err != nil {
		errs = append(errs, fmt.Errorf("checking coordination RBAC: %w", err))
	}
	return soonerResult(result, rbacResult), errs
}

func soonerResult(a, b ctrl.Result) ctrl.Result {
	switch {
	case a.RequeueAfter == 0:
		return b
	case b.RequeueAfter == 0 || a.RequeueAfter <= b.RequeueAfter:
		return a
	}
	return b
}

// pruneRemovedNodeGroups deletes every generated object still labeled for a node group that no
// longer exists in cluster.Spec.NodeGroups. Reconciling only ever creates/updates objects for
// the node groups currently in the spec, so nothing else does this: without it, removing a
// NodeGroup from the spec would silently leak its StatefulSet/Services/ConfigMap/Secret/PDB
// until the entire NiFiCluster is deleted (the owner-reference cascade only fires then).
//
// While a removed node group drains, the result asks to look again soon: pods are not watched, so
// nothing else would before the next status poll.
func (r *NiFiClusterReconciler) pruneRemovedNodeGroups(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (ctrl.Result, error) {
	current := make(map[string]bool, len(cluster.Spec.NodeGroups))
	for _, nodeGroup := range cluster.Spec.NodeGroups {
		current[nodeGroup.Name] = true
	}

	selector := client.MatchingLabels{
		labelAppManagedBy: managedByValue,
		labelAppInstance:  cluster.Name,
	}

	// A removed node group's StatefulSet goes first, one pod at a time: deleting it
	// outright would decommission every node of the group at once. Until it is gone, the group
	// keeps its Services, ConfigMap, Secret and PDB, which its remaining nodes need to offload.
	draining, err := r.drainRemovedStatefulSets(ctx, cluster, selector, current)
	if err != nil {
		return ctrl.Result{}, err
	}
	keep := maps.Clone(current)
	maps.Copy(keep, draining)

	lists := []client.ObjectList{
		&appsv1.StatefulSetList{},
		&corev1.ServiceList{},
		&corev1.ConfigMapList{},
		&corev1.SecretList{},
		&policyv1.PodDisruptionBudgetList{},
	}
	for _, list := range lists {
		if err := r.pruneList(ctx, cluster.Namespace, cluster.Name, selector, keep, list); err != nil {
			return ctrl.Result{}, err
		}
	}
	if len(draining) > 0 {
		return ctrl.Result{RequeueAfter: drainRequeueInterval}, nil
	}
	return ctrl.Result{}, nil
}

// drainRequeueInterval is how soon a cluster with a draining removed node group is looked at again.
const drainRequeueInterval = 5 * time.Second

// drainRemovedStatefulSets scales the StatefulSet of every removed node group down one replica
// per pass, like a scale-down (stepScaleDown): the next pod goes only once the previous one is
// gone, so each node's preStop offload runs on its own. It returns the node groups still
// draining; a StatefulSet at zero replicas with no pod left is not draining and gets pruned. The
// StatefulSet status change on each pod's deletion re-queues the cluster through Owns().
func (r *NiFiClusterReconciler) drainRemovedStatefulSets(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, selector client.MatchingLabels, current map[string]bool,
) (map[string]bool, error) {
	list := &appsv1.StatefulSetList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace), selector); err != nil {
		return nil, fmt.Errorf("listing StatefulSets: %w", err)
	}
	draining := map[string]bool{}
	for i := range list.Items {
		sts := &list.Items[i]
		group, present := sts.Labels[labelNodeGroup]
		if !present || current[group] || !metav1.IsControlledBy(sts, cluster) || !sts.DeletionTimestamp.IsZero() {
			continue
		}
		existing := &appsv1.StatefulSet{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(sts), existing); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("getting StatefulSet %q of removed node group: %w", sts.Name, err)
		}
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(existing.Namespace), client.MatchingLabels(existing.Spec.Selector.MatchLabels)); err != nil {
			return nil, fmt.Errorf("listing pods of removed node group %q: %w", group, err)
		}
		replicas := int32(0)
		if existing.Spec.Replicas != nil {
			replicas = *existing.Spec.Replicas
		}
		if replicas == 0 && len(pods.Items) == 0 {
			continue
		}
		draining[group] = true
		base := existing.DeepCopy()
		// Scaling down applies whenScaled, but removing a node group is deleting its StatefulSet:
		// the PVCs must get whenDeleted, as they did before draining existed.
		policy := existing.Spec.PersistentVolumeClaimRetentionPolicy
		alignPolicy := policy != nil && policy.WhenScaled != policy.WhenDeleted
		if alignPolicy {
			policy.WhenScaled = policy.WhenDeleted
		}
		// Same confirmed-offload gate as stepScaleDown (NIFI-248 B4): removing this node group one
		// node at a time still deletes each node's PVC the moment it is gone, so that node must be
		// confirmed OFFLOADED first, not just absent from the pod list.
		next := replicas
		if replicas > 0 && int32(len(pods.Items)) <= replicas { //nolint:gosec // pod count of one StatefulSet
			target := replicas - 1
			targetPodName := fmt.Sprintf("%s-%d", existing.Name, target)
			offloaded, err := r.confirmOffloaded(ctx, cluster, targetPodName, podNamed(pods.Items, targetPodName))
			if err != nil {
				return nil, err
			}
			if offloaded {
				next = target
			}
		}
		if next == replicas && !alignPolicy {
			continue
		}
		existing.Spec.Replicas = &next
		if err := r.Patch(ctx, existing, client.MergeFrom(base)); err != nil {
			return nil, fmt.Errorf("scaling down StatefulSet %q of removed node group: %w", existing.Name, err)
		}
		if next < replicas {
			r.event(cluster, corev1.EventTypeNormal, reasonNodeGroupDraining, fmt.Sprintf(
				"node group %q was removed: removing its pod %s-%d (%d left) before deleting its StatefulSet",
				group, existing.Name, next, next))
		}
	}
	return draining, nil
}

// pruneList deletes every item in list (after populating it via List) whose node-group label
// isn't in current. Uses apimeta.ExtractList to walk items generically across the different
// concrete List types (StatefulSetList, ServiceList, ...), each of which only exposes its own
// typed Items field, not a common interface.
func (r *NiFiClusterReconciler) pruneList(
	ctx context.Context, namespace, owner string, selector client.MatchingLabels, current map[string]bool, list client.ObjectList,
) error {
	if err := r.List(ctx, list, client.InNamespace(namespace), selector); err != nil {
		return fmt.Errorf("listing %T: %w", list, err)
	}

	items, err := apimeta.ExtractList(list)
	if err != nil {
		return fmt.Errorf("extracting items from %T: %w", list, err)
	}

	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			continue
		}
		// nodeGroupName, present is false for a cluster-scoped object with no node-group
		// label at all (e.g. the TLS CA Secret, see tlsCASecretName) - those don't belong to
		// any node group in the first place, so "is this node group still in the spec" does
		// not apply and they must never be pruned here, regardless of what an empty-string
		// lookup in current would otherwise (incorrectly) return.
		nodeGroupName, present := obj.GetLabels()[labelNodeGroup]
		if !present || current[nodeGroupName] {
			continue
		}
		// Labels alone don't prove the operator made it: a user object that copied them (say
		// from `kubectl get -o yaml`) must never be deleted here.
		if ref := metav1.GetControllerOf(obj); ref == nil || ref.Kind != nifiClusterKind || ref.Name != owner {
			continue
		}
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting orphaned %T %q: %w", obj, obj.GetName(), err)
		}
		// Forget what was last written to a pruned StatefulSet, so re-adding the node group
		// creates it again instead of matching the stale cache entry.
		if _, isSts := obj.(*appsv1.StatefulSet); isSts {
			r.templateHashCache.Delete(client.ObjectKeyFromObject(obj))
		}
	}
	return nil
}

// reconcileNodeGroup ensures the ConfigMap, Services and StatefulSet for a single node group
// exist and match the desired spec.
func (r *NiFiClusterReconciler) reconcileNodeGroup(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup, overLimit bool,
) (sts *appsv1.StatefulSet, waitingOffload bool, err error) {
	layers, err := r.applyConfigOverrideSecrets(ctx, cluster, nodeGroup)
	if err != nil {
		return nil, false, fmt.Errorf("resolving ConfigOverrides Secret: %w", err)
	}
	if err := r.reconcileConfigSecret(ctx, cluster, nodeGroup, layers.secret); err != nil {
		return nil, false, fmt.Errorf("reconciling config Secret: %w", err)
	}
	if err := r.reconcileEntrypointSecret(ctx, cluster, nodeGroup); err != nil {
		return nil, false, fmt.Errorf("reconciling entrypoint Secret: %w", err)
	}

	cm := buildConfigMap(layers.cluster, layers.nodeGroup)
	if err := r.createOrUpdate(ctx, cluster, cm, func(existing client.Object) {
		existing.(*corev1.ConfigMap).Data = cm.Data
		existing.(*corev1.ConfigMap).Labels = cm.Labels
		existing.(*corev1.ConfigMap).Annotations = cm.Annotations
	}); err != nil {
		return nil, false, fmt.Errorf("reconciling ConfigMap: %w", err)
	}

	headless := buildHeadlessService(cluster, nodeGroup)
	if err := r.createOrUpdate(ctx, cluster, headless, func(existing client.Object) {
		svc := existing.(*corev1.Service)
		svc.Labels = headless.Labels
		svc.Annotations = headless.Annotations
		svc.Spec.Ports = headless.Spec.Ports
		svc.Spec.Selector = headless.Spec.Selector
	}); err != nil {
		return nil, false, fmt.Errorf("reconciling headless Service: %w", err)
	}

	clientSvc := buildClientService(cluster, nodeGroup)
	if err := r.createOrUpdate(ctx, cluster, clientSvc, func(existing client.Object) {
		svc := existing.(*corev1.Service)
		svc.Labels = clientSvc.Labels
		svc.Annotations = clientSvc.Annotations
		svc.Spec.Ports = clientSvc.Spec.Ports
		svc.Spec.Selector = clientSvc.Spec.Selector
		svc.Spec.Type = clientSvc.Spec.Type
	}); err != nil {
		return nil, false, fmt.Errorf("reconciling client Service: %w", err)
	}

	authSecretHash, err := r.referencedAuthSecretVersion(ctx, cluster)
	if err != nil {
		return nil, false, err
	}
	configHash := configMapHash(cm)
	if layers.version != "" {
		configHash = stringsHash([]string{configHash, layers.version})
	}
	built, err := buildStatefulSet(cluster, nodeGroup, configHash, authSecretHash, nifiPodServiceAccount(cluster),
		statefulSetOptions{NoFSGroup: r.NoFSGroup, FSGroup: r.FSGroup, RunAsUser: r.RunAsUser})
	if err != nil {
		return nil, false, fmt.Errorf("building StatefulSet: %w", err)
	}
	if overLimit {
		if err := r.capReplicasOverLimit(ctx, built); err != nil {
			return nil, false, err
		}
	}
	waitingOffload, err = r.stepScaleDown(ctx, cluster, built)
	if err != nil {
		return nil, false, err
	}
	if err := r.reconcileStatefulSet(ctx, cluster, built); err != nil {
		return nil, false, fmt.Errorf("reconciling StatefulSet: %w", err)
	}

	if err := r.reconcilePodDisruptionBudget(ctx, cluster, nodeGroup); err != nil {
		return nil, false, fmt.Errorf("reconciling PodDisruptionBudget: %w", err)
	}

	if err := r.reconcileTLSRotation(ctx, cluster, nodeGroup); err != nil {
		return nil, false, fmt.Errorf("reconciling TLS certificate rotation: %w", err)
	}

	observed := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(built), observed); err != nil {
		return nil, false, fmt.Errorf("getting reconciled StatefulSet: %w", err)
	}
	return observed, waitingOffload, nil
}

// reconcileSensitivePropsSecret ensures the cluster's Secret holding nifi.sensitive.props.key
// exists. Every node of a NiFi cluster must share one key to decrypt the one flow they all run.
// The Secret is deliberately create-only: rotating the key would break decryption of any
// sensitive property NiFi already encrypted with it, so an existing Secret is never touched.
//
// On first creation the key is adopted from the legacy per-node-group Secrets written by
// operator versions where every node group was its own NiFi cluster, so flows already on the
// retained PVCs still decrypt after the coordination migration. If those legacy Secrets disagree,
// errConflictingLegacySensitiveKeys is returned and nothing is created.
func (r *NiFiClusterReconciler) reconcileSensitivePropsSecret(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	key := client.ObjectKey{Name: sensitivePropsSecretName(cluster), Namespace: cluster.Namespace}
	existing := &corev1.Secret{}
	found, err := r.getOwned(ctx, cluster, key, existing)
	if errors.Is(err, errNotOwned) {
		// Not controlled by this cluster: adopt it only if it is the key a deleted cluster of the
		// same name left behind for its retained PVCs. Anything else is refused - a
		// pre-planted key would let its author decrypt every sensitive property.
		retained, rerr := r.retainedSensitivePropsSecret(ctx, cluster, existing)
		if rerr != nil {
			return rerr
		}
		if !retained {
			return fmt.Errorf("sensitive-props Secret: %w", err)
		}
		if existing.Labels[labelAppManagedBy] != managedByValue {
			// The cache strips the data of Secrets without the managed-by label (CacheByObject):
			// re-read it, or the Update below would write the key back empty.
			if err := r.secretReader().Get(ctx, key, existing); err != nil {
				return fmt.Errorf("re-reading retained sensitive-props Secret: %w", err)
			}
			if existing.Labels == nil {
				existing.Labels = map[string]string{}
			}
			maps.Copy(existing.Labels, clusterLabels(cluster))
		}
		if err := controllerutil.SetControllerReference(cluster, existing, r.Scheme); err != nil {
			return fmt.Errorf("adopting retained sensitive-props Secret: %w", err)
		}
		delete(existing.Annotations, retainedForAnnotation)
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("adopting retained sensitive-props Secret: %w", err)
		}
		return r.deleteLegacySensitivePropsSecrets(ctx, cluster)
	}
	if err != nil {
		return fmt.Errorf("sensitive-props Secret: %w", err)
	}
	if found {
		if err := r.ensureSecretMetadata(ctx, existing, clusterLabels(cluster), cluster.Spec.Annotations); err != nil {
			return err
		}
		return r.deleteLegacySensitivePropsSecrets(ctx, cluster)
	}

	adopted := ""
	for i := range cluster.Spec.NodeGroups {
		legacy := &corev1.Secret{}
		// Uncached: an older operator version may have left it without labels or owner, and the
		// cache strips the data of such Secrets (see CacheByObject).
		err := r.secretReader().Get(ctx, client.ObjectKey{
			Name: legacySensitivePropsSecretName(cluster, &cluster.Spec.NodeGroups[i]), Namespace: cluster.Namespace,
		}, legacy)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("getting legacy sensitive-props Secret: %w", err)
		}
		value := string(legacy.Data[sensitivePropsKeyDataKey])
		if value == "" {
			continue
		}
		if adopted != "" && adopted != value {
			return errConflictingLegacySensitiveKeys
		}
		adopted = value
	}

	if adopted == "" {
		generated, err := generateSensitivePropsKey()
		if err != nil {
			return err
		}
		adopted = generated
	}

	secret := buildSensitivePropsSecret(cluster, adopted)
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating sensitive-props Secret: %w", err)
	}
	return nil
}

// configOverrideLayers is the result of resolving a node group's ConfigOverrides against the
// Secrets they reference.
type configOverrideLayers struct {
	// cluster and nodeGroup are what buildConfigMap renders: their NifiProperties hold only the
	// keys whose winning value is a plain CR value. They are the inputs themselves (not copies)
	// when no Secret contributes anything.
	cluster   *nifiv1alpha1.NiFiCluster
	nodeGroup *nifiv1alpha1.NodeGroup
	// secret holds the keys whose winning value comes from a Secret. They go into the
	// operator-owned config Secret, never the ConfigMap.
	secret map[string]string
	// version identifies the referenced Secrets by UID/resourceVersion (never their contents),
	// "" when none is referenced and found. Folded into the config hash so rotating one rolls
	// the pods.
	version string
}

// applyConfigOverrideSecrets resolves the four ConfigOverrides layers, lowest precedence first:
// cluster plain < cluster Secret < node-group plain < node-group Secret. A more specific scope
// always beats a broader one, and within a scope the Secret beats plain values. Keys won by a
// Secret are split out of the plain maps so their values never land in the ConfigMap.
// A referenced Secret that doesn't exist yet is only logged, not treated as an error - the same
// leniency as referencedAuthSecretVersion, since GitOps tools/secret syncers routinely create
// Secrets after or alongside the NiFiCluster.
func (r *NiFiClusterReconciler) applyConfigOverrideSecrets(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup,
) (configOverrideLayers, error) {
	clusterSecret, err := r.getConfigOverrideSecret(ctx, cluster.Namespace, configOverridesSecretName(cluster.Spec.ConfigOverrides))
	if err != nil {
		return configOverrideLayers{}, err
	}
	nodeGroupSecret, err := r.getConfigOverrideSecret(ctx, cluster.Namespace, configOverridesSecretName(nodeGroup.ConfigOverrides))
	if err != nil {
		return configOverrideLayers{}, err
	}
	layers := configOverrideLayers{cluster: cluster, nodeGroup: nodeGroup}
	var versions []string
	for _, s := range []*corev1.Secret{clusterSecret, nodeGroupSecret} {
		if s != nil {
			versions = append(versions, string(s.UID)+"/"+s.ResourceVersion)
		}
	}
	if len(versions) > 0 {
		layers.version = stringsHash(versions)
	}
	if len(secretData(clusterSecret)) == 0 && len(secretData(nodeGroupSecret)) == 0 {
		return layers, nil
	}

	var clusterPlain, nodeGroupPlain map[string]string
	if co := cluster.Spec.ConfigOverrides; co != nil {
		clusterPlain = co.NifiProperties
	}
	if co := nodeGroup.ConfigOverrides; co != nil {
		nodeGroupPlain = co.NifiProperties
	}
	plain, fromSecret := map[string]string{}, map[string]string{}
	for _, layer := range []struct {
		values map[string]string
		secret bool
	}{
		{clusterPlain, false}, {secretData(clusterSecret), true}, {nodeGroupPlain, false}, {secretData(nodeGroupSecret), true},
	} {
		for k, v := range layer.values {
			if layer.secret {
				delete(plain, k)
				fromSecret[k] = v
			} else {
				delete(fromSecret, k)
				plain[k] = v
			}
		}
	}
	for k := range fromSecret {
		if IsReservedNifiPropertyKey(cluster, k) {
			delete(fromSecret, k)
		}
	}

	layers.cluster = cluster.DeepCopy()
	if layers.cluster.Spec.ConfigOverrides != nil {
		layers.cluster.Spec.ConfigOverrides.NifiProperties = nil
	}
	layers.nodeGroup = nodeGroup.DeepCopy()
	if layers.nodeGroup.ConfigOverrides == nil {
		layers.nodeGroup.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{}
	}
	layers.nodeGroup.ConfigOverrides.NifiProperties = plain
	layers.secret = fromSecret
	return layers, nil
}

// getConfigOverrideSecret returns the ConfigOverrides Secret secretName, or nil when secretName
// is empty or the Secret doesn't exist yet.
func (r *NiFiClusterReconciler) getConfigOverrideSecret(ctx context.Context, namespace, secretName string) (*corev1.Secret, error) {
	if secretName == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("ConfigOverrides Secret not found; its values are skipped until it exists",
				"secretName", secretName)
			return nil, nil
		}
		return nil, fmt.Errorf("getting ConfigOverrides Secret %q: %w", secretName, err)
	}
	return secret, nil
}

// secretData returns secret's data as strings, nil for a nil Secret.
func secretData(secret *corev1.Secret) map[string]string {
	if secret == nil {
		return nil
	}
	data := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		data[k] = string(v)
	}
	return data
}

// reconcileConfigSecret keeps the node group's operator-owned config Secret holding the
// Secret-sourced nifi.properties overrides, or removes it when there are none.
func (r *NiFiClusterReconciler) reconcileConfigSecret(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup, props map[string]string,
) error {
	if len(props) == 0 {
		return r.deleteOwned(ctx, cluster, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: configSecretName(cluster, nodeGroup), Namespace: cluster.Namespace},
		})
	}
	secret := buildConfigSecret(cluster, nodeGroup, props)
	return r.createOrUpdateOwned(ctx, cluster, secret, func(existing client.Object) {
		existing.SetLabels(secret.Labels)
		existing.SetAnnotations(secret.Annotations)
		existing.(*corev1.Secret).Data = secret.Data
	})
}

// reconcileEntrypointSecret keeps the node group's operator-owned entrypoint Secret current (see
// entrypointSecretName for why the entrypoint script lives in a Secret, not the ConfigMap).
func (r *NiFiClusterReconciler) reconcileEntrypointSecret(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup,
) error {
	secret := buildEntrypointSecret(cluster, nodeGroup)
	return r.createOrUpdateOwned(ctx, cluster, secret, func(existing client.Object) {
		existing.SetLabels(secret.Labels)
		existing.SetAnnotations(secret.Annotations)
		existing.(*corev1.Secret).Data = secret.Data
	})
}

// configOverridesSecretName nil-safely reads ConfigOverridesSpec.SecretName.
func configOverridesSecretName(co *nifiv1alpha1.ConfigOverridesSpec) string {
	if co == nil {
		return ""
	}
	return co.SecretName
}

// deleteLegacySensitivePropsSecrets removes the per-node-group sensitive-props Secrets once the
// cluster-wide Secret exists and no StatefulSet still runs the legacy layout that reads them.
func (r *NiFiClusterReconciler) deleteLegacySensitivePropsSecrets(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	legacySts, err := r.legacyStatefulSets(ctx, cluster)
	if err != nil {
		return err
	}
	if len(legacySts) > 0 {
		return nil
	}
	for i := range cluster.Spec.NodeGroups {
		if err := r.deleteOwned(ctx, cluster, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: legacySensitivePropsSecretName(cluster, &cluster.Spec.NodeGroups[i]), Namespace: cluster.Namespace,
		}}); err != nil {
			return fmt.Errorf("deleting legacy sensitive-props Secret: %w", err)
		}
	}
	return nil
}

// clusterNeedsTLS reports whether any node group in cluster has TLS enabled - the CA Secret
// (reconcileTLSCA) is only created when at least one node group actually needs it.
func clusterNeedsTLS(cluster *nifiv1alpha1.NiFiCluster) bool {
	for i := range cluster.Spec.NodeGroups {
		if cluster.Spec.NodeGroups[i].TLS != nil && cluster.Spec.NodeGroups[i].TLS.Enabled {
			return true
		}
	}
	return false
}

// retainedForAnnotation marks a sensitive-props Secret that the operator released from a deleted
// NiFiCluster so a same-name recreate can adopt it. Its value is the cluster name.
const retainedForAnnotation = "nifi.ksolves.com/retained-for"

// retainSensitivePropsSecret releases the cluster's sensitive-props Secret before the cluster is
// deleted, so garbage collection keeps it. The flow on the retained PVCs is encrypted with this key;
// a NiFiCluster recreated under the same name must be able to decrypt it. Delete the
// Secret by hand to discard the key along with the PVCs.
func (r *NiFiClusterReconciler) retainSensitivePropsSecret(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: sensitivePropsSecretName(cluster), Namespace: cluster.Namespace}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(secret, cluster) {
		return nil
	}
	refs := secret.OwnerReferences[:0]
	for _, ref := range secret.OwnerReferences {
		if ref.UID != cluster.UID {
			refs = append(refs, ref)
		}
	}
	secret.OwnerReferences = refs
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[retainedForAnnotation] = cluster.Name
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("retaining sensitive-props Secret: %w", err)
	}
	return nil
}

// deleteReleasedSensitivePropsSecret deletes cluster's sensitive-props Secret if an earlier
// reconcileDelete pass released it (retainSensitivePropsSecret) for PVCs that are now gone too.
func (r *NiFiClusterReconciler) deleteReleasedSensitivePropsSecret(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: sensitivePropsSecretName(cluster), Namespace: cluster.Namespace}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if secret.Annotations[retainedForAnnotation] != cluster.Name || metav1.GetControllerOf(secret) != nil {
		return nil
	}
	// The deletion cannot be undone: confirm against the API server that no PVC needs the key,
	// since the cache may not hold a PVC created a moment ago.
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.secretReader().List(ctx, pvcs, client.InNamespace(cluster.Namespace)); err != nil {
		return fmt.Errorf("listing PVCs: %w", err)
	}
	for i := range pvcs.Items {
		// A terminating PVC counts too: its PersistentVolume may be retained, data and all.
		if pvc := &pvcs.Items[i]; mayBeClusterPVC(cluster, pvc) && !deletedWithWorkload(pvc) {
			return nil
		}
	}
	if err := r.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting released sensitive-props Secret: %w", err)
	}
	return nil
}

// retainedSensitivePropsSecret reports whether secret is the key a deleted NiFiCluster of the same
// name released (retainSensitivePropsSecret) and that cluster's PVCs are still there to use it.
// Requiring the PVCs too means a Secret planted with a forged annotation alone isn't adopted for a
// brand-new cluster, which has no data that key could decrypt.
func (r *NiFiClusterReconciler) retainedSensitivePropsSecret(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, secret *corev1.Secret,
) (bool, error) {
	if secret.Annotations[retainedForAnnotation] != cluster.Name || metav1.GetControllerOf(secret) != nil {
		return false, nil
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(cluster.Namespace)); err != nil {
		return false, fmt.Errorf("listing PVCs: %w", err)
	}
	// Exact claim names (<template>-<statefulset>-<ordinal>), not a suffix: "data-x-foo-default-0"
	// of cluster "x", node group "foo-default" must not count for cluster "foo", node group
	// "default". Or labelled for this cluster: the key is kept for the PVCs of a node group removed
	// before the delete too (keepsDataOnDelete), and a recreate without that group must still adopt
	// it, or it could never start. Same match as keepsDataOnDelete, except that a PVC the
	// garbage collector will delete still counts here: adopting a key is never a loss. Only an
	// unlabelled claim (before 0.6.0) of a node group the recreated cluster lacks keeps a key it
	// cannot adopt.
	for j := range pvcs.Items {
		if pvcs.Items[j].DeletionTimestamp == nil && mayBeClusterPVC(cluster, &pvcs.Items[j]) {
			return true, nil
		}
	}
	return false, nil
}

// reconcileTLSCA ensures a Secret holding this NiFiCluster's self-signed TLS CA exists,
// generating one only the first time. Like reconcileSensitivePropsSecret, this is deliberately
// create-only: replacing an already-trusted CA would invalidate every leaf certificate the CSI
// driver has already issued from it until every pod is restarted and reissued a new one signed
// by the new CA - a coordinated rotation this reconciler does not attempt. One CA per
// NiFiCluster (see tlsCASecretName), shared by every node group in it, not one per node group.
func (r *NiFiClusterReconciler) reconcileTLSCA(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	key := client.ObjectKey{Name: tlsCASecretName(cluster), Namespace: cluster.Namespace}
	// Only a CA the cluster itself controls is trusted. A pre-planted Secret of this name would
	// otherwise sign every node certificate with a key someone else holds.
	existing := &corev1.Secret{}
	found, err := r.getOwned(ctx, cluster, key, existing)
	if err != nil {
		return fmt.Errorf("TLS CA Secret: %w", err)
	}
	if found {
		if len(existing.Data[TLSKeystorePasswordDataKey]) == 0 {
			// Backfill for a CA Secret created by an operator version before this field existed
			// (it used one hardcoded password for every cluster instead) - generating it once
			// here, the same as a brand-new CA, rather than leaving every such cluster on that
			// old shared value forever.
			password, err := r.resolveKeystorePassword(ctx, cluster)
			if err != nil {
				return fmt.Errorf("resolving TLS keystore password: %w", err)
			}
			patch := client.MergeFrom(existing.DeepCopy())
			if existing.Data == nil {
				existing.Data = map[string][]byte{}
			}
			existing.Data[TLSKeystorePasswordDataKey] = []byte(password)
			if err := r.Patch(ctx, existing, patch); err != nil {
				return fmt.Errorf("backfilling TLS keystore password: %w", err)
			}
		}
		return r.ensureSecretMetadata(ctx, existing, clusterLabels(cluster), cluster.Spec.Annotations)
	}

	keystorePassword, err := r.resolveKeystorePassword(ctx, cluster)
	if err != nil {
		return fmt.Errorf("resolving TLS keystore password: %w", err)
	}
	secret, err := buildTLSCASecret(cluster, keystorePassword)
	if err != nil {
		return fmt.Errorf("building TLS CA Secret: %w", err)
	}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating TLS CA Secret: %w", err)
	}
	return nil
}

// resolveKeystorePassword returns the password reconcileTLSCA stores alongside the CA - read from
// nodeGroups[0].TLS.KeystorePasswordSecretName (uniform across node groups, enforced by the
// webhook's validateUniformTLS) when the user set one, or freshly generated otherwise. Only called
// the one time reconcileTLSCA actually creates the CA Secret, so a Secret that doesn't exist yet
// correctly blocks cluster creation (the user asked for a specific password; silently generating
// one instead would use a password they never see, baked in forever).
func (r *NiFiClusterReconciler) resolveKeystorePassword(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (string, error) {
	secretName := ""
	if len(cluster.Spec.NodeGroups) > 0 && cluster.Spec.NodeGroups[0].TLS != nil {
		secretName = cluster.Spec.NodeGroups[0].TLS.KeystorePasswordSecretName
	}
	if secretName == "" {
		return generateRandomPassword()
	}
	secret := &corev1.Secret{}
	if err := r.secretReader().Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, secret); err != nil {
		return "", fmt.Errorf("getting keystorePasswordSecretName Secret %q: %w", secretName, err)
	}
	password := string(secret.Data[authSecretPasswordKey])
	if password == "" {
		return "", fmt.Errorf("secret %q has no non-empty %q key", secretName, authSecretPasswordKey)
	}
	return password, nil
}

// reconcileAuthSecret keeps the operator-synthesized login-credentials Secret (authSecretName) in
// sync with spec.authentication. Three cases:
//   - A user-supplied SecretName is set: that Secret is read directly by the pod (see
//     buildStatefulSet), never touched by the operator, so any operator-owned Secret is deleted.
//   - Literal Username/Password are set: the CR's own fields are the source of truth, so this is
//     deliberately create-or-update (not create-once) - editing the password in the CR should
//     actually change the running cluster's login, not silently do nothing.
//   - spec.authentication is left unset entirely: the operator defaults to a Secret-backed,
//     randomly-generated login shared by every node (see reconcileDefaultAuthSecret), rather than
//     leaving each node to fall back to NiFi's own per-node random single-user credentials.
func (r *NiFiClusterReconciler) reconcileAuthSecret(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	auth := cluster.Spec.Authentication

	if auth != nil && auth.SingleUser != nil && auth.SingleUser.SecretName != "" {
		// Only ever delete the operator's own Secret: a same-named Secret someone else created is
		// left alone.
		if err := r.deleteOwned(ctx, cluster, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: authSecretName(cluster), Namespace: cluster.Namespace},
		}); err != nil {
			return fmt.Errorf("deleting unused auth Secret: %w", err)
		}
		return nil
	}

	if auth == nil || auth.SingleUser == nil {
		return r.reconcileDefaultAuthSecret(ctx, cluster)
	}

	// createOrUpdateOwned, not createOrUpdate: a pre-existing Secret of this name that the
	// operator doesn't control is refused rather than adopted and overwritten.
	secret := buildAuthSecret(cluster, auth.SingleUser)
	return r.createOrUpdateOwned(ctx, cluster, secret, func(existing client.Object) {
		existing.(*corev1.Secret).Data = secret.Data
		existing.(*corev1.Secret).Labels = secret.Labels
		existing.(*corev1.Secret).Annotations = secret.Annotations
	})
}

// reconcileDefaultAuthSecret ensures the operator-generated default login Secret exists,
// generating one only the first time - create-once, like reconcileSensitivePropsSecret/
// reconcileTLSCA, since there's no CR field tracking this password for an update to follow, and
// silently rotating it on a later reconcile would just log out whoever is already using it.
func (r *NiFiClusterReconciler) reconcileDefaultAuthSecret(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	key := client.ObjectKey{Name: authSecretName(cluster), Namespace: cluster.Namespace}
	// An existing Secret is reused only when the operator owns it. A same-named Secret someone
	// else created would otherwise silently become the cluster's login.
	existing := &corev1.Secret{}
	found, err := r.getOwned(ctx, cluster, key, existing)
	if err != nil {
		return fmt.Errorf("default auth Secret: %w", err)
	}
	if found {
		return r.ensureSecretMetadata(ctx, existing, clusterLabels(cluster), cluster.Spec.Annotations)
	}

	secret, err := buildDefaultAuthSecret(cluster)
	if err != nil {
		return fmt.Errorf("building default auth Secret: %w", err)
	}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating default auth Secret: %w", err)
	}
	r.event(cluster, corev1.EventTypeNormal, "DefaultAuthSecretCreated",
		fmt.Sprintf("Generated a default login (Secret %q, username %q) - no spec.authentication was set",
			key.Name, defaultAuthUsername))
	return nil
}

// referencedAuthSecretVersion returns referencedAuthSecretHash of the Secret backing
// spec.authentication.singleUser: the user-managed one named by secretName, or the
// operator-owned authSecretName one synthesized from literal Username/Password. It returns ""
// when singleUser isn't set (the default login Secret is create-once, so it never needs to roll
// the pods). A missing Secret also yields "": the pods then fail with CreateContainerConfigError
// until it exists, and its creation triggers a reconcile through the Secret watches in
// SetupWithManager.
func (r *NiFiClusterReconciler) referencedAuthSecretVersion(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (string, error) {
	auth := cluster.Spec.Authentication
	if auth == nil || auth.SingleUser == nil {
		return "", nil
	}
	name := effectiveAuthSecretName(cluster)
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		log.FromContext(ctx).Info("authentication Secret not found; pods cannot start until it exists",
			"secretName", name)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting authentication Secret %q: %w", name, err)
	}
	return referencedAuthSecretHash(secret), nil
}

// referencesSecret reports whether cluster reads the user-managed Secret name: as its
// single-user login (spec.authentication.singleUser.secretName) or as a ConfigOverrides Secret,
// at cluster or node-group level.
func referencesSecret(cluster *nifiv1alpha1.NiFiCluster, name string) bool {
	if auth := cluster.Spec.Authentication; auth != nil && auth.SingleUser != nil && auth.SingleUser.SecretName == name {
		return true
	}
	if configOverridesSecretName(cluster.Spec.ConfigOverrides) == name {
		return true
	}
	for i := range cluster.Spec.NodeGroups {
		if configOverridesSecretName(cluster.Spec.NodeGroups[i].ConfigOverrides) == name {
			return true
		}
	}
	return false
}

// clustersReferencingSecret maps a Secret event to every NiFiCluster in the same namespace that
// reads it (see referencesSecret). Such Secrets are user-managed, not owned by the cluster, so
// Owns(&corev1.Secret{}) alone never sees their changes - and rotating one must roll the pods.
func (r *NiFiClusterReconciler) clustersReferencingSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	clusters := &nifiv1alpha1.NiFiClusterList{}
	if err := r.List(ctx, clusters, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing NiFiClusters for Secret event", "secret", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range clusters.Items {
		if referencesSecret(&clusters.Items[i], obj.GetName()) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&clusters.Items[i])})
		}
	}
	return requests
}

// stepScaleDown makes a scale-down remove one node at a time, and never lowers replicas to remove
// the next node until NiFi confirms the previous one has fully offloaded its queued data
// (NIFI-248 B4): Kubernetes' own PersistentVolumeClaimRetentionPolicy deletes a departed node's
// PVC once it is actually gone, and that write cannot be undone once made, so the confirmation has
// to come before this function lowers replicas, not after. See confirmOffloaded; this also keeps
// the StatefulSet's Parallel pod management from decommissioning several nodes onto each other at
// once, same as before. The StatefulSet's status change on a pod's deletion re-queues the cluster
// through Owns().
// waiting reports whether a scale-down step is being held pending an offload confirmation, so the
// caller can ask for a quick requeue (offloadRequeueInterval) instead of falling back to the
// default, much longer NiFi status poll interval - the same reason drainRemovedStatefulSets uses
// its own drainRequeueInterval instead of that default.
func (r *NiFiClusterReconciler) stepScaleDown(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, sts *appsv1.StatefulSet,
) (waiting bool, err error) {
	existing := &appsv1.StatefulSet{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(sts), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting StatefulSet for scale-down: %w", err)
	}
	if existing.Spec.Replicas == nil {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(sts.Namespace), client.MatchingLabels(sts.Spec.Selector.MatchLabels)); err != nil {
		return false, fmt.Errorf("listing pods for scale-down: %w", err)
	}
	if *sts.Spec.Replicas >= *existing.Spec.Replicas {
		// Scaling up, or no change under way: nothing to remove by lowering replicas. A previous
		// scale-down step may have disconnected/offloaded a node through the REST API while still
		// waiting for confirmation (so replicas was never actually lowered) - if that scale-down
		// was then cancelled by scaling back up, the node's pod is inside the desired range again
		// but stays disconnected from NiFi forever: NiFi's cluster protocol only lets an already-
		// connected node manage its own reconnection, so there is no API call that brings a fully
		// disconnected node back (confirmed live; NIFI-248 PR #210 review). Deleting the pod lets
		// it rejoin cleanly through NiFi's own startup sequence instead - safe even mid-transfer,
		// since any FlowFiles not yet offloaded simply stay on the pod's retained PVC and reload
		// when it restarts (confirmed live with real queued data, including force-killed mid-
		// offload). Only worth checking when some pod in range is not Ready: in the steady state
		// every pod is Ready and this never calls the NiFi API.
		if err := r.reconnectStrandedNodes(ctx, cluster, sts.Name, pods.Items, *existing.Spec.Replicas); err != nil {
			return false, err
		}
		return false, nil
	}
	current := *existing.Spec.Replicas
	if int32(len(pods.Items)) > current { //nolint:gosec // pod count of one StatefulSet
		// The previous step's pod is still terminating: hold before even looking at the next one.
		// Its own deletion re-queues the cluster through Owns(), same as before this fix.
		sts.Spec.Replicas = &current
		return false, nil
	}
	target := current - 1
	targetPodName := fmt.Sprintf("%s-%d", sts.Name, target)
	offloaded, err := r.confirmOffloaded(ctx, cluster, targetPodName, podNamed(pods.Items, targetPodName))
	if err != nil {
		return false, err
	}
	if !offloaded {
		sts.Spec.Replicas = &current
		return true, nil
	}
	sts.Spec.Replicas = &target
	return false, nil
}

// reconnectStrandedNodeGracePeriod is how long reconnectStrandedNodes waits after restarting a
// pod before it is willing to restart that same pod again: NiFi's own node list keeps the old,
// disconnected identity at that address until the fresh pod actually boots the JVM and
// reconnects, which takes longer than one reconcile - without this, the pod would be deleted
// again on every next reconcile, before it ever got the chance (confirmed live: 305 repeats in
// under a minute without this check - NIFI-248 PR #210 review).
const reconnectStrandedNodeGracePeriod = 3 * time.Minute

// reconnectStrandedNodes deletes the pod of a node, within the first replicaCount ordinals of
// stsName, that NiFi still shows as OFFLOADING/OFFLOADED/DISCONNECTED after being left behind by
// a scale-down step that was cancelled by scaling back up before it finished (NIFI-248 PR #210
// review) - and only such a node: membership in offloadStartedCache (set by confirmOffloaded the
// moment it starts actively driving a node through disconnect+offload, for this operator's own
// scale-down step) is the one and only signal this function ever acts on. It deliberately never
// just scans NiFi's own node list for any DISCONNECTED/OFFLOADING/OFFLOADED entry regardless of
// cause: a coordinator-loss event marks every node DISCONNECTED at once (the readiness probe
// checks connectedToCluster, so every pod goes NotReady together too) - treating all of them as
// "stranded" would force-delete the entire cluster in one pass, bypassing its PodDisruptionBudget;
// a node merely slow to start (a large flow, many NARs) would be deleted again on every pass
// before it ever got the chance to finish joining; and an admin's own deliberate manual disconnect
// would be silently reverted. None of those nodes was ever put into offload by this operator, so
// none of them is ever in the cache, and none of them is ever touched here (NIFI-248 PR #210
// review - found live after the first version of this function, which scanned unconditionally,
// shipped with exactly this gap). Never forces anything through: an unreachable API is skipped
// silently, same posture as confirmOffloaded, since this is a best-effort cleanup, not something a
// scale-down step waits on.
func (r *NiFiClusterReconciler) reconnectStrandedNodes(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, stsName string, pods []corev1.Pod, replicaCount int32,
) error {
	anyNotReady := false
	for ord := int32(0); ord < replicaCount; ord++ {
		if pod := podNamed(pods, fmt.Sprintf("%s-%d", stsName, ord)); pod == nil || !podReady(pod) {
			anyNotReady = true
			break
		}
	}
	if !anyNotReady {
		return nil
	}
	api, err := r.newNiFiAPI(ctx, cluster)
	if err != nil {
		return nil
	}
	nodes, err := api.ListNodes(ctx)
	if err != nil {
		return nil
	}
	for ord := int32(0); ord < replicaCount; ord++ {
		podName := fmt.Sprintf("%s-%d", stsName, ord)
		offloadKey := types.NamespacedName{Namespace: cluster.Namespace, Name: podName}
		if _, tracked := r.offloadStartedCache.Load(offloadKey); !tracked {
			// Never put into offload by this operator: not this function's business, whatever
			// NiFi currently reports for it.
			continue
		}
		for _, n := range nodes {
			p, _, _ := strings.Cut(n.Address, ".")
			if p != podName {
				continue
			}
			if n.Status != nifiStateOffloading && n.Status != nifiStateOffloaded && n.Status != nifiStateDisconnected {
				break
			}
			pod := podNamed(pods, podName)
			if pod == nil {
				break
			}
			if time.Since(pod.CreationTimestamp.Time) < reconnectStrandedNodeGracePeriod {
				// Just restarted for this exact reason: NiFi's own node list still shows the old,
				// disconnected identity at this address until the fresh pod actually boots and
				// reconnects (confirmed live: without this check, the pod was deleted again on
				// every next reconcile, before it ever got the chance - NIFI-248 PR #210 review).
				break
			}
			if pod.DeletionTimestamp != nil {
				// Already being deleted by a concurrent/retried reconcile of the same stranded
				// node: Delete on a pod mid-termination still returns success, not NotFound, so
				// without this check the event below would fire again for a restart that was
				// already underway (confirmed live - NIFI-248 PR #210 review).
				break
			}
			if err := r.Delete(ctx, pod); err != nil {
				if client.IgnoreNotFound(err) != nil {
					return fmt.Errorf("restarting stranded node %s (status %s) after its scale-down was cancelled: %w", podName, n.Status, err)
				}
				// Already deleted by a concurrent/retried reconcile of the same stranded node:
				// nothing was actually restarted here, so no event for it (NIFI-248 PR #210 review).
				break
			}
			// Reconnected now: this pod is no longer this operator's responsibility to track. If
			// a later scale-down ever touches it again, confirmOffloaded starts it fresh
			// (NIFI-248 PR #210 review).
			r.offloadStartedCache.Delete(offloadKey)
			r.event(cluster, corev1.EventTypeNormal, reasonReconnectingStrandedNode, fmt.Sprintf(
				"%s is back inside the desired node count but still %s from a cancelled scale-down; restarting it so it rejoins NiFi",
				podName, n.Status))
			break
		}
	}
	return nil
}

// offloadRequeueInterval is how soon a cluster holding a scale-down step on an offload
// confirmation is looked at again.
const offloadRequeueInterval = 5 * time.Second

// offloadStuckTimeout is how long a node may stay OFFLOADING before confirmOffloaded treats it
// as genuinely stuck (a Warning) rather than normally in progress (a Normal event).
const offloadStuckTimeout = 5 * time.Minute

// DefaultOffloadThroughputBytesPerSecond is the offload rate assumed when no
// --offload-throughput-bytes-per-second is given (10 MiB/s): used only to estimate how long an
// in-progress offload will take, for the OffloadStarted event - confirmOffloaded itself never
// relies on this estimate to decide anything.
const DefaultOffloadThroughputBytesPerSecond int64 = 10 << 20

// confirmOffloaded drives the NiFi node backing podName through disconnect and offload via the
// REST API directly (rather than relying on that node's own preStop hook, which only starts once
// the pod is already terminating - too late to still protect its PVC, see stepScaleDown) and
// reports whether it has reached OFFLOADED. Confirmed live against NiFi 2.12: DISCONNECTING
// completes synchronously; OFFLOADING only starts offloading and needs polling until the node's
// Status reaches OFFLOADED.
//
// Calls the NiFi API itself (ListNodes) rather than reading the cached cluster.Status.Nodes
// pollNiFiStatus already maintains: that poll skips the API call entirely while readyReplicas is
// 0, a condition about the cluster as a whole that has nothing to do with whether the one specific
// node being offloaded can be reached - and a node mid-scale-down otherwise has live peers, so this
// adds no real cost in practice, just one more API round trip during an active scale-down step.
//
// Never forces anything through: while the credentials needed to call the API are missing, the
// node cannot be reached, or it has not reached OFFLOADED yet, it reports false and the caller
// holds - including if NiFi itself leaves a node stuck OFFLOADING forever (a known NiFi bug,
// Apache NIFI-6189), in which case this cluster's scale-down simply stays held until a human
// intervenes in NiFi or scales back up.
//
// pod is the target pod itself (nil when it does not exist): when the node is not in NiFi's own
// view at all, a pod that was never Ready could never have joined, so there is nothing to offload
// (NIFI-248 PR #210 review: scale-down must not hang forever on a node that never joined); a
// currently-Ready pod might just not have propagated to NiFi's listing yet, so that case still
// holds.
func (r *NiFiClusterReconciler) confirmOffloaded(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, podName string, pod *corev1.Pod,
) (bool, error) {
	offloadKey := types.NamespacedName{Namespace: cluster.Namespace, Name: podName}
	api, err := r.newNiFiAPI(ctx, cluster)
	if errors.Is(err, errNoNiFiCredentials) {
		return false, r.blockScaleDown(ctx, cluster, "NoCredentials", fmt.Sprintf(
			"scale-down cannot confirm %s has offloaded its data: its login credentials Secret (%s) does not exist.",
			podName, effectiveAuthSecretName(cluster)))
	}
	if err != nil {
		return false, r.blockScaleDown(ctx, cluster, "APIUnreachable",
			fmt.Sprintf("scale-down cannot confirm %s has offloaded its data: %v", podName, err))
	}
	nodes, err := api.ListNodes(ctx)
	if err != nil {
		return false, r.blockScaleDown(ctx, cluster, "APIUnreachable",
			fmt.Sprintf("scale-down cannot confirm %s has offloaded its data: %v", podName, err))
	}

	var nodeID, state, queued string
	found := false
	for _, n := range nodes {
		if p, _, _ := strings.Cut(n.Address, "."); p == podName {
			nodeID, state, queued, found = n.NodeID, n.Status, n.Queued, true
			break
		}
	}
	if !found {
		r.offloadStartedCache.Delete(offloadKey)
		if pod == nil || !podReady(pod) {
			// Its pod does not exist, or has never become Ready (stuck Pending with no PV, or
			// crash-looping before NiFi ever started) - it could never have joined NiFi, so there
			// is nothing to offload. A node that genuinely just joined and whose listing simply
			// hasn't propagated to this poll yet would be Ready right now (NiFi only accepts
			// traffic from a node whose container passed its own readiness probe), so this does
			// not race with that case: only an actually-stuck node ever takes this path.
			return true, r.removeCondition(ctx, cluster, scaleDownBlockedConditionType)
		}
		// Ready, but not in NiFi's own view yet - the poll may not have caught up with this node
		// joining. Hold rather than guess either way.
		return false, r.removeCondition(ctx, cluster, scaleDownBlockedConditionType)
	}
	if state == nifiStateOffloaded {
		r.offloadStartedCache.Delete(offloadKey)
		return true, r.removeCondition(ctx, cluster, scaleDownBlockedConditionType)
	}

	if state == nifiStateOffloading {
		startedAt, _ := r.offloadStartedCache.LoadOrStore(offloadKey, time.Now())
		if elapsed := time.Since(startedAt.(time.Time)); elapsed > offloadStuckTimeout {
			r.event(cluster, corev1.EventTypeWarning, reasonOffloadStuck, fmt.Sprintf(
				"scale-down has been waiting for %s to finish offloading for over %s; it still holds %s queued - "+
					"this may be a stuck node (Apache NIFI-6189) rather than a slow one", podName, offloadStuckTimeout, queued))
		} else {
			r.event(cluster, corev1.EventTypeNormal, reasonOffloadInProgress, fmt.Sprintf(
				"scale-down is waiting for %s to finish offloading before removing it; it still holds %s queued", podName, queued))
		}
		return false, r.removeCondition(ctx, cluster, scaleDownBlockedConditionType)
	}
	if state != nifiStateDisconnected {
		if err := api.SetNodeStatus(ctx, nodeID, nifiRequestDisconnect); err != nil {
			if cerr := r.blockScaleDown(ctx, cluster, "NodeUnresponsive", fmt.Sprintf(
				"scale-down could not disconnect %s before offloading it: %v. If this node can never respond "+
					"(e.g. it crashed or NiFi leaves it stuck - Apache NIFI-6189), see docs/scaling.md for a "+
					"manual override.", podName, err)); cerr != nil {
				return false, cerr
			}
			return false, fmt.Errorf("disconnecting %s before offload: %w", podName, err)
		}
	}
	if err := api.SetNodeStatus(ctx, nodeID, nifiStateOffloading); err != nil {
		if cerr := r.blockScaleDown(ctx, cluster, "NodeUnresponsive", fmt.Sprintf(
			"scale-down could not request offload for %s: %v. If this node can never respond (e.g. it crashed or "+
				"NiFi leaves it stuck - Apache NIFI-6189), see docs/scaling.md for a manual override.", podName, err)); cerr != nil {
			return false, cerr
		}
		return false, fmt.Errorf("requesting offload for %s: %w", podName, err)
	}
	// A fresh attempt starting now: reset unconditionally (not LoadOrStore), so a stale timestamp
	// left over from an earlier, cancelled attempt against this same pod can never make this new
	// attempt look instantly stuck (NIFI-248 PR #210 review). This is also the point this pod
	// becomes a member of the cache reconnectStrandedNodes scopes itself to.
	r.offloadStartedCache.Store(offloadKey, time.Now())
	msg := fmt.Sprintf("scale-down is offloading %s before removing it; its StatefulSet replica count "+
		"will not be lowered until NiFi confirms it holds no more queued data", podName)
	if seconds, ok := estimatedOffloadSeconds(r.OffloadThroughputBytesPerSecond, queued); ok {
		msg += fmt.Sprintf(" (holds %s queued, an estimated %ds at the configured throughput)", queued, seconds)
	}
	r.event(cluster, corev1.EventTypeNormal, reasonOffloadStarted, msg)
	return false, r.removeCondition(ctx, cluster, scaleDownBlockedConditionType)
}

// blockScaleDown records why a scale-down step cannot confirm a node has offloaded (NIFI-248 PR
// #210 review), so the reason is visible on the NiFiCluster instead of only a transient reconcile
// error or a silent hold.
func (r *NiFiClusterReconciler) blockScaleDown(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, reason, message string) error {
	_, err := r.setCondition(ctx, cluster, scaleDownBlockedConditionType, metav1.ConditionTrue, reason, message)
	return err
}

// podNamed returns the pod named name in pods, or nil when it is not there.
func podNamed(pods []corev1.Pod, name string) *corev1.Pod {
	for i := range pods {
		if pods[i].Name == name {
			return &pods[i]
		}
	}
	return nil
}

// estimatedOffloadSeconds estimates how long offloading queued (NiFi's own "<count> / <size>"
// figure) will take at throughputBytesPerSecond, or DefaultOffloadThroughputBytesPerSecond when
// that is not configured (<= 0). False when queued cannot be parsed.
func estimatedOffloadSeconds(throughputBytesPerSecond int64, queued string) (int64, bool) {
	throughput := throughputBytesPerSecond
	if throughput <= 0 {
		throughput = DefaultOffloadThroughputBytesPerSecond
	}
	bytes, ok := parseQueuedBytes(queued)
	if !ok {
		return 0, false
	}
	return (bytes + throughput - 1) / throughput, true // rounded up
}

// reconcilePodDisruptionBudget ensures a node group's PodDisruptionBudget matches spec, creating,
// updating or deleting it as needed depending on whether it's enabled.
func (r *NiFiClusterReconciler) reconcilePodDisruptionBudget(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup,
) error {
	if !pdbEnabled(nodeGroup) {
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: statefulSetName(cluster, nodeGroup), Namespace: cluster.Namespace},
		}
		if err := r.Delete(ctx, pdb); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting disabled PodDisruptionBudget: %w", err)
		}
		return nil
	}

	pdb := buildPodDisruptionBudget(cluster, nodeGroup)
	return r.createOrUpdate(ctx, cluster, pdb, func(existing client.Object) {
		existingPdb := existing.(*policyv1.PodDisruptionBudget)
		existingPdb.Labels = pdb.Labels
		existingPdb.Annotations = pdb.Annotations
		existingPdb.Spec.MinAvailable = pdb.Spec.MinAvailable
		existingPdb.Spec.Selector = pdb.Spec.Selector
	})
}

// reconcileStatefulSet creates sts if missing, or updates it only when something we actually
// manage (Labels, Annotations - which includes templateHashAnnotation, see buildStatefulSet - or
// Replicas) has actually changed from what's already stored.
//
// Deliberately does NOT go through the generic createOrUpdate/whole-object-equality path: doing
// so here means comparing our freshly-built Spec.Template against the server's copy, which the
// API server has since decorated with Pod-spec defaults ours never sets (a readinessProbe's
// successThreshold, a container's terminationMessagePath, and more) - so it always looks
// "different" even when nothing meaningful changed, causing an infinite update loop. Confirmed
// live at 40-60+ writes/sec before this fix. Comparing only templateHashAnnotation (computed
// purely from our own pre-submission struct, see podTemplateHash) sidesteps server defaulting
// entirely.
func (r *NiFiClusterReconciler) reconcileStatefulSet(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, sts *appsv1.StatefulSet,
) error {
	if err := controllerutil.SetControllerReference(cluster, sts, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}

	key := client.ObjectKeyFromObject(sts)
	desired := stsCacheEntry{
		hash:       sts.Annotations[templateHashAnnotation],
		metaHash:   metadataHash(sts.Labels, sts.Annotations),
		replicas:   *sts.Spec.Replicas,
		clusterUID: cluster.UID,
	}
	if sts.Spec.PersistentVolumeClaimRetentionPolicy != nil {
		desired.pvcRetention = *sts.Spec.PersistentVolumeClaimRetentionPolicy
	}
	store := func(generation int64) {
		entry := desired
		entry.generation = generation
		r.templateHashCache.Store(key, entry)
	}

	// Trust our own memory of the last update we applied before ever reading the object back -
	// see the templateHashCache field comment for why reading it back is unsafe here. The cache
	// only answers "is it up to date", never "does it exist" or "did someone else change it":
	//   - a StatefulSet deleted out of band (kubectl delete, a node group removed and re-added)
	//     is recreated;
	//   - one whose spec someone else changed (kubectl scale, kubectl edit) has a generation other
	//     than the one our own last write produced, and is put back.
	// Both events re-queue the cluster through Owns(). The informer copy is only trusted for the
	// fast path; anything that looks off there is re-checked against the API server below, so an
	// informer lagging behind our own write costs one extra read, never an extra write.
	cached, hit := r.cachedStatefulSet(key, desired)
	if hit {
		current := &appsv1.StatefulSet{}
		err := r.Get(ctx, key, current)
		switch {
		case err == nil && current.Generation == cached.generation &&
			current.Annotations[templateHashAnnotation] == desired.hash:
			return nil
		case apierrors.IsNotFound(err):
			r.templateHashCache.Delete(key)
			hit = false
		case err != nil:
			return fmt.Errorf("checking StatefulSet exists: %w", err)
		}
	}

	existing := &appsv1.StatefulSet{}
	// APIReader, not the cached r.Client: see the field comment on NiFiClusterReconciler.
	err := r.APIReader.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		r.templateHashCache.Delete(key)
		if err := r.Create(ctx, sts); err != nil {
			return err
		}
		store(sts.Generation)
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting StatefulSet: %w", err)
	}

	// Being deleted: its final removal re-queues the cluster through Owns(), and the next pass
	// creates the new one.
	if !existing.DeletionTimestamp.IsZero() {
		r.templateHashCache.Delete(key)
		return nil
	}

	// statefulSetUpToDate cannot see a changed pod template (see above), so a spec changed since
	// our own last write is re-applied whole.
	drifted := hit && existing.Generation != cached.generation
	if !drifted && statefulSetUpToDate(existing, sts) {
		store(existing.Generation)
		return nil
	}

	// Captured before existing is overwritten below, so the lifecycle events this emits
	// describe the actual transition, not just the end state.
	oldReplicas, newReplicas := existing.Spec.Replicas, sts.Spec.Replicas
	oldImage, newImage := containerImage(existing), containerImage(sts)
	templateChanged := !apiequality.Semantic.DeepEqual(existing.Spec.Template, sts.Spec.Template)
	nodeGroupName := sts.Labels[labelNodeGroup]

	// Keep a `kubectl rollout restart` in progress: it is the one template change made by
	// someone else that is meant to stick, and dropping it would roll the pods a second time.
	restartedAt, restarting := existing.Spec.Template.Annotations[restartedAtAnnotation]
	existing.Labels = sts.Labels
	existing.Annotations = sts.Annotations
	existing.Spec.Replicas = sts.Spec.Replicas
	// Mutable on a live StatefulSet and outside the pod template, so changing it never restarts pods.
	existing.Spec.PersistentVolumeClaimRetentionPolicy = sts.Spec.PersistentVolumeClaimRetentionPolicy
	existing.Spec.Template = *sts.Spec.Template.DeepCopy()
	if restarting {
		if existing.Spec.Template.Annotations == nil {
			existing.Spec.Template.Annotations = map[string]string{}
		}
		existing.Spec.Template.Annotations[restartedAtAnnotation] = restartedAt
	}
	if err := r.Update(ctx, existing); err != nil {
		return err
	}
	switch {
	case drifted:
		// An out-of-band change being reverted isn't a spec-driven scale/restart/upgrade - it's
		// undone, not applied - so only StatefulSetDriftReverted fires here, never the events below.
		log.FromContext(ctx).Info("reverted out-of-band change to StatefulSet", "statefulSet", key.Name)
		r.event(cluster, corev1.EventTypeNormal, "StatefulSetDriftReverted", fmt.Sprintf(
			"StatefulSet %q was changed outside the operator; its replicas and pod template were reset "+
				"to the NiFiCluster spec", key.Name))
	default:
		if oldReplicas != nil && newReplicas != nil && *oldReplicas != *newReplicas {
			r.event(cluster, corev1.EventTypeNormal, reasonScaling, fmt.Sprintf(
				"node group %q scaling from %d to %d replicas", nodeGroupName, *oldReplicas, *newReplicas))
		}
		if templateChanged {
			if oldImage != "" && newImage != "" && oldImage != newImage {
				r.event(cluster, corev1.EventTypeNormal, reasonUpgradeStarted, fmt.Sprintf(
					"node group %q rolling out image change from %q to %q", nodeGroupName, oldImage, newImage))
				apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
					Type:               upgradingConditionType,
					Status:             metav1.ConditionTrue,
					Reason:             "RolloutInProgress",
					Message:            fmt.Sprintf("Node group %q is rolling out image %q.", nodeGroupName, newImage),
					ObservedGeneration: cluster.Generation,
				})
			} else {
				r.event(cluster, corev1.EventTypeNormal, reasonRollingRestart, fmt.Sprintf(
					"node group %q pod template changed; pods are rolling to the updated template", nodeGroupName))
			}
		}
	}
	store(existing.Generation)
	return nil
}

// containerImage returns the NifiContainerName container's image from sts's pod template, or ""
// if that container isn't present (only possible for a not-yet-fully-built StatefulSet, never a
// real one this reconciler produced).
func containerImage(sts *appsv1.StatefulSet) string {
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == NifiContainerName {
			return sts.Spec.Template.Spec.Containers[i].Image
		}
	}
	return ""
}

// restartedAtAnnotation is the pod template annotation `kubectl rollout restart` sets.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// cachedStatefulSet returns the cache entry for key when it records an update matching desired.
func (r *NiFiClusterReconciler) cachedStatefulSet(key client.ObjectKey, desired stsCacheEntry) (stsCacheEntry, bool) {
	v, ok := r.templateHashCache.Load(key)
	if !ok {
		return stsCacheEntry{}, false
	}
	cached := v.(stsCacheEntry)
	return cached, cached.hash == desired.hash && cached.metaHash == desired.metaHash &&
		cached.replicas == desired.replicas && cached.clusterUID == desired.clusterUID &&
		cached.pvcRetention == desired.pvcRetention
}

// metadataHash identifies a StatefulSet's desired labels and annotations, so a change to only
// spec.labels/spec.annotations (which leaves the pod template and replicas alone) still misses
// the write cache and reaches the StatefulSet.
func metadataHash(labels, annotations map[string]string) string {
	entries := make([]string, 0, len(labels)+len(annotations))
	for k, v := range labels {
		entries = append(entries, "l:"+k+"="+v)
	}
	for k, v := range annotations {
		entries = append(entries, "a:"+k+"="+v)
	}
	sort.Strings(entries)
	return stringsHash(entries)
}

// statefulSetUpToDate reports whether existing already matches everything desired manages about
// a StatefulSet. See reconcileStatefulSet for why this doesn't compare Spec.Template directly.
func statefulSetUpToDate(existing, desired *appsv1.StatefulSet) bool {
	if !apiequality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return false
	}
	if !apiequality.Semantic.DeepEqual(existing.Annotations, desired.Annotations) {
		return false
	}
	if !apiequality.Semantic.DeepEqual(existing.Spec.PersistentVolumeClaimRetentionPolicy, desired.Spec.PersistentVolumeClaimRetentionPolicy) {
		return false
	}
	return apiequality.Semantic.DeepEqual(existing.Spec.Replicas, desired.Spec.Replicas)
}

// createOrUpdate creates obj if it does not exist, or applies mutate and updates it otherwise -
// but only when mutate actually changes something. obj is always owned by cluster so garbage
// collection removes it when the cluster is deleted.
//
// Skipping no-op updates isn't an optimization, it's a correctness requirement: every object
// here is Owns()-watched (see SetupWithManager), so an unconditional Update on every reconcile -
// even one that writes back identical content - bumps resourceVersion, which re-triggers the
// watch, which queues another reconcile, forever. Confirmed live: this was silently hammering
// the API server at 40-60+ writes/sec per test cluster, everything called it a "resource
// exhaustion" problem with the *host* instead of ever suspecting the reconcile loop itself.
// controller-runtime's own CreateOrUpdate does exactly the create-or-diff-then-update dance we
// want, including the equality check that makes a no-op mutate a true no-op.
func (r *NiFiClusterReconciler) createOrUpdate(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, obj client.Object, mutate func(client.Object),
) error {
	// obj already carries the fully-built desired state from its buildX() call, and callers'
	// mutate closures read desired values off that same obj by name (e.g. "cm.Data" where cm IS
	// obj) - so obj must never be the thing CreateOrUpdate's Get() overwrites with server state,
	// or those closures would read back the server's *old* values instead of the desired ones.
	// target is a separate, empty-bodied object carrying only the same Name/Namespace/GVK for
	// CreateOrUpdate to Get/mutate/Create/Update in place, leaving obj itself untouched as the
	// closures' source of truth throughout.
	target := obj.DeepCopyObject().(client.Object)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, target, func() error {
		mutate(target)
		return controllerutil.SetControllerReference(cluster, target, r.Scheme)
	})
	return err
}

// reconcileDelete handles cleanup once a NiFiCluster is marked for deletion. Owner-reference
// garbage collection removes every generated object; this only needs to drop the finalizer.
func (r *NiFiClusterReconciler) reconcileDelete(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (ctrl.Result, error) {
	// Forget the per-cluster throttles, which would otherwise outlive every deleted cluster.
	r.lastNiFiPoll.Delete(cluster.UID)
	r.lastRBACCheck.Delete(cluster.UID)
	certmetrics.DeleteCAExpiry(cluster.Namespace, cluster.Name)
	if !controllerutil.ContainsFinalizer(cluster, nifiClusterFinalizer) {
		return ctrl.Result{}, nil
	}
	// NiFiNodeGroups are still there while the finalizer holds the cluster, and their retention
	// policy counts as much as the inline groups'.
	nodeGroupObjects, err := r.listNodeGroupObjects(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Merged into a copy: the finalizer removal below writes cluster, and must not write the
	// NiFiNodeGroups into its spec.
	merged := cluster.DeepCopy()
	mergeNodeGroups(merged, nodeGroupObjects)
	// the key and NiFi's component state belong to the flow on the PVCs. Kept only while
	// some PVC outlives the cluster; when none does, garbage collection takes the still-owned key
	// Secret and the component state is deleted below.
	keepData, err := r.keepsDataOnDelete(ctx, merged)
	if err != nil {
		return ctrl.Result{}, err
	}
	if keepData {
		if err := r.retainSensitivePropsSecret(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !keepData {
		// An earlier pass may have released the key while a PVC still looked like it would stay;
		// with nothing left to decrypt, it must not outlive the cluster.
		if err := r.deleteReleasedSensitivePropsSecret(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}
	if needsKubernetesAPI(cluster) {
		requeue, err := r.cleanupCoordinationState(ctx, cluster)
		if err != nil || requeue > 0 {
			return ctrl.Result{RequeueAfter: requeue}, err
		}
		if !keepData {
			if err := r.deleteComponentState(ctx, cluster); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	// A NiFiNodeGroup that appeared while the cluster could not be reconciled may lack its owner
	// reference; without it garbage collection would leave it behind.
	for i := range nodeGroupObjects {
		if o := &nodeGroupObjects[i]; o.DeletionTimestamp.IsZero() {
			if err := r.ensureNodeGroupOwner(ctx, cluster, o); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	controllerutil.RemoveFinalizer(cluster, nifiClusterFinalizer)
	if err := r.Update(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// nodeGroupStatus derives a NodeGroupStatus from the observed StatefulSet.
func nodeGroupStatus(nodeGroup *nifiv1alpha1.NodeGroup, sts *appsv1.StatefulSet) nifiv1alpha1.NodeGroupStatus {
	return nifiv1alpha1.NodeGroupStatus{
		Name:                nodeGroup.Name,
		Replicas:            nodeGroup.Replicas,
		ReadyReplicas:       sts.Status.ReadyReplicas,
		UpdatedReplicas:     sts.Status.UpdatedReplicas,
		StatefulSetObserved: sts.Status.ObservedGeneration >= sts.Generation,
	}
}

// updateStatus aggregates per-node-group status onto NiFiCluster.Status. Skips the write
// entirely when nothing actually changed - NiFiCluster is the primary watched resource (see
// SetupWithManager's For(...)), so an unconditional Status().Update() on every reconcile would
// re-trigger this same reconciler on its own status write, forever. See createOrUpdate's comment
// for how this exact shape of bug was found live.
//
// failed names the node groups that could not be reconciled: they make the cluster Degraded and
// never Available.
func (r *NiFiClusterReconciler) updateStatus(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, statuses []nifiv1alpha1.NodeGroupStatus, failed []string,
	poll *nifiPoll,
) error {
	before := cluster.Status.DeepCopy()
	applyNiFiPoll(&cluster.Status, poll, cluster.Generation)

	// Set by earlier versions, which could pause reconciliation.
	apimeta.RemoveStatusCondition(&cluster.Status.Conditions, "ReconciliationPaused")

	var totalReplicas, totalReady, totalUpdated int32
	allObserved := true
	for _, s := range statuses {
		totalReplicas += s.Replicas
		totalReady += s.ReadyReplicas
		totalUpdated += s.UpdatedReplicas
		allObserved = allObserved && s.StatefulSetObserved
	}

	available := metav1.ConditionFalse
	if totalReady == totalReplicas && totalReplicas > 0 && len(failed) == 0 {
		available = metav1.ConditionTrue
	}
	degraded := metav1.Condition{
		Type: degradedConditionType, Status: metav1.ConditionFalse, Reason: "AllNodeGroupsReconciled",
		Message: "Every node group reconciled.", ObservedGeneration: cluster.Generation,
	}
	if len(failed) > 0 {
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = "NodeGroupReconcileFailed"
		degraded.Message = fmt.Sprintf("Node groups that failed to reconcile: %s. See the operator log and Events.", strings.Join(failed, ", "))
	}
	if total := clusterReplicas(cluster); total > nifiv1alpha1.MaxClusterNodes {
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = reasonNodeLimitExceeded
		degraded.Message = nodeLimitMessage(total)
	}
	apimeta.SetStatusCondition(&cluster.Status.Conditions, degraded)

	// Set by earlier versions, which expanded volumes online.
	apimeta.RemoveStatusCondition(&cluster.Status.Conditions, "StorageResizing")

	if before.Replicas > totalReplicas {
		cluster.Status.NodeRemovalPending = true
	}
	cluster.Status.NodeGroupStatuses = statuses
	cluster.Status.Replicas = totalReplicas
	cluster.Status.ReadyReplicas = totalReady
	cluster.Status.CoordinationBackend = coordinationBackend(cluster)
	cluster.Status.AuthSecretName = effectiveAuthSecretName(cluster)
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               coordinationConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             reasonCoordinationUpToDate,
		Message:            fmt.Sprintf("All node groups run as one NiFi cluster on the %s coordination backend.", coordinationBackend(cluster)),
		ObservedGeneration: cluster.Generation,
	})
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "Available",
		Status:             available,
		Reason:             "NodeGroupsReconciled",
		Message:            fmt.Sprintf("%d/%d replicas ready", totalReady, totalReplicas),
		ObservedGeneration: cluster.Generation,
	})

	// An upgrade that reasonUpgradeStarted set to True completes once every node group is back to
	// fully ready AND every replica is on the current pod template revision - checked here, once
	// per reconcile after every node group has had its turn, rather than inside
	// reconcileStatefulSet itself (which only sees one node group at a time and runs before this
	// aggregation exists).
	//
	// ReadyReplicas alone is not enough: live testing caught it reporting a false completion
	// mid-rollout, the moment a just-updated pod's readiness happened to coincide with an
	// old-revision pod that hadn't been replaced yet, making totalReady == totalReplicas look
	// satisfied before every pod had actually moved to the new image.
	//
	// allObserved closes a second, subtler gap live testing also caught: reconcileNodeGroup reads
	// the StatefulSet straight back right after writing its new spec, in the same reconcile - every
	// status field on that read, not just ReadyReplicas, can still be the *previous* template's
	// values for one reconcile, since the real StatefulSet controller hasn't processed the new
	// generation yet. StatefulSetObserved (ObservedGeneration caught up to Generation) is what
	// actually distinguishes a stale read from a trustworthy one.
	upgraded := allObserved && totalUpdated == totalReplicas
	if available == metav1.ConditionTrue && upgraded && apimeta.IsStatusConditionTrue(cluster.Status.Conditions, upgradingConditionType) {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               upgradingConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             "RolloutComplete",
			Message:            "All node groups are ready on their current image.",
			ObservedGeneration: cluster.Generation,
		})
		r.event(cluster, corev1.EventTypeNormal, reasonUpgradeCompleted, "all node groups are ready on their current image")
	}

	if apiequality.Semantic.DeepEqual(before, &cluster.Status) {
		return nil
	}
	return r.updateClusterStatus(ctx, cluster)
}

// SetupWithManager registers this reconciler with mgr, watching NiFiCluster and its owned
// StatefulSets/Services/ConfigMaps so changes to generated objects trigger reconciliation too.
func (r *NiFiClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nifiv1alpha1.NiFiCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.clustersReferencingSecret)).
		Watches(&nifiv1alpha1.NiFiNodeGroup{}, handler.EnqueueRequestsFromMapFunc(clusterOfNodeGroup),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("nificluster").
		WithOptions(controller.Options{RateLimiter: reconcileRateLimiter()}).
		Complete(r)
}

// maxReconcileBackoff caps the retry delay of a NiFiCluster whose reconcile keeps failing, such as
// one with a node group that cannot be created. controller-runtime's default cap is 1000s; at 5
// minutes a fixed cause (a Secret created, a quota raised) is picked up soon without a spec edit.
const maxReconcileBackoff = 5 * time.Minute

// reconcileRateLimiter is controller-runtime's default rate limiter with maxReconcileBackoff as
// the per-item cap: exponential per-cluster backoff from 5ms, and an overall 10 qps (burst 100).
func reconcileRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, maxReconcileBackoff),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}
