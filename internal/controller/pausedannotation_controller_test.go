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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// Earlier versions paused reconciliation with nifi.ksolves.com/reconcile-paused; it is ignored now.
var _ = Describe("NiFiCluster with the former reconcile-paused annotation", func() {
	const (
		namespace   = "default"
		clusterName = "paused-ignored"
		timeout     = 10 * time.Second
		interval    = 100 * time.Millisecond
	)
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	AfterEach(func() {
		c := &nifiv1alpha1.NiFiCluster{}
		if err := k8sClient.Get(ctx, key(clusterName), c); err == nil {
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName), c)) }, timeout, interval).Should(BeTrue())
		}
		deleteLeftovers(clusterName)
	})

	It("keeps reconciling and drops the ReconciliationPaused condition", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace,
				Annotations: map[string]string{"nifi.ksolves.com/reconcile-paused": "true"}},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := key(clusterName + "-default")
		Eventually(func() error { return k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{}) }, timeout, interval).Should(Succeed())

		// An out-of-band edit is reverted, as on any cluster.
		Eventually(func() error {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return err
			}
			three := int32(3)
			sts.Spec.Replicas = &three
			return k8sClient.Update(ctx, sts)
		}, timeout, interval).Should(Succeed())
		// Reverting 3 -> 1 takes two scale-down steps, each confirming its node OFFLOADED first
		// (NIFI-248 B4) before advancing - up to offloadRequeueInterval per step, so this needs
		// more than the usual timeout.
		Eventually(func() int32 {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return -1
			}
			return *sts.Spec.Replicas
		}, 25*time.Second, interval).Should(Equal(int32(1)))

		// A condition left over from an earlier version is removed.
		Eventually(func() error {
			c := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), c); err != nil {
				return err
			}
			apimeta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: "ReconciliationPaused", Status: metav1.ConditionTrue, Reason: "AnnotationSet", Message: "paused",
			})
			return k8sClient.Status().Update(ctx, c)
		}, timeout, interval).Should(Succeed())
		// The status write re-queues the cluster, whose next status write drops it.
		Eventually(func() *metav1.Condition {
			c := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), c); err != nil {
				return nil
			}
			return apimeta.FindStatusCondition(c.Status.Conditions, "ReconciliationPaused")
		}, timeout, interval).Should(BeNil())
	})
})
