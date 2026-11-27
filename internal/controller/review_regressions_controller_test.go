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
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// Regression specs for the 1 Oct 2026 code review, one per finding.
var _ = Describe("code review regressions", func() {
	const (
		namespace = "default"
		timeout   = 10 * time.Second
		interval  = 100 * time.Millisecond
	)

	var clusterName string
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
	updateCluster := func(mutate func(*nifiv1alpha1.NiFiCluster)) {
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), latest); err != nil {
				return err
			}
			mutate(latest)
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())
	}
	stsExists := func(name string) func() bool {
		return func() bool { return k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}) == nil }
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

	// H4: the StatefulSet write cache must never stop a deleted StatefulSet from being recreated.
	It("H4: recreates a StatefulSet deleted out of band and one whose node group was removed and re-added", func() {
		clusterName = "h4"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "a", Replicas: 1, Storage: "1Gi"},
		))).To(Succeed())
		Eventually(stsExists(clusterName+"-a"), timeout, interval).Should(BeTrue())

		// kubectl delete sts
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(clusterName+"-a"), sts)).To(Succeed())
		oldUID := sts.UID
		Expect(k8sClient.Delete(ctx, sts)).To(Succeed())
		Eventually(func() bool {
			fresh := &appsv1.StatefulSet{}
			return k8sClient.Get(ctx, key(clusterName+"-a"), fresh) == nil && fresh.UID != oldUID
		}, timeout, interval).Should(BeTrue())

		// Replace node group a by b, wait for a to be pruned, then bring a back unchanged.
		swap := func(name string) {
			updateCluster(func(c *nifiv1alpha1.NiFiCluster) {
				c.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: name, Replicas: 1, Storage: "1Gi"}}
			})
		}
		swap("b")
		Eventually(stsExists(clusterName+"-a"), timeout, interval).Should(BeFalse())
		Eventually(stsExists(clusterName+"-b"), timeout, interval).Should(BeTrue())
		swap("a")
		Eventually(stsExists(clusterName+"-a"), timeout, interval).Should(BeTrue())
	})

	// a cache hit must not hide a spec someone else changed. kubectl scale and kubectl edit
	// are put back; a kubectl rollout restart in progress is kept.
	It("reverts out-of-band replica and pod template changes, keeps a rollout restart", func() {
		clusterName = "drift"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "a", Replicas: 1, Storage: "1Gi"},
		))).To(Succeed())
		stsKey := key(clusterName + "-a")
		Eventually(stsExists(stsKey.Name), timeout, interval).Should(BeTrue())
		patchSts := func(mutate func(*appsv1.StatefulSet)) {
			Eventually(func() error {
				sts := &appsv1.StatefulSet{}
				if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
					return err
				}
				mutate(sts)
				return k8sClient.Update(ctx, sts)
			}, timeout, interval).Should(Succeed())
		}
		replicas := func() int32 {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil || sts.Spec.Replicas == nil {
				return -1
			}
			return *sts.Spec.Replicas
		}

		// kubectl scale sts --replicas=3
		patchSts(func(sts *appsv1.StatefulSet) { sts.Spec.Replicas = ptr.To(int32(3)) })
		// Reverting 3 -> 1 takes two scale-down steps, each confirming its node OFFLOADED first
		// (NIFI-248 B4) before advancing - up to offloadRequeueInterval per step, so this needs
		// more than the usual timeout.
		Eventually(replicas, 25*time.Second, interval).Should(Equal(int32(1)))

		// kubectl edit (an extra env var) together with kubectl rollout restart
		const restartedAt = "2026-10-06T12:00:00Z"
		patchSts(func(sts *appsv1.StatefulSet) {
			c := &sts.Spec.Template.Spec.Containers[0]
			c.Env = append(c.Env, corev1.EnvVar{Name: "DRIFT", Value: "1"})
			if sts.Spec.Template.Annotations == nil {
				sts.Spec.Template.Annotations = map[string]string{}
			}
			sts.Spec.Template.Annotations[restartedAtAnnotation] = restartedAt
		})
		hasDriftEnv := func(sts *appsv1.StatefulSet) bool {
			for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "DRIFT" {
					return true
				}
			}
			return false
		}
		sts := &appsv1.StatefulSet{}
		Eventually(func() bool {
			return k8sClient.Get(ctx, stsKey, sts) == nil && !hasDriftEnv(sts)
		}, timeout, interval).Should(BeTrue())
		Expect(sts.Spec.Template.Annotations).To(HaveKeyWithValue(restartedAtAnnotation, restartedAt))

		// Once put back, the operator leaves it alone: no further writes.
		settled := sts.Generation
		Consistently(func() int64 {
			fresh := &appsv1.StatefulSet{}
			_ = k8sClient.Get(ctx, stsKey, fresh)
			return fresh.Generation
		}, 2*time.Second, interval).Should(Equal(settled))
	})

	// A spec.labels-only edit leaves the pod template and replicas alone; the write cache must not
	// treat the StatefulSet as up to date (found during a failover test).
	It("applies a labels-only change to an existing StatefulSet", func() {
		clusterName = "metaonly"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "a", Replicas: 1, Storage: "1Gi"},
		))).To(Succeed())
		stsKey := key(clusterName + "-a")
		Eventually(stsExists(stsKey.Name), timeout, interval).Should(BeTrue())
		// Let the first reconciles settle so the write cache holds an entry.
		Consistently(stsExists(stsKey.Name), time.Second, interval).Should(BeTrue())

		updateCluster(func(c *nifiv1alpha1.NiFiCluster) { c.Spec.Labels = map[string]string{"team": "data"} })
		Eventually(func() string {
			sts := &appsv1.StatefulSet{}
			_ = k8sClient.Get(ctx, stsKey, sts)
			return sts.Labels["team"]
		}, timeout, interval).Should(Equal("data"))
	})

	// M14: NiFi's Leases carry no owner, so deleting the cluster must remove them. Component state
	// is kept on purpose (a same-name recreate resumes its flow), and nothing of another cluster's
	// is touched.
	It("M14: deletes the cluster's Leases on delete, keeps component state and other clusters' objects", func() {
		clusterName = "m14"
		Expect(k8sClient.Create(ctx, newCluster(clusterName, nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}))).To(Succeed())
		Eventually(stsExists(clusterName+"-default"), timeout, interval).Should(BeTrue())

		state := clusterName + "-nifi-component-f5fc97b3-01a0-1000-ffff-ffff81295497"
		for _, name := range []string{clusterName + "-cluster-coordinator", clusterName + "-primary-node", "m14x-cluster-coordinator"} {
			Expect(k8sClient.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: state, Namespace: namespace}})).To(Succeed())
		// The retained PVC the state belongs to (envtest runs no StatefulSet controller).
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-" + clusterName + "-default-0", Namespace: namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, pvc)
			_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: state, Namespace: namespace}})
			_ = k8sClient.Delete(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "m14x-cluster-coordinator", Namespace: namespace}})
		})

		c := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(clusterName), c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName), c)) }, timeout, interval).Should(BeTrue())

		for _, name := range []string{clusterName + "-cluster-coordinator", clusterName + "-primary-node"} {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &coordinationv1.Lease{}))).To(BeTrue(), "Lease %s left behind", name)
		}
		Expect(k8sClient.Get(ctx, key(state), &corev1.ConfigMap{})).To(Succeed(), "component state must be kept")
		Expect(k8sClient.Get(ctx, key("m14x-cluster-coordinator"), &coordinationv1.Lease{})).To(Succeed(), "another cluster's Lease was deleted")
	})

	// M8: a CA or sensitive-props Secret someone else created under the cluster's name is never
	// trusted.
	DescribeTable("M8: refuses a pre-planted Secret instead of using it",
		func(name, secretSuffix string, tls bool) {
			clusterName = name
			planted := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName + secretSuffix, Namespace: namespace},
				StringData: map[string]string{"planted": "yes"},
			}
			Expect(k8sClient.Create(ctx, planted)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, planted) })

			group := nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
			if tls {
				group.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			}
			Expect(k8sClient.Create(ctx, newCluster(clusterName, group))).To(Succeed())
			Consistently(stsExists(clusterName+"-default"), 2*time.Second, interval).Should(BeFalse())
			got := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key(planted.Name), got)).To(Succeed())
			Expect(got.OwnerReferences).To(BeEmpty())
			Expect(string(got.Data["planted"])).To(Equal("yes"))
		},
		Entry("TLS CA", "m8-ca", "-ca-certificate", true),
		Entry("sensitive-props key", "m8-key", "-sensitive-props", false),
	)

	// M17: the sensitive-props key outlives the cluster, so a same-name recreate on the retained
	// PVCs can still decrypt the flow - but only when those PVCs exist.
	It("M17: keeps the sensitive-props key across delete and recreate onto the retained PVCs", func() {
		clusterName = "m17"
		group := nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}
		Expect(k8sClient.Create(ctx, newCluster(clusterName, group))).To(Succeed())
		keyName := key(clusterName + "-sensitive-props")
		first := &corev1.Secret{}
		Eventually(func() error { return k8sClient.Get(ctx, keyName, first) }, timeout, interval).Should(Succeed())
		originalKey := string(first.Data["sensitive-props-key"])
		Expect(originalKey).NotTo(BeEmpty())

		// envtest runs no StatefulSet controller, so create the PVC a real one would have left.
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-" + clusterName + "-default-0", Namespace: namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pvc) })

		c := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(clusterName), c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName), c)) }, timeout, interval).Should(BeTrue())

		retained := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, keyName, retained)).To(Succeed())
		Expect(retained.OwnerReferences).To(BeEmpty())
		Expect(retained.Annotations).To(HaveKeyWithValue("nifi.ksolves.com/retained-for", clusterName))

		// envtest runs no garbage collector: delete what the old cluster owned, the way a real
		// cluster would once the owner is gone. The released key has no owner left, so it stays.
		oldUID := c.UID
		for _, list := range []client.ObjectList{
			&corev1.SecretList{}, &corev1.ConfigMapList{}, &corev1.ServiceList{}, &corev1.ServiceAccountList{},
			&rbacv1.RoleList{}, &rbacv1.RoleBindingList{}, &appsv1.StatefulSetList{},
		} {
			Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
			items, err := apimeta.ExtractList(list)
			Expect(err).NotTo(HaveOccurred())
			for _, item := range items {
				obj := item.(client.Object)
				for _, ref := range obj.GetOwnerReferences() {
					if ref.UID == oldUID {
						_ = k8sClient.Delete(ctx, obj)
					}
				}
			}
		}

		Expect(k8sClient.Create(ctx, newCluster(clusterName, group))).To(Succeed())
		recreated := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(clusterName), recreated)).To(Succeed())
		Eventually(func() bool {
			s := &corev1.Secret{}
			return k8sClient.Get(ctx, keyName, s) == nil && metav1.IsControlledBy(s, recreated) &&
				string(s.Data["sensitive-props-key"]) == originalKey
		}, timeout, interval).Should(BeTrue())
		Eventually(stsExists(clusterName+"-default"), timeout, interval).Should(BeTrue())
	})

	// L8: pruning a removed node group deletes only what the cluster controls, never a user object
	// that merely carries the same labels.
	It("L8: prune leaves an unowned object with the node group's labels alone", func() {
		clusterName = "l8"
		Expect(k8sClient.Create(ctx, newCluster(clusterName,
			nifiv1alpha1.NodeGroup{Name: "b", Replicas: 1, Storage: "1Gi"},
		))).To(Succeed())
		Eventually(stsExists(clusterName+"-b"), timeout, interval).Should(BeTrue())
		copied := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "users-copy-of-b", Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "nifi", "app.kubernetes.io/instance": clusterName,
				"app.kubernetes.io/managed-by": "nifi-operator", "nifi.ksolves.com/node-group": "b",
			},
		}}
		Expect(k8sClient.Create(ctx, copied)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, copied) })

		updateCluster(func(c *nifiv1alpha1.NiFiCluster) {
			c.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "a", Replicas: 1, Storage: "1Gi"}}
		})
		Eventually(stsExists(clusterName+"-b"), timeout, interval).Should(BeFalse())
		Consistently(func() error { return k8sClient.Get(ctx, key(copied.Name), &corev1.ConfigMap{}) }, time.Second, interval).Should(Succeed())
	})
})
