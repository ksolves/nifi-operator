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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

const (
	// coordinationBackendAnnotation records, on every StatefulSet the operator writes, the
	// coordination backend its pod template was built for. A StatefulSet without it predates
	// shared cluster coordination (each node group ran its own embedded ZooKeeper quorum).
	coordinationBackendAnnotation = "nifi.ksolves.com/coordination-backend"

	// approveCoordinationMigrationAnnotation approves the full-stop migration of a cluster whose
	// StatefulSets were built for a different coordination backend (see
	// reconcileCoordinationMigration). Its value must name the backend being migrated to, so an
	// approval left on the object can never silently authorize a later, different migration.
	approveCoordinationMigrationAnnotation = "nifi.ksolves.com/approve-coordination-migration"

	// kubernetesLeaderElectionManager and kubernetesStateProviderID are NiFi's own names for its
	// Kubernetes Lease based leader election and the ConfigMap state provider entry shipped in
	// the image's conf/state-management.xml.
	kubernetesLeaderElectionManager = "KubernetesLeaderElectionManager"
	kubernetesStateProviderID       = "kubernetes-provider"

	// curatorLeaderElectionManager and zooKeeperStateProviderID are NiFi's names for its
	// ZooKeeper (Curator) based leader election and the ZooKeeper state provider entry.
	curatorLeaderElectionManager = "CuratorLeaderElectionManager"
	zooKeeperStateProviderID     = "zk-provider"
)

// coordinationBackend returns the cluster's effective coordination backend. The CRD defaults it,
// so the fallback only matters for objects built in tests or before defaulting ran.
func coordinationBackend(cluster *nifiv1alpha1.NiFiCluster) nifiv1alpha1.CoordinationBackend {
	if c := cluster.Spec.Coordination; c != nil && c.Backend != "" {
		return c.Backend
	}
	return nifiv1alpha1.CoordinationBackendKubernetes
}

// clusterReplicas is the total number of NiFi nodes across every node group.
func clusterReplicas(cluster *nifiv1alpha1.NiFiCluster) int32 {
	var total int32
	for i := range cluster.Spec.NodeGroups {
		total += cluster.Spec.NodeGroups[i].Replicas
	}
	return total
}

// coordinationProperties returns the nifi.properties entries selecting the cluster's coordination
// backend. The embedded ZooKeeper server is always off.
//
// Kubernetes: Lease based leader election plus the ConfigMap cluster state provider. The lease
// prefix is the NiFiCluster's name, which is unique per namespace - NiFi requires a unique prefix
// for every cluster sharing a namespace. The entrypoint copies the same value into the
// kubernetes-provider "ConfigMap Name Prefix" in state-management.xml, so a cluster's Leases and
// state ConfigMaps never collide with another cluster's. nifi.zookeeper.* stays at the image's
// empty defaults.
//
// ZooKeeper: Curator leader election and the ZooKeeper state provider against the external
// ensemble, under the cluster's own root node. The entrypoint mirrors the connect string and root
// node into the zk-provider in state-management.xml, which does not read them from
// nifi.properties.
func coordinationProperties(cluster *nifiv1alpha1.NiFiCluster) map[string]string {
	if coordinationBackend(cluster) == nifiv1alpha1.CoordinationBackendZooKeeper {
		return map[string]string{
			nifiPropLeaderElectionImpl:     curatorLeaderElectionManager,
			nifiPropZookeeperConnectString: cluster.Spec.Coordination.ZooKeeper.ConnectString,
			nifiPropZookeeperRootNode:      zooKeeperRootNode(cluster),
			nifiPropStateProviderCluster:   zooKeeperStateProviderID,
			nifiPropEmbeddedZookeeperStart: "false",
		}
	}
	return map[string]string{
		nifiPropLeaderElectionImpl:        kubernetesLeaderElectionManager,
		nifiPropLeaderElectionLeasePrefix: leaseNamePrefix(cluster),
		nifiPropStateProviderCluster:      kubernetesStateProviderID,
		nifiPropEmbeddedZookeeperStart:    "false",
	}
}

// zooKeeperRootNode returns the znode a ZooKeeper-coordinated cluster lives under: as set, or
// /nifi/<namespace>/<name>, which no other NiFiCluster can share.
func zooKeeperRootNode(cluster *nifiv1alpha1.NiFiCluster) string {
	if zk := cluster.Spec.Coordination.ZooKeeper; zk != nil && zk.RootNode != "" {
		return zk.RootNode
	}
	return fmt.Sprintf("/nifi/%s/%s", cluster.Namespace, cluster.Name)
}

// needsKubernetesAPI reports whether the NiFi pods themselves talk to the Kubernetes API, which
// only Kubernetes coordination does. Otherwise they get no Role and no ServiceAccount token.
func needsKubernetesAPI(cluster *nifiv1alpha1.NiFiCluster) bool {
	return coordinationBackend(cluster) == nifiv1alpha1.CoordinationBackendKubernetes
}

// coordinationConditionType is the NiFiCluster condition reporting whether every StatefulSet runs
// the coordination backend in spec. False while a legacy cluster awaits (or is going through) its
// full-stop migration.
const coordinationConditionType = "CoordinationUpToDate"

// Reasons for coordinationConditionType.
const (
	reasonCoordinationUpToDate        = "UpToDate"
	reasonMigrationAwaitingApproval   = "MigrationAwaitingApproval"
	reasonMigrationStoppingNodes      = "MigrationStoppingNodes"
	reasonConflictingSensitivePropKey = "ConflictingSensitivePropsKeys"
)

// errConflictingLegacySensitiveKeys means the legacy per-node-group sensitive-props Secrets of a
// cluster hold different keys, so no single cluster-wide key can decrypt every node group's
// existing flow. See reconcileSensitivePropsSecret.
var errConflictingLegacySensitiveKeys = errors.New("legacy node groups use different nifi.sensitive.props.key values")

// legacyStatefulSets returns the cluster's StatefulSets whose pod template was built for a
// different coordination backend than spec asks for - in practice, StatefulSets written by an
// operator version where every node group ran its own embedded ZooKeeper quorum (no
// coordinationBackendAnnotation at all).
func (r *NiFiClusterReconciler) legacyStatefulSets(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) ([]appsv1.StatefulSet, error) {
	list := &appsv1.StatefulSetList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		labelAppManagedBy: managedByValue,
		labelAppInstance:  cluster.Name,
	}); err != nil {
		return nil, fmt.Errorf("listing StatefulSets: %w", err)
	}
	desired := string(coordinationBackend(cluster))
	var legacy []appsv1.StatefulSet
	for i := range list.Items {
		if !metav1.IsControlledBy(&list.Items[i], cluster) {
			continue
		}
		if list.Items[i].Annotations[coordinationBackendAnnotation] != desired {
			legacy = append(legacy, list.Items[i])
		}
	}
	return legacy, nil
}

// reconcileCoordinationMigration gates every node-group change on the cluster's StatefulSets all
// running the coordination backend in spec. It returns proceed=true when the normal reconcile may
// continue.
//
// A legacy cluster is never migrated implicitly: its nodes would have to switch leader election
// and state backend together, and a single legacy pod restarting onto a ConfigMap already
// rewritten for the new backend would form a second, split-brain cluster. So until the user
// approves (approveCoordinationMigrationAnnotation=<target backend>) the operator leaves the node groups
// completely untouched and reports CoordinationUpToDate=False. Once approved it performs a full
// stop: every legacy StatefulSet is scaled to zero, and only when no NiFi pod of the cluster is
// left does the normal reconcile rewrite ConfigMaps and StatefulSets for the new backend and start
// the nodes again. PVCs (flow, repositories) and the sensitive-props key are kept; cluster-scoped
// component state held by the old backend is not carried over.
func (r *NiFiClusterReconciler) reconcileCoordinationMigration(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster,
) (proceed bool, result ctrl.Result, err error) {
	legacy, err := r.legacyStatefulSets(ctx, cluster)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if len(legacy) == 0 {
		return true, ctrl.Result{}, nil
	}

	backend := coordinationBackend(cluster)
	if cluster.Annotations[approveCoordinationMigrationAnnotation] != string(backend) {
		msg := fmt.Sprintf("%d StatefulSet(s) were built for a different coordination backend than %s "+
			"(each node group ran its own embedded ZooKeeper). Node groups are left untouched until the "+
			"migration is approved. Approving stops ALL NiFi nodes of this cluster, restarts them as one "+
			"cluster on the %s backend, keeps flows, repositories and the sensitive-props key, and does not "+
			"carry over cluster-scoped component state (e.g. listing processors' state). To approve: "+
			"kubectl annotate nificluster.nifi.ksolves.com %s %s=%s",
			len(legacy), backend, backend, cluster.Name, approveCoordinationMigrationAnnotation, backend)
		if changed, err := r.setCondition(ctx, cluster, coordinationConditionType, metav1.ConditionFalse,
			reasonMigrationAwaitingApproval, msg); err != nil {
			return false, ctrl.Result{}, err
		} else if changed {
			r.event(cluster, corev1.EventTypeWarning, reasonMigrationAwaitingApproval, msg)
		}
		return false, ctrl.Result{}, nil
	}

	for i := range legacy {
		sts := &legacy[i]
		if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
			continue
		}
		zero := int32(0)
		patch := client.MergeFrom(sts.DeepCopy())
		sts.Spec.Replicas = &zero
		if err := r.Patch(ctx, sts, patch); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("scaling legacy StatefulSet %q to zero: %w", sts.Name, err)
		}
		// The cached "last applied" entry no longer describes the object; force the next
		// reconcileStatefulSet to compare against the server.
		r.templateHashCache.Delete(client.ObjectKeyFromObject(sts))
		r.event(cluster, corev1.EventTypeNormal, reasonMigrationStoppingNodes,
			fmt.Sprintf("scaled legacy StatefulSet %s to zero for the coordination migration", sts.Name))
	}

	running, err := r.clusterPodCount(ctx, cluster)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if running > 0 {
		if _, err := r.setCondition(ctx, cluster, coordinationConditionType, metav1.ConditionFalse, reasonMigrationStoppingNodes,
			// No pod count in the message: it would change on every poll and rewrite status each time.
			fmt.Sprintf("Coordination migration approved: waiting for all NiFi pods to stop before restarting "+
				"the cluster on the %s backend.", backend)); err != nil {
			return false, ctrl.Result{}, err
		}
		return false, ctrl.Result{RequeueAfter: migrationPollInterval}, nil
	}
	return true, ctrl.Result{}, nil
}

// migrationPollInterval is how often a stopping migration re-checks for remaining pods. Pod
// deletions are not watched (the operator does not cache Pods cluster-wide), so this is polled.
const migrationPollInterval = 10 * time.Second

// clusterPodCount counts the cluster's NiFi pods that still exist (in any phase). It reads the
// API server directly rather than through the manager's cache, so the operator never starts a
// cluster-wide Pod informer just for this rarely-used path.
func (r *NiFiClusterReconciler) clusterPodCount(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (int, error) {
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		labelAppName:     appNameValue,
		labelAppInstance: cluster.Name,
	}); err != nil {
		return 0, fmt.Errorf("listing NiFi pods: %w", err)
	}
	return len(pods.Items), nil
}

// setCondition records a NiFiCluster status condition and persists it right away, skipping the
// write when nothing changed (NiFiCluster is the primary watched object, so a write per reconcile
// would re-trigger reconciliation forever). changed reports whether a write happened, so callers
// can emit an Event only on transitions.
func (r *NiFiClusterReconciler) setCondition(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, condType string, status metav1.ConditionStatus, reason, message string,
) (changed bool, err error) {
	if existing := apimeta.FindStatusCondition(cluster.Status.Conditions, condType); existing != nil &&
		existing.Status == status && existing.Reason == reason && existing.Message == message &&
		existing.ObservedGeneration == cluster.Generation {
		return false, nil
	}
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: message, ObservedGeneration: cluster.Generation,
	})
	if err := r.updateClusterStatus(ctx, cluster); err != nil {
		return false, fmt.Errorf("updating %s condition: %w", condType, err)
	}
	return true, nil
}

// event records a Kubernetes Event on the NiFiCluster, when an EventRecorder is configured.
func (r *NiFiClusterReconciler) event(cluster *nifiv1alpha1.NiFiCluster, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(cluster, eventType, reason, message)
	}
}

// leaseNamePrefix is the prefix NiFi puts on its Leases and component-state ConfigMaps (the
// lease prefix, which the entrypoint also uses as the state provider's ConfigMap name prefix).
func leaseNamePrefix(cluster *nifiv1alpha1.NiFiCluster) string {
	return cluster.Name
}

// coordinationCleanupTimeout bounds how long cluster deletion waits for the NiFi pods to stop
// before removing their coordination objects anyway, so a stuck pod can't block the delete forever.
const coordinationCleanupTimeout = 10 * time.Minute

// cleanupCoordinationState removes the coordinator and primary-node Leases NiFi created for a
// Kubernetes-coordinated cluster that is being deleted. NiFi gives them no owner reference, so
// garbage collection never removes them. Running pods would recreate them, so the
// StatefulSets are deleted first and the cleanup waits for the pods to be gone; it returns a
// non-zero requeue while it waits.
//
// NiFi's component-state ConfigMaps (<prefix>-nifi-component-<id>) are deliberately kept, like the
// retained PVCs and the sensitive-props key: a NiFiCluster recreated under the same name resumes
// its flow, including List*/Query* processor state, instead of re-reading data from scratch.
func (r *NiFiClusterReconciler) cleanupCoordinationState(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (time.Duration, error) {
	selector := client.MatchingLabels{labelAppManagedBy: managedByValue, labelAppInstance: cluster.Name}
	waiting := time.Since(cluster.DeletionTimestamp.Time) < coordinationCleanupTimeout
	if waiting {
		stsList := &appsv1.StatefulSetList{}
		if err := r.List(ctx, stsList, client.InNamespace(cluster.Namespace), selector); err != nil {
			return 0, fmt.Errorf("listing StatefulSets: %w", err)
		}
		for i := range stsList.Items {
			if metav1.IsControlledBy(&stsList.Items[i], cluster) {
				if err := r.Delete(ctx, &stsList.Items[i]); client.IgnoreNotFound(err) != nil {
					return 0, fmt.Errorf("deleting StatefulSet %q: %w", stsList.Items[i].Name, err)
				}
			}
		}
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), selector); err != nil {
			return 0, fmt.Errorf("listing pods: %w", err)
		}
		if len(pods.Items) > 0 {
			return 5 * time.Second, nil
		}
	}

	prefix := leaseNamePrefix(cluster)
	for _, name := range []string{prefix + "-cluster-coordinator", prefix + "-primary-node"} {
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}}
		if err := r.Delete(ctx, lease); client.IgnoreNotFound(err) != nil {
			return 0, fmt.Errorf("deleting Lease %q: %w", name, err)
		}
	}
	return 0, nil
}
