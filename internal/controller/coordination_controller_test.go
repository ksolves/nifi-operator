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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

var _ = Describe("NiFiCluster coordination", func() {
	const (
		namespace = "default"
		timeout   = 10 * time.Second
		interval  = 100 * time.Millisecond
	)

	var clusterName string

	newCluster := func(name string, groups ...nifiv1alpha1.NodeGroup) *nifiv1alpha1.NiFiCluster {
		return &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: groups,
			},
		}
	}
	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: namespace}
	}
	getCluster := func() *nifiv1alpha1.NiFiCluster {
		c := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(clusterName), c)).To(Succeed())
		return c
	}
	coordinationCondition := func() *metav1.Condition {
		c := &nifiv1alpha1.NiFiCluster{}
		if err := k8sClient.Get(ctx, key(clusterName), c); err != nil {
			return nil
		}
		return apimeta.FindStatusCondition(c.Status.Conditions, coordinationConditionType)
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

	It("creates the NiFi pods' ServiceAccount, Role and RoleBinding, owned by the cluster", func() {
		clusterName = "coord-rbac"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}))).To(Succeed())

		sa := &corev1.ServiceAccount{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-nifi"), sa) }, timeout, interval).Should(Succeed())
		role := &rbacv1.Role{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-nifi"), role) }, timeout, interval).Should(Succeed())
		binding := &rbacv1.RoleBinding{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-nifi"), binding) }, timeout, interval).Should(Succeed())

		cluster := getCluster()
		for _, obj := range []client.Object{sa, role, binding} {
			Expect(metav1.IsControlledBy(obj, cluster)).To(BeTrue(), "%T %s not controlled by the NiFiCluster", obj, obj.GetName())
		}
		Expect(role.Rules).To(Equal(nifiCoordinationRules()))
		Expect(binding.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: clusterName + "-nifi"}))
		Expect(binding.Subjects).To(ConsistOf(rbacv1.Subject{
			Kind: rbacv1.ServiceAccountKind, Name: clusterName + "-nifi", Namespace: namespace,
		}))

		sts := &appsv1.StatefulSet{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-default"), sts) }, timeout, interval).Should(Succeed())
		Expect(sts.Spec.Template.Spec.ServiceAccountName).To(Equal(clusterName + "-nifi"))
		Expect(sts.Annotations).To(HaveKeyWithValue(coordinationBackendAnnotation, "Kubernetes"))
	})

	It("binds the Role to a user-supplied ServiceAccount instead of creating one", func() {
		clusterName = "coord-user-sa"
		cluster := newCluster(clusterName, nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"})
		cluster.Spec.ServiceAccountName = "my-nifi"
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		binding := &rbacv1.RoleBinding{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-nifi"), binding) }, timeout, interval).Should(Succeed())
		Expect(binding.Subjects).To(ConsistOf(rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "my-nifi", Namespace: namespace}))
		Consistently(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName+"-nifi"), &corev1.ServiceAccount{}))
		}, time.Second, interval).Should(BeTrue())
	})

	It("runs every node group as one cluster with one sensitive-props key", func() {
		clusterName = "coord-shared"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "alpha", Replicas: 2, Storage: "1Gi"},
		))).To(Succeed())
		createNodeGroupObject(namespace, clusterName, nifiv1alpha1.NodeGroup{Name: "beta", Replicas: 1, Storage: "1Gi"})

		for _, group := range []string{"alpha", "beta"} {
			cm := &corev1.ConfigMap{}
			Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-"+group+"-config"), cm) }, timeout, interval).Should(Succeed())
			props := cm.Data["nifi.properties.overrides"]
			Expect(props).To(ContainSubstring("nifi.cluster.leader.election.kubernetes.lease.prefix=" + clusterName + "\n"))
			Expect(props).To(ContainSubstring("nifi.cluster.flow.election.max.candidates=3\n"))
			Expect(cm.Data).NotTo(HaveKey("zookeeper.properties.overrides"))

			sts := &appsv1.StatefulSet{}
			Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-"+group), sts) }, timeout, interval).Should(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Env).To(ContainElement(HaveField("ValueFrom.SecretKeyRef.Name", clusterName+"-sensitive-props")))
			for _, p := range sts.Spec.Template.Spec.Containers[0].Ports {
				Expect(p.Name).NotTo(HavePrefix("zk-"), "embedded ZooKeeper port still exposed")
			}
		}

		Expect(k8sClient.Get(ctx, key(clusterName+"-sensitive-props"), &corev1.Secret{})).To(Succeed())
		Eventually(func() metav1.ConditionStatus {
			if c := coordinationCondition(); c != nil {
				return c.Status
			}
			return ""
		}, timeout, interval).Should(Equal(metav1.ConditionTrue))
		Expect(getCluster().Status.CoordinationBackend).To(Equal(nifiv1alpha1.CoordinationBackendKubernetes))
	})

	It("does not roll the pods when only the replica count changes", func() {
		clusterName = "coord-scale"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "default", Replicas: 3, Storage: "1Gi"}))).To(Succeed())

		sts := &appsv1.StatefulSet{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-default"), sts) }, timeout, interval).Should(Succeed())
		originalHash := sts.Spec.Template.Annotations[configHashAnnotation]

		Eventually(func() error {
			c := getCluster()
			c.Spec.NodeGroups[0].Replicas = 2
			return k8sClient.Update(ctx, c)
		}, timeout, interval).Should(Succeed())

		Eventually(func() int32 {
			if err := k8sClient.Get(ctx, key(clusterName+"-default"), sts); err != nil {
				return -1
			}
			return *sts.Spec.Replicas
		}, timeout, interval).Should(Equal(int32(2)))
		Expect(sts.Spec.Template.Annotations[configHashAnnotation]).To(Equal(originalHash))

		cm := &corev1.ConfigMap{}
		Eventually(func() string {
			if err := k8sClient.Get(ctx, key(clusterName+"-default-config"), cm); err != nil {
				return ""
			}
			return cm.Data["nifi.properties.overrides"]
		}, timeout, interval).Should(ContainSubstring("nifi.cluster.flow.election.max.candidates=2\n"))
	})

	It("adopts a legacy per-node-group sensitive-props key", func() {
		clusterName = "coord-adopt"
		legacy := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-default-sensitive-props", Namespace: namespace},
			StringData: map[string]string{sensitivePropsKeyDataKey: "legacy-key-0123456789abcdef"},
		}
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })

		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}))).To(Succeed())

		secret := &corev1.Secret{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-sensitive-props"), secret) }, timeout, interval).Should(Succeed())
		Expect(string(secret.Data[sensitivePropsKeyDataKey])).To(Equal("legacy-key-0123456789abcdef"))
	})

	It("refuses to merge node groups whose legacy sensitive-props keys differ", func() {
		clusterName = "coord-conflict"
		for group, value := range map[string]string{"alpha": "legacy-key-aaaaaaaaaaaa", "beta": "legacy-key-bbbbbbbbbbbb"} {
			legacy := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-" + group + "-sensitive-props", Namespace: namespace},
				StringData: map[string]string{sensitivePropsKeyDataKey: value},
			}
			Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })
		}

		createNodeGroupObject(namespace, clusterName, nifiv1alpha1.NodeGroup{Name: "beta", Replicas: 1, Storage: "1Gi"})
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "alpha", Replicas: 1, Storage: "1Gi"},
		))).To(Succeed())

		Eventually(func() string {
			if c := coordinationCondition(); c != nil {
				return c.Reason
			}
			return ""
		}, timeout, interval).Should(Equal(reasonConflictingSensitivePropKey))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName+"-sensitive-props"), &corev1.Secret{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName+"-alpha"), &appsv1.StatefulSet{}))).To(BeTrue())
	})

	It("holds a legacy cluster untouched until the migration is approved, then migrates it", func() {
		clusterName = "coord-legacy"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "default", Replicas: 2, Storage: "1Gi"}))).To(Succeed())

		sts := &appsv1.StatefulSet{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-default"), sts) }, timeout, interval).Should(Succeed())

		// Simulate a StatefulSet written by an operator version without shared coordination.
		Eventually(func() error {
			if err := k8sClient.Get(ctx, key(clusterName+"-default"), sts); err != nil {
				return err
			}
			delete(sts.Annotations, coordinationBackendAnnotation)
			return k8sClient.Update(ctx, sts)
		}, timeout, interval).Should(Succeed())

		Eventually(func() string {
			if c := coordinationCondition(); c != nil {
				return c.Reason
			}
			return ""
		}, timeout, interval).Should(Equal(reasonMigrationAwaitingApproval))
		Expect(coordinationCondition().Message).To(ContainSubstring(approveCoordinationMigrationAnnotation))
		Eventually(func() bool {
			events := &corev1.EventList{}
			if err := k8sClient.List(ctx, events, client.InNamespace(namespace)); err != nil {
				return false
			}
			for _, e := range events.Items {
				if e.InvolvedObject.Name == clusterName && e.Reason == reasonMigrationAwaitingApproval && e.Type == corev1.EventTypeWarning {
					return true
				}
			}
			return false
		}, timeout, interval).Should(BeTrue(), "no MigrationAwaitingApproval Warning event recorded")

		// Untouched while unapproved: no stamp, replicas unchanged.
		Consistently(func() bool {
			if err := k8sClient.Get(ctx, key(clusterName+"-default"), sts); err != nil {
				return false
			}
			_, stamped := sts.Annotations[coordinationBackendAnnotation]
			return !stamped && *sts.Spec.Replicas == 2
		}, 2*time.Second, interval).Should(BeTrue())

		// An approval naming some other value is not an approval for this migration.
		Eventually(func() error {
			c := getCluster()
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[approveCoordinationMigrationAnnotation] = "true"
			return k8sClient.Update(ctx, c)
		}, timeout, interval).Should(Succeed())
		Consistently(func() string {
			if c := coordinationCondition(); c != nil {
				return c.Reason
			}
			return ""
		}, 2*time.Second, interval).Should(Equal(reasonMigrationAwaitingApproval))

		// envtest runs no StatefulSet controller, so stand in a NiFi pod by hand: the operator
		// must scale to zero and then wait for every pod of the cluster to be gone.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: clusterName + "-default-0", Namespace: namespace,
				Labels: map[string]string{labelAppName: appNameValue, labelAppInstance: clusterName, labelNodeGroup: "default"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nifi", Image: "nifi"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		Eventually(func() error {
			c := getCluster()
			c.Annotations[approveCoordinationMigrationAnnotation] = string(nifiv1alpha1.CoordinationBackendKubernetes)
			return k8sClient.Update(ctx, c)
		}, timeout, interval).Should(Succeed())

		Eventually(func() string {
			if c := coordinationCondition(); c != nil {
				return c.Reason
			}
			return ""
		}, timeout, interval).Should(Equal(reasonMigrationStoppingNodes))
		Expect(k8sClient.Get(ctx, key(clusterName+"-default"), sts)).To(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(0)))
		Expect(sts.Annotations).NotTo(HaveKey(coordinationBackendAnnotation), "rewritten before all pods stopped")

		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())

		Eventually(func() bool {
			if err := k8sClient.Get(ctx, key(clusterName+"-default"), sts); err != nil {
				return false
			}
			return sts.Annotations[coordinationBackendAnnotation] == "Kubernetes" && *sts.Spec.Replicas == 2
		}, 2*migrationPollInterval, interval).Should(BeTrue())
		Eventually(func() string {
			if c := coordinationCondition(); c != nil {
				return c.Reason
			}
			return ""
		}, timeout, interval).Should(Equal(reasonCoordinationUpToDate))
	})

	It("rejects removing coordination.backend once set", func() {
		clusterName = "coord-cel"
		cluster := newCluster(clusterName, nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"})
		cluster.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{Backend: nifiv1alpha1.CoordinationBackendKubernetes}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		// Retried only past optimistic-concurrency conflicts (the controller adds its finalizer
		// concurrently); the first real answer from the API server must be Invalid.
		var err error
		Eventually(func() bool {
			c := getCluster()
			c.Spec.Coordination = nil
			err = k8sClient.Update(ctx, c)
			return !apierrors.IsConflict(err)
		}, timeout, interval).Should(BeTrue())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
		Expect(strings.Contains(err.Error(), "immutable")).To(BeTrue())
	})

	It("allows setting coordination on a cluster that never chose a backend", func() {
		clusterName = "coord-cel-first"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}))).To(Succeed())

		Eventually(func() error {
			c := getCluster()
			c.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{Backend: nifiv1alpha1.CoordinationBackendKubernetes}
			return k8sClient.Update(ctx, c)
		}, timeout, interval).Should(Succeed())
	})
})
