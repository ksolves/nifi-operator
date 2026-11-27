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

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// serviceConditionType is only present (False) while the cluster Service cannot be created.
const serviceConditionType = "ServiceReady"

// reconcileExposure reconciles the cluster Service, which spans every node group. Ingress, Gateway
// API routes and OpenShift Routes are left to the user, pointed at this Service.
func (r *NiFiClusterReconciler) reconcileExposure(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	var errs []error
	svc := buildClusterService(cluster)
	if err := r.createOrUpdateOwned(ctx, cluster, svc, func(existing client.Object) {
		s := existing.(*corev1.Service)
		s.Labels = svc.Labels
		s.Annotations = svc.Annotations
		ports := svc.Spec.Ports
		if svc.Spec.Type != corev1.ServiceTypeClusterIP {
			ports = keepAllocatedNodePorts(ports, s.Spec.Ports)
		}
		s.Spec.Type = svc.Spec.Type
		s.Spec.Selector = svc.Spec.Selector
		s.Spec.Ports = ports
		s.Spec.SessionAffinity = svc.Spec.SessionAffinity
		s.Spec.SessionAffinityConfig = svc.Spec.SessionAffinityConfig
		s.Spec.LoadBalancerClass = svc.Spec.LoadBalancerClass
		s.Spec.LoadBalancerSourceRanges = svc.Spec.LoadBalancerSourceRanges
		s.Spec.ExternalTrafficPolicy = svc.Spec.ExternalTrafficPolicy
	}); err != nil {
		errs = append(errs, fmt.Errorf("reconciling cluster Service: %w", err))
		if errors.Is(err, errNotOwned) {
			if _, cerr := r.setCondition(ctx, cluster, serviceConditionType, metav1.ConditionFalse, "ServiceNameConflict",
				fmt.Sprintf("A Service named %q already exists in this namespace and is not managed by this NiFiCluster. "+
					"Rename or remove it so the operator can create the cluster Service.", clusterServiceName(cluster))); cerr != nil {
				errs = append(errs, cerr)
			}
		}
	} else if err := r.removeCondition(ctx, cluster, serviceConditionType); err != nil {
		errs = append(errs, err)
	}

	// Conditions only earlier versions set (spec.gateway, spec.route).
	for _, condType := range []string{"GatewayReady", "RouteReady"} {
		if err := r.removeCondition(ctx, cluster, condType); err != nil {
			errs = append(errs, err)
		}
	}
	return kerrors.NewAggregate(errs)
}

// removeCondition drops a status condition that no longer applies, persisting only when it was
// present.
func (r *NiFiClusterReconciler) removeCondition(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, condType string) error {
	if !apimeta.RemoveStatusCondition(&cluster.Status.Conditions, condType) {
		return nil
	}
	return r.updateClusterStatus(ctx, cluster)
}
