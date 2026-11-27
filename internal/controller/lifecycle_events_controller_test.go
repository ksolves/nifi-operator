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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// Covers the Scaling/RollingRestart/Upgrade* events - CertRotated is covered alongside the
// existing TLS rotation test (nificluster_controller_test.go), and NodeRemoved alongside the
// existing node-removal tests (noderemoval_test.go/noderemoval_auth_test.go), since both already
// set up exactly the scenario those events need.
var _ = Describe("NiFiCluster lifecycle events", func() {
	const (
		namespace = "default"
		nodeGroup = "default"
		timeout   = 10 * time.Second
		interval  = 100 * time.Millisecond
	)

	var clusterName string

	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: namespace}
	}

	AfterEach(func() {
		c := &nifiv1alpha1.NiFiCluster{}
		if err := k8sClient.Get(ctx, key(clusterName), c); err == nil {
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName), c))
			}, timeout, interval).Should(BeTrue())
		}
		deleteLeftovers(clusterName)
	})

	hasEvent := func(reason, eventType string) bool {
		events := &corev1.EventList{}
		if err := k8sClient.List(ctx, events, client.InNamespace(namespace)); err != nil {
			return false
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Name == clusterName && e.Reason == reason && e.Type == eventType {
				return true
			}
		}
		return false
	}

	It("emits a Scaling event when a node group's replica count changes", func() {
		clusterName = "events-scale"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: nodeGroup, Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, key(clusterName+"-"+nodeGroup), &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())

		Expect(hasEvent(reasonScaling, corev1.EventTypeNormal)).To(BeFalse(), "Scaling fired on initial creation, not just on a real replica change")

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].Replicas = 3
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool { return hasEvent(reasonScaling, corev1.EventTypeNormal) }, timeout, interval).Should(BeTrue())
	})

	It("emits a RollingRestart event when the pod template changes for a non-image reason", func() {
		clusterName = "events-restart"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: nodeGroup, Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, key(clusterName+"-"+nodeGroup), &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), latest); err != nil {
				return err
			}
			latest.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
				NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "60 secs"},
			}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool { return hasEvent(reasonRollingRestart, corev1.EventTypeNormal) }, timeout, interval).Should(BeTrue())
		Expect(hasEvent(reasonUpgradeStarted, corev1.EventTypeNormal)).To(BeFalse(), "a config-only template change must not also look like an image upgrade")
	})

	It("emits UpgradeStarted and sets Upgrading=True on an image change, then UpgradeCompleted once ready", func() {
		clusterName = "events-upgrade"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: nodeGroup, Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, key(clusterName+"-"+nodeGroup), &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), latest); err != nil {
				return err
			}
			latest.Spec.Image.Tag = "2.12.1"
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool { return hasEvent(reasonUpgradeStarted, corev1.EventTypeNormal) }, timeout, interval).Should(BeTrue())
		Eventually(func() *metav1.Condition {
			c := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), c); err != nil {
				return nil
			}
			return apimeta.FindStatusCondition(c.Status.Conditions, upgradingConditionType)
		}, timeout, interval).Should(HaveField("Status", metav1.ConditionTrue))

		// Regression for a real bug caught live on minikube: a pod still on the *old* revision
		// being Ready can make ReadyReplicas alone equal the desired count mid-rollout, before any
		// pod has actually moved to the new image - UpdatedReplicas lagging behind must withhold
		// UpgradeCompleted even though ReadyReplicas already matches.
		Eventually(func() error {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, key(clusterName+"-"+nodeGroup), sts); err != nil {
				return err
			}
			sts.Status.Replicas = *sts.Spec.Replicas
			sts.Status.ReadyReplicas = *sts.Spec.Replicas
			sts.Status.UpdatedReplicas = 0
			return k8sClient.Status().Update(ctx, sts)
		}, timeout, interval).Should(Succeed())

		Consistently(func() bool { return hasEvent(reasonUpgradeCompleted, corev1.EventTypeNormal) }, "2s", interval).Should(BeFalse(),
			"UpgradeCompleted fired while UpdatedReplicas still lagged ReadyReplicas - a pod on the old image was counted as done")

		// A second, subtler regression also caught live: Ready/UpdatedReplicas can BOTH already
		// match the desired count while still describing the *previous* template, for the one
		// reconcile right after a spec change, before the StatefulSet controller has processed the
		// new generation at all (ObservedGeneration lags Generation). Leaving ObservedGeneration
		// unset here (it defaults to 0) simulates exactly that stale read.
		Eventually(func() error {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, key(clusterName+"-"+nodeGroup), sts); err != nil {
				return err
			}
			sts.Status.Replicas = *sts.Spec.Replicas
			sts.Status.ReadyReplicas = *sts.Spec.Replicas
			sts.Status.UpdatedReplicas = *sts.Spec.Replicas
			sts.Status.ObservedGeneration = 0
			return k8sClient.Status().Update(ctx, sts)
		}, timeout, interval).Should(Succeed())

		Consistently(func() bool { return hasEvent(reasonUpgradeCompleted, corev1.EventTypeNormal) }, "2s", interval).Should(BeFalse(),
			"UpgradeCompleted fired from a stale StatefulSet read (ObservedGeneration hadn't caught up to Generation yet)")

		// envtest has no kubelet, so the StatefulSet's pods never actually report Ready/updated on
		// their own - fabricate everything updateStatus reads to decide the rollout genuinely
		// finished, including ObservedGeneration catching up to the StatefulSet's own Generation.
		Eventually(func() error {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, key(clusterName+"-"+nodeGroup), sts); err != nil {
				return err
			}
			sts.Status.Replicas = *sts.Spec.Replicas
			sts.Status.ReadyReplicas = *sts.Spec.Replicas
			sts.Status.UpdatedReplicas = *sts.Spec.Replicas
			sts.Status.ObservedGeneration = sts.Generation
			return k8sClient.Status().Update(ctx, sts)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool { return hasEvent(reasonUpgradeCompleted, corev1.EventTypeNormal) }, timeout, interval).Should(BeTrue())
		Eventually(func() *metav1.Condition {
			c := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), c); err != nil {
				return nil
			}
			return apimeta.FindStatusCondition(c.Status.Conditions, upgradingConditionType)
		}, timeout, interval).Should(HaveField("Status", metav1.ConditionFalse))
	})
})
