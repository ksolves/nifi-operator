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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// Coordination RBAC against envtest's real RBAC authorizer.
var _ = Describe("Kubernetes coordination RBAC", func() {
	const namespace = "default"
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	newCluster := func(name string, groups ...nifiv1alpha1.NodeGroup) *nifiv1alpha1.NiFiCluster {
		return &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: groups,
			},
		}
	}
	cleanup := func(c *nifiv1alpha1.NiFiCluster) {
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			deleteLeftovers(c.Name)
		})
	}
	rbacCondition := func(name string) *metav1.Condition {
		c := &nifiv1alpha1.NiFiCluster{}
		if err := k8sClient.Get(ctx, key(name), c); err != nil {
			return nil
		}
		return apimeta.FindStatusCondition(c.Status.Conditions, coordinationRBACConditionType)
	}

	It("reports a node group's own ServiceAccount without the Role, and clears once it is bound by hand", func() {
		const name = "rbac-custom-sa"
		c := newCluster(name,
			nifiv1alpha1.NodeGroup{Name: "team", Replicas: 1, Storage: "1Gi",
				PodOverrides: &nifiv1alpha1.PodOverridesSpec{ServiceAccountName: "team-sa"}})
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		cleanup(c)

		Eventually(func(g Gomega) {
			cond := rbacCondition(name)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal(reasonCoordinationRBACMissing))
			g.Expect(cond.Message).To(ContainSubstring(`"team-sa" (node groups team)`))
			g.Expect(cond.Message).NotTo(ContainSubstring(`"` + name + `-nifi"`))
		}, time.Minute, 500*time.Millisecond).Should(Succeed())

		By("binding the cluster's coordination Role to team-sa by hand")
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-team-sa", Namespace: namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name + "-nifi"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "team-sa", Namespace: namespace}},
		}
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, binding) })
		Eventually(func(g Gomega) {
			cond := rbacCondition(name)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		}, time.Minute, 500*time.Millisecond).Should(Succeed())
	})

	It("keeps two clusters in one namespace apart: own Role, RoleBinding and Lease/state prefix", func() {
		names := []string{"iso-one", "iso-two"}
		for _, n := range names {
			c := newCluster(n, nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"})
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			cleanup(c)
		}
		for _, n := range names {
			Eventually(func(g Gomega) {
				binding := &rbacv1.RoleBinding{}
				g.Expect(k8sClient.Get(ctx, key(n+"-nifi"), binding)).To(Succeed())
				g.Expect(binding.RoleRef.Name).To(Equal(n + "-nifi"))
				g.Expect(binding.Subjects).To(ConsistOf(rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: n + "-nifi", Namespace: namespace}))
				cm := &corev1.ConfigMap{}
				g.Expect(k8sClient.Get(ctx, key(n+"-default-config"), cm)).To(Succeed())
				all := ""
				for _, v := range cm.Data {
					all += v + "\n"
				}
				g.Expect(all).To(ContainSubstring(nifiPropLeaderElectionLeasePrefix + "=" + n + "\n"))
			}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		}

		By("deleting the first cluster, which must leave the second one's RBAC alone")
		first := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(names[0]), first)).To(Succeed())
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, key(names[0]), first)) }, time.Minute, 200*time.Millisecond).Should(BeTrue())
		Expect(k8sClient.Get(ctx, key(names[1]+"-nifi"), &rbacv1.Role{})).To(Succeed())
		Expect(k8sClient.Get(ctx, key(names[1]+"-nifi"), &rbacv1.RoleBinding{})).To(Succeed())
		Expect(strings.HasPrefix(names[1], names[0])).To(BeFalse(), "test names must not share a prefix")
	})
})
