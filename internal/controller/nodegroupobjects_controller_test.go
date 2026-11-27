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
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// NiFiNodeGroup objects against envtest: a NiFiNodeGroup is reconciled as a node group of its cluster and can be
// scaled through its /scale subresource.
var _ = Describe("NiFiNodeGroup", func() {
	const ns = "default"
	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
	stsReplicas := func(name string) int32 {
		sts := &appsv1.StatefulSet{}
		if err := k8sClient.Get(ctx, key(name), sts); err != nil || sts.Spec.Replicas == nil {
			return -1
		}
		return *sts.Spec.Replicas
	}
	newGroup := func(name, cluster string, replicas int32) *nifiv1alpha1.NiFiNodeGroup {
		return &nifiv1alpha1.NiFiNodeGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: nifiv1alpha1.NiFiNodeGroupSpec{ClusterRef: cluster,
				// envtest runs no webhook: spec.name is set here as the defaulting webhook would.
				NodeGroup: nifiv1alpha1.NodeGroup{Name: name, Replicas: replicas, Storage: "1Gi"}},
		}
	}
	// scaleTo retries the Get-then-Update the /scale subresource needs on a conflict: the
	// controller reconciles this same NiFiNodeGroup concurrently (status, owner reference), so the
	// resourceVersion a Get just returned can already be stale by the time Update reaches the API
	// server. Only a conflict is retried - any other error (the CRD's own per-field limit,
	// Minimum=1) is returned straight away, exactly as a single Get+Update would have.
	scaleTo := func(group *nifiv1alpha1.NiFiNodeGroup, replicas int32) error {
		var err error
		for range 20 {
			scale := &autoscalingv1.Scale{}
			if err = k8sClient.SubResource("scale").Get(ctx, group, scale); err != nil {
				return err
			}
			scale.Spec.Replicas = replicas
			if err = k8sClient.SubResource("scale").Update(ctx, group, client.WithSubResourceBody(scale)); !apierrors.IsConflict(err) {
				return err
			}
		}
		return err
	}

	It("runs a NiFiNodeGroup as a node group of its cluster, scaled through /scale", func() {
		const name = "nng-scale"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "main", Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		group := newGroup("workers", name, 1)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, group)
			_ = k8sClient.Delete(ctx, cluster)
			deleteLeftovers(name)
		})

		By("creating the group's StatefulSet beside the inline group's")
		Eventually(func() int32 { return stsReplicas(name + "-workers") }, 30*time.Second, 200*time.Millisecond).Should(Equal(int32(1)))
		Expect(stsReplicas(name + "-main")).To(Equal(int32(1)))

		By("writing the group's status, owner and selector, with the replicas its StatefulSet has")
		// envtest runs no StatefulSet controller: write the status a real one would.
		Eventually(func() error {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, key(name+"-workers"), sts); err != nil {
				return err
			}
			sts.Status.Replicas = 1
			return k8sClient.Status().Update(ctx, sts)
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Eventually(func(g Gomega) {
			o := &nifiv1alpha1.NiFiNodeGroup{}
			g.Expect(k8sClient.Get(ctx, key("workers"), o)).To(Succeed())
			g.Expect(o.Status.Replicas).To(Equal(int32(1)))
			g.Expect(o.Status.Selector).To(Equal(
				"app.kubernetes.io/instance=" + name + ",app.kubernetes.io/name=nifi,nifi.ksolves.com/node-group=workers"))
			g.Expect(o.OwnerReferences).To(ContainElement(HaveField("Name", name)))
			g.Expect(apimeta.FindStatusCondition(o.Status.Conditions, nodeGroupReadyConditionType)).NotTo(BeNil())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("never writing the merged node groups into the NiFiCluster")
		stored := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(name), stored)).To(Succeed())
		Expect(stored.Spec.NodeGroups).To(HaveLen(1))
		Expect(stored.Status.NodeGroupStatuses).To(HaveLen(2))

		By("scaling through the /scale subresource, as kubectl scale and an HPA do")
		scale := &autoscalingv1.Scale{}
		Expect(k8sClient.SubResource("scale").Get(ctx, group, scale)).To(Succeed())
		Expect(scale.Spec.Replicas).To(Equal(int32(1)))
		Expect(scale.Status.Selector).To(ContainSubstring("nifi.ksolves.com/node-group=workers"))
		Expect(scaleTo(group, 2)).To(Succeed())
		Eventually(func() int32 { return stsReplicas(name + "-workers") }, 30*time.Second, 200*time.Millisecond).Should(Equal(int32(2)))
		Expect(stsReplicas(name + "-main")).To(Equal(int32(1)))

		By("refusing more than 3 replicas in one group: /scale is checked against the CRD schema")
		err := scaleTo(group, 4)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "scale to 4: %v", err)

		// NIFI-248 B1(b): a real cluster's new NiFiNodeGroupScaleValidator webhook would refuse
		// this exact scale (it would know "main" already uses 1 node), but this envtest runs no
		// webhook at all (see suite_test.go), so it still exercises the controller's own backstop:
		// capReplicasOverLimit holds this group's StatefulSet at its current replicas rather than
		// advancing to the new, over-limit desired one.
		By("holding the StatefulSet at its current replicas while /scale takes the cluster past 3 nodes")
		Expect(scaleTo(group, 3)).To(Succeed())
		Eventually(func(g Gomega) {
			c := &nifiv1alpha1.NiFiCluster{}
			g.Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
			cond := apimeta.FindStatusCondition(c.Status.Conditions, degradedConditionType)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(reasonNodeLimitExceeded))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Consistently(func() int32 { return stsReplicas(name + "-workers") }, 2*time.Second, 200*time.Millisecond).Should(Equal(int32(2)))
		// The NiFiNodeGroup's status still follows its StatefulSet, as for a paused cluster.
		Expect(k8sClient.Get(ctx, key("workers"), group)).To(Succeed())
		Expect(apimeta.FindStatusCondition(group.Status.Conditions, nodeGroupReadyConditionType)).NotTo(BeNil())

		By("reconciling again once the total is back within the limit")
		Expect(scaleTo(group, 1)).To(Succeed())
		Eventually(func() int32 { return stsReplicas(name + "-workers") }, 30*time.Second, 200*time.Millisecond).Should(Equal(int32(1)))
		Eventually(func(g Gomega) {
			c := &nifiv1alpha1.NiFiCluster{}
			g.Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
			cond := apimeta.FindStatusCondition(c.Status.Conditions, degradedConditionType)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("refusing a scale to zero")
		err = scaleTo(group, 0)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "scale to 0: %v", err)

		By("removing the group's StatefulSet when the NiFiNodeGroup is deleted")
		Expect(k8sClient.Delete(ctx, group)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-workers"), &appsv1.StatefulSet{}))
		}, 30*time.Second, 200*time.Millisecond).Should(BeTrue())
		Expect(stsReplicas(name + "-main")).To(Equal(int32(1)))
	})

	It("reports a NiFiNodeGroup named like an inline node group as a conflict and leaves it alone", func() {
		const name = "nng-conflict"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "main", Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		group := newGroup("main", name, 3)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, group)
			_ = k8sClient.Delete(ctx, cluster)
			deleteLeftovers(name)
		})
		Eventually(func(g Gomega) {
			o := &nifiv1alpha1.NiFiNodeGroup{}
			g.Expect(k8sClient.Get(ctx, key("main"), o)).To(Succeed())
			c := apimeta.FindStatusCondition(o.Status.Conditions, nodeGroupConflictConditionType)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Consistently(func() int32 { return stsReplicas(name + "-main") }, 2*time.Second, 200*time.Millisecond).Should(Equal(int32(1)))
	})

	It("reports a NiFiNodeGroup whose cluster does not exist as a conflict until the cluster appears", func() {
		const name = "nng-orphan"
		group := newGroup("lonely", name, 2)
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, group)
			deleteLeftovers(name)
		})
		Eventually(func(g Gomega) {
			o := &nifiv1alpha1.NiFiNodeGroup{}
			g.Expect(k8sClient.Get(ctx, key("lonely"), o)).To(Succeed())
			c := apimeta.FindStatusCondition(o.Status.Conditions, nodeGroupConflictConditionType)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(c.Reason).To(Equal("ClusterNotFound"))
			g.Expect(o.Status.Replicas).To(BeZero())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "main", Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		Eventually(func(g Gomega) {
			o := &nifiv1alpha1.NiFiNodeGroup{}
			g.Expect(k8sClient.Get(ctx, key("lonely"), o)).To(Succeed())
			g.Expect(apimeta.FindStatusCondition(o.Status.Conditions, nodeGroupConflictConditionType)).To(BeNil())
			g.Expect(o.Status.Selector).NotTo(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Eventually(func() int32 { return stsReplicas(name + "-lonely") }, 30*time.Second, 200*time.Millisecond).Should(Equal(int32(2)))
	})
})
