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
	"fmt"
	"sort"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// The CoordinationRBACReady condition reports whether every ServiceAccount the
// cluster's NiFi pods run as may actually use Leases and ConfigMaps for Kubernetes coordination -
// the operator's own binding, one made by hand, or neither. Without it, pods on a node group's own
// ServiceAccount silently fail to elect a coordinator or keep component state.
const (
	coordinationRBACConditionType = "CoordinationRBACReady"
	reasonCoordinationRBACMissing = "CoordinationRBACMissing"

	// rbacCheckInterval is how often a passing check is repeated, so a binding removed later is
	// still reported.
	rbacCheckInterval = time.Minute
	// rbacRecheckInterval is how soon a failing check is repeated. A check only reports the
	// condition False after failing twice in a row: the API server's authorizer can lag a
	// RoleBinding the operator has only just written.
	rbacRecheckInterval = 10 * time.Second
)

// rbacCheck is the last coordination RBAC check of one cluster.
type rbacCheck struct {
	at         time.Time
	generation int64
	failures   int
}

// reconcileCoordinationRBACCondition sets CoordinationRBACReady from SubjectAccessReviews of every
// ServiceAccount the pods run as, at most every rbacCheckInterval while it passes.
func (r *NiFiClusterReconciler) reconcileCoordinationRBACCondition(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (ctrl.Result, error) {
	if !needsKubernetesAPI(cluster) {
		r.lastRBACCheck.Delete(cluster.UID)
		return ctrl.Result{}, r.removeCondition(ctx, cluster, coordinationRBACConditionType)
	}
	var prev rbacCheck
	if v, ok := r.lastRBACCheck.Load(cluster.UID); ok {
		prev = v.(rbacCheck)
		wait := rbacCheckInterval
		if prev.failures > 0 {
			wait = rbacRecheckInterval
		}
		if prev.generation == cluster.Generation {
			if left := wait - time.Since(prev.at); left > 0 {
				return ctrl.Result{RequeueAfter: left}, nil
			}
		}
	}

	missing, err := r.missingCoordinationAccess(ctx, cluster)
	if err != nil {
		// Without an answer, report nothing rather than a guess; try again shortly.
		log.FromContext(ctx).V(1).Info("checking coordination RBAC failed", "error", err.Error())
		return ctrl.Result{RequeueAfter: rbacRecheckInterval}, nil
	}
	check := rbacCheck{at: time.Now(), generation: cluster.Generation}
	if len(missing) == 0 {
		r.lastRBACCheck.Store(cluster.UID, check)
		_, err := r.setCondition(ctx, cluster, coordinationRBACConditionType, metav1.ConditionTrue, "Granted",
			"Every ServiceAccount the NiFi pods run as may use the Leases and ConfigMaps Kubernetes coordination needs.")
		return ctrl.Result{RequeueAfter: rbacCheckInterval}, err
	}
	check.failures = prev.failures + 1
	r.lastRBACCheck.Store(cluster.UID, check)
	if check.failures < 2 {
		return ctrl.Result{RequeueAfter: rbacRecheckInterval}, nil
	}
	message := r.coordinationRBACMessage(cluster, missing)
	changed, err := r.setCondition(ctx, cluster, coordinationRBACConditionType, metav1.ConditionFalse, reasonCoordinationRBACMissing, message)
	if changed {
		r.event(cluster, corev1.EventTypeWarning, reasonCoordinationRBACMissing, message)
	}
	return ctrl.Result{RequeueAfter: rbacRecheckInterval}, err
}

// podServiceAccounts maps every ServiceAccount the cluster's NiFi pods run as to the node groups
// running as it.
func podServiceAccounts(cluster *nifiv1alpha1.NiFiCluster) map[string][]string {
	out := map[string][]string{}
	for i := range cluster.Spec.NodeGroups {
		ng := &cluster.Spec.NodeGroups[i]
		sa := nifiPodServiceAccount(cluster)
		if ng.PodOverrides != nil && ng.PodOverrides.ServiceAccountName != "" {
			sa = ng.PodOverrides.ServiceAccountName
		}
		out[sa] = append(out[sa], ng.Name)
	}
	return out
}

// missingCoordinationAccess returns, per ServiceAccount the pods run as, the coordination
// permissions (as "verb resource") it lacks, asking the API server's authorizer through a
// SubjectAccessReview for each.
func (r *NiFiClusterReconciler) missingCoordinationAccess(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) (map[string][]string, error) {
	missing := map[string][]string{}
	for sa := range podServiceAccounts(cluster) {
		for _, rule := range nifiCoordinationRules() {
			for _, verb := range rule.Verbs {
				sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
					User: "system:serviceaccount:" + cluster.Namespace + ":" + sa,
					Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + cluster.Namespace,
						"system:authenticated"},
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: cluster.Namespace, Verb: verb, Group: rule.APIGroups[0], Resource: rule.Resources[0],
					},
				}}
				if err := r.Create(ctx, sar); err != nil {
					return nil, fmt.Errorf("SubjectAccessReview for ServiceAccount %s: %w", sa, err)
				}
				if !sar.Status.Allowed {
					missing[sa] = append(missing[sa], verb+" "+rule.Resources[0])
				}
			}
		}
	}
	return missing, nil
}

// coordinationRBACMessage names each ServiceAccount missing access, its node groups and how to
// grant it.
func (r *NiFiClusterReconciler) coordinationRBACMessage(cluster *nifiv1alpha1.NiFiCluster, missing map[string][]string) string {
	groups := podServiceAccounts(cluster)
	sas := make([]string, 0, len(missing))
	for sa := range missing {
		sas = append(sas, sa)
	}
	sort.Strings(sas)
	parts := make([]string, 0, len(sas))
	for _, sa := range sas {
		parts = append(parts, fmt.Sprintf(
			"ServiceAccount %q (node groups %s) may not %s; grant it with: kubectl -n %s create rolebinding %s-%s --role=%s --serviceaccount=%s:%s",
			sa, strings.Join(groups[sa], ", "), strings.Join(missing[sa], ", "),
			cluster.Namespace, nifiRoleName(cluster), sa, nifiRoleName(cluster), cluster.Namespace, sa))
	}
	msg := "NiFi pods cannot use Kubernetes coordination: " + strings.Join(parts, "; ") + "."
	if !r.bindsNodeGroupServiceAccounts(cluster) {
		msg += " Or have the operator bind it: set coordination.bindPodServiceAccounts, which the operator admin " +
			"must allow with --allow-custom-service-account-binding."
	}
	return msg
}
