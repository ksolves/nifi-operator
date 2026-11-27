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

package v1alpha1

import (
	"context"
	"fmt"
	"net/http"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-nifi-ksolves-com-v1alpha1-nifinodegroup-scale,mutating=false,failurePolicy=fail,sideEffects=None,timeoutSeconds=5,groups=nifi.ksolves.com,resources=nifinodegroups/scale,verbs=update,versions=v1alpha1,name=vnifinodegroupscale-v1alpha1.kb.io,admissionReviewVersions=v1

// SetupNiFiNodeGroupScaleWebhookWithManager registers the /scale subresource validator. It is
// registered directly on the webhook server, not through ctrl.NewWebhookManagedBy: a subresource
// webhook's request Object is the generic autoscaling/v1 Scale type (what specpath/statuspath map
// the NiFiNodeGroup's own fields to and from), never a NiFiNodeGroup, so it does not fit
// webhook.CustomValidator, which is built around one Go type.
func SetupNiFiNodeGroupScaleWebhookWithManager(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register(
		"/validate-nifi-ksolves-com-v1alpha1-nifinodegroup-scale",
		&webhook.Admission{Handler: &NiFiNodeGroupScaleValidator{Client: mgr.GetClient(), decoder: admission.NewDecoder(mgr.GetScheme())}},
	)
}

// NiFiNodeGroupScaleValidator checks a NiFiNodeGroup's /scale subresource against the
// cluster-wide node limit (NIFI-248 B1(b)). Neither of the limit's other two defenses sees this
// write: NiFiNodeGroupCustomValidator.ValidateUpdate never runs for a subresource request, and the
// CRD's own per-field CEL rule on spec.replicas only ever sees this one object's own value, with
// no way to add in its siblings' - so kubectl scale, or an HPA/KEDA driving this group on its own,
// could push the cluster's total node count past MaxClusterNodes without either of those two
// noticing. This is the one of the three checks that can actually refuse the write at admission
// time, rather than merely limiting its effect afterwards (capReplicasOverLimit,
// NiFiClusterReconciler.enforceNodeLimit) - so, unlike those two, it is not a backstop: a
// NiFiNodeGroup updated while this webhook is unreachable or disabled still only ever hits the
// backstops, exactly as before this webhook existed.
type NiFiNodeGroupScaleValidator struct {
	Client  client.Client
	decoder admission.Decoder
}

var _ admission.Handler = &NiFiNodeGroupScaleValidator{}

// Handle implements admission.Handler.
func (v *NiFiNodeGroupScaleValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	scale := &autoscalingv1.Scale{}
	if err := v.decoder.DecodeRaw(req.Object, scale); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding Scale: %w", err))
	}

	ng := &nifiv1alpha1.NiFiNodeGroup{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: req.Name, Namespace: req.Namespace}, ng); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted between the scale request being issued and admission seeing it: nothing
			// left to protect.
			return admission.Allowed("")
		}
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("getting NiFiNodeGroup %s/%s: %w", req.Namespace, req.Name, err))
	}

	oldTotal, newTotal, err := v.clusterTotals(ctx, ng, scale.Spec.Replicas)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	// Ratchet, like the CRD's own CEL rule on the cluster-wide total (NIFI-248 B8/B9): over the
	// limit, a request that does not raise the total any further - a no-op (an HPA re-submits its
	// current desired replicas every sync period, whether or not anything changed) or a decrease -
	// is never refused, even though the total it leaves behind is still over the limit. Only an
	// increase past the limit is.
	if newTotal > nifiv1alpha1.MaxClusterNodes && newTotal > oldTotal {
		return admission.Denied(fmt.Sprintf(
			"scaling NiFiNodeGroup %q to %d would give NiFiCluster %q %d NiFi nodes in total, across "+
				"all node groups and NiFiNodeGroups; at most %d are allowed",
			ng.Name, scale.Spec.Replicas, ng.Spec.ClusterRef, newTotal, nifiv1alpha1.MaxClusterNodes))
	}
	return admission.Allowed("")
}

// clusterTotals returns ng's own cluster's current total node count, and what that total would
// become if ng's replicas changed to newReplicas: every inline node group's replicas, plus every
// sibling NiFiNodeGroup's (ng's own replicas counted as they are now for oldTotal, as newReplicas
// for newTotal; everyone else's the same in both). A NiFiCluster that does not exist (yet, or
// anymore) has nothing else to sum against: reconcileCoordinationMigration's and the main
// validator's own existing checks handle that case, so both totals are just newReplicas, letting
// the write through rather than guessing.
func (v *NiFiNodeGroupScaleValidator) clusterTotals(ctx context.Context, ng *nifiv1alpha1.NiFiNodeGroup, newReplicas int32) (oldTotal, newTotal int32, err error) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: ng.Spec.ClusterRef, Namespace: ng.Namespace}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return newReplicas, newReplicas, nil
		}
		return 0, 0, fmt.Errorf("getting NiFiCluster %s/%s: %w", ng.Namespace, ng.Spec.ClusterRef, err)
	}

	for i := range cluster.Spec.NodeGroups {
		oldTotal += cluster.Spec.NodeGroups[i].Replicas
	}
	newTotal = oldTotal

	siblings := &nifiv1alpha1.NiFiNodeGroupList{}
	if err := v.Client.List(ctx, siblings, client.InNamespace(ng.Namespace)); err != nil {
		return 0, 0, fmt.Errorf("listing NiFiNodeGroups: %w", err)
	}
	for i := range siblings.Items {
		o := &siblings.Items[i]
		if o.Spec.ClusterRef != cluster.Name || !o.DeletionTimestamp.IsZero() {
			continue
		}
		if o.Name == ng.Name {
			oldTotal += o.Spec.Replicas
			newTotal += newReplicas
			continue
		}
		oldTotal += o.Spec.Replicas
		newTotal += o.Spec.Replicas
	}
	return oldTotal, newTotal, nil
}
