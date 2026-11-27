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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// the PVC retention policy, cluster default plus per node group override.
var _ = Describe("PVC retention policy", func() {
	const (
		namespace = "default"
		timeout   = 10 * time.Second
		interval  = 100 * time.Millisecond
	)
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	newCluster := func(name string, policy *nifiv1alpha1.PVCRetentionPolicy, groups ...nifiv1alpha1.NodeGroup) *nifiv1alpha1.NiFiCluster {
		return &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:                                nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups:                           groups,
				PersistentVolumeClaimRetentionPolicy: policy,
			},
		}
	}
	stsPolicy := func(name string) func() appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
		return func() appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, key(name), sts); err != nil || sts.Spec.PersistentVolumeClaimRetentionPolicy == nil {
				return appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{}
			}
			return *sts.Spec.PersistentVolumeClaimRetentionPolicy
		}
	}
	policy := func(d, s appsv1.PersistentVolumeClaimRetentionPolicyType) appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
		return appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: d, WhenScaled: s}
	}
	retain, del := appsv1.RetainPersistentVolumeClaimRetentionPolicyType, appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	deleteCluster := func(name string) *nifiv1alpha1.NiFiCluster {
		c := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &nifiv1alpha1.NiFiCluster{})) },
			timeout, interval).Should(BeTrue())
		return c
	}
	// envtest runs no StatefulSet controller: claimFor creates the PVC a real one would have made
	// for a node group's StatefulSet, owned by it when its policy deletes the PVCs.
	claimFor := func(cluster, sts string, ownedBy bool) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-" + sts + "-0", Namespace: namespace, Labels: map[string]string{
				labelAppInstance: cluster, labelAppManagedBy: managedByValue,
			}},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		if ownedBy {
			owner := &appsv1.StatefulSet{}
			Eventually(func() error { return k8sClient.Get(ctx, key(sts), owner) }, timeout, interval).Should(Succeed())
			pvc.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts, UID: owner.UID}}
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pvc) })
		return pvc
	}

	It("resolves group over cluster over Retain, and applies a changed policy to the live StatefulSet", func() {
		const name = "ret-map"
		cluster := newCluster(name, &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCDelete, WhenScaled: nifiv1alpha1.PVCDelete},
			nifiv1alpha1.NodeGroup{Name: "a", Replicas: 1, Storage: "1Gi",
				PersistentVolumeClaimRetentionPolicy: &nifiv1alpha1.PVCRetentionPolicy{WhenScaled: nifiv1alpha1.PVCRetain}},
		)
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cluster)
			deleteLeftovers(name)
		})
		Eventually(stsPolicy(name+"-a"), timeout, interval).Should(Equal(policy(del, retain)))

		By("dropping the cluster default: back to Retain, in place")
		uid := func() types.UID {
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name+"-a"), sts)).To(Succeed())
			return sts.UID
		}
		before := uid()
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(name), latest); err != nil {
				return err
			}
			latest.Spec.PersistentVolumeClaimRetentionPolicy = nil
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())
		Eventually(stsPolicy(name+"-a"), timeout, interval).Should(Equal(policy(retain, retain)))
		Expect(uid()).To(Equal(before), "the StatefulSet must be updated, not recreated")
	})

	It("lets garbage collection take the key and deletes component state when every group deletes its PVCs", func() {
		const name = "ret-del"
		cluster := newCluster(name, &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCDelete},
			nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { deleteLeftovers(name) })
		secret := &corev1.Secret{}
		Eventually(func() error { return k8sClient.Get(ctx, key(name+"-sensitive-props"), secret) }, timeout, interval).Should(Succeed())
		state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-nifi-component-f5fc97b3-01a0-1000-ffff-ffff81295497", Namespace: namespace}}
		// Cluster "ret-del-nifi-component-x"'s state starts with this cluster's prefix too.
		other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: name + "-nifi-component-x-nifi-component-f5fc97b3-01a0-1000-ffff-ffff81295497", Namespace: namespace}}
		Expect(k8sClient.Create(ctx, state)).To(Succeed())
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		claimFor(name, name+"-default", true)
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, state)
			_ = k8sClient.Delete(ctx, other)
		})

		old := deleteCluster(name)

		Expect(k8sClient.Get(ctx, key(secret.Name), secret)).To(Succeed())
		Expect(metav1.IsControlledBy(secret, old)).To(BeTrue(), "the key must stay owned, so garbage collection deletes it")
		Expect(secret.Annotations).NotTo(HaveKey(retainedForAnnotation))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(state.Name), &corev1.ConfigMap{}))).To(BeTrue(), "component state left behind")
		Expect(k8sClient.Get(ctx, key(other.Name), &corev1.ConfigMap{})).To(Succeed(), "another cluster's component state was deleted")
		_ = k8sClient.Delete(ctx, secret)
	})

	It("keeps the key and component state when a NiFiNodeGroup keeps its PVCs, even if the inline groups delete theirs", func() {
		const name = "ret-mixed"
		cluster := newCluster(name, &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCDelete},
			nifiv1alpha1.NodeGroup{Name: "main", Replicas: 1, Storage: "1Gi"})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		group := &nifiv1alpha1.NiFiNodeGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: namespace},
			Spec: nifiv1alpha1.NiFiNodeGroupSpec{ClusterRef: name, NodeGroup: nifiv1alpha1.NodeGroup{
				Name: "keep", Replicas: 1, Storage: "1Gi",
				PersistentVolumeClaimRetentionPolicy: &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCRetain},
			}},
		}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, group)
			deleteLeftovers(name)
		})
		Eventually(stsPolicy(name+"-main"), timeout, interval).Should(Equal(policy(del, retain)))
		Eventually(stsPolicy(name+"-keep"), timeout, interval).Should(Equal(policy(retain, retain)))
		claimFor(name, name+"-main", true)
		claimFor(name, name+"-keep", false)
		state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-nifi-component-1", Namespace: namespace}}
		Expect(k8sClient.Create(ctx, state)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, state) })

		deleteCluster(name)

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name+"-sensitive-props"), secret)).To(Succeed())
		Expect(secret.OwnerReferences).To(BeEmpty())
		Expect(secret.Annotations).To(HaveKeyWithValue(retainedForAnnotation, name))
		Expect(k8sClient.Get(ctx, key(state.Name), &corev1.ConfigMap{})).To(Succeed(), "component state must be kept")
		_ = k8sClient.Delete(ctx, secret)
	})

	It("keeps the key when a Retain NiFiNodeGroup is already being deleted", func() {
		const name = "ret-nng-deleting"
		cluster := newCluster(name, &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCDelete},
			nifiv1alpha1.NodeGroup{Name: "main", Replicas: 1, Storage: "1Gi"})
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		group := &nifiv1alpha1.NiFiNodeGroup{
			ObjectMeta: metav1.ObjectMeta{Name: "hold", Namespace: namespace, Finalizers: []string{"test.nifi.ksolves.com/hold"}},
			Spec: nifiv1alpha1.NiFiNodeGroupSpec{ClusterRef: name, NodeGroup: nifiv1alpha1.NodeGroup{
				Name: "hold", Replicas: 1, Storage: "1Gi",
				PersistentVolumeClaimRetentionPolicy: &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCRetain},
			}},
		}
		Expect(k8sClient.Create(ctx, group)).To(Succeed())
		DeferCleanup(func() {
			Eventually(func() error {
				latest := &nifiv1alpha1.NiFiNodeGroup{}
				if err := k8sClient.Get(ctx, key(group.Name), latest); err != nil {
					if apierrors.IsNotFound(err) {
						return nil
					}
					return err
				}
				latest.Finalizers = nil
				return k8sClient.Update(ctx, latest)
			}, timeout, interval).Should(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key(group.Name), &nifiv1alpha1.NiFiNodeGroup{}))
			},
				timeout, interval).Should(BeTrue())
			deleteLeftovers(name)
		})
		Eventually(stsPolicy(name+"-main"), timeout, interval).Should(Equal(policy(del, retain)))
		Eventually(stsPolicy(name+"-hold"), timeout, interval).Should(Equal(policy(retain, retain)))
		claimFor(name, name+"-main", true)
		claimFor(name, name+"-hold", false)

		// The test finalizer holds the node group in deletion while the cluster goes.
		Expect(k8sClient.Delete(ctx, group)).To(Succeed())
		Eventually(func(g Gomega) {
			latest := &nifiv1alpha1.NiFiNodeGroup{}
			g.Expect(k8sClient.Get(ctx, key(group.Name), latest)).To(Succeed())
			g.Expect(latest.DeletionTimestamp).NotTo(BeNil())
		}, timeout, interval).Should(Succeed())

		deleteCluster(name)

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name+"-sensitive-props"), secret)).To(Succeed())
		Expect(secret.OwnerReferences).To(BeEmpty(), "the key must be released, or garbage collection deletes it")
		Expect(secret.Annotations).To(HaveKeyWithValue(retainedForAnnotation, name))
		_ = k8sClient.Delete(ctx, secret)
	})

	// what survives is decided by the PVCs themselves: one the StatefulSet controller has not
	// marked for deletion (no owner yet; envtest runs no StatefulSet controller) or one a removed
	// node group left behind stays, so switching the spec to Delete and then deleting the cluster
	// must not drop their key.
	It("keeps the key and component state for PVCs that stay, even when the spec says Delete", func() {
		const name = "ret-paused"
		Expect(k8sClient.Create(ctx, newCluster(name, nil, nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}))).To(Succeed())
		DeferCleanup(func() { deleteLeftovers(name) })
		Eventually(stsPolicy(name+"-default"), timeout, interval).Should(Equal(policy(retain, retain)))
		claimFor(name, name+"-default", false)
		// A node group removed earlier left its retained PVC behind, with no StatefulSet at all.
		claimFor(name, name+"-gone", false)
		state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-nifi-component-f5fc97b3-01a0-1000-ffff-ffff81295497", Namespace: namespace}}
		Expect(k8sClient.Create(ctx, state)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, state) })

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(name), latest); err != nil {
				return err
			}
			latest.Spec.PersistentVolumeClaimRetentionPolicy = &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: nifiv1alpha1.PVCDelete}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())
		Eventually(stsPolicy(name+"-default"), timeout, interval).Should(Equal(policy(del, retain)))

		deleteCluster(name)

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name+"-sensitive-props"), secret)).To(Succeed())
		Expect(secret.OwnerReferences).To(BeEmpty(), "the key must be released, or garbage collection deletes it")
		Expect(secret.Annotations).To(HaveKeyWithValue(retainedForAnnotation, name))
		Expect(k8sClient.Get(ctx, key(state.Name), &corev1.ConfigMap{})).To(Succeed(), "component state must be kept")
		_ = k8sClient.Delete(ctx, secret)
	})

	// the cache holds a Secret without the managed-by label without its data; adopting
	// a retained key from there would write it back empty.
	It("adopts a retained key without the managed-by label with its data intact", func() {
		const name = "ret-adopt"
		retained := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sensitive-props", Namespace: namespace,
				Annotations: map[string]string{retainedForAnnotation: name}},
			StringData: map[string]string{sensitivePropsKeyDataKey: "the-old-key-0123456789"},
		}
		Expect(k8sClient.Create(ctx, retained)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, retained) })
		claimFor(name, name+"-default", false)

		Expect(k8sClient.Create(ctx, newCluster(name, nil, nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}))).To(Succeed())
		DeferCleanup(func() { deleteLeftovers(name) })
		cluster := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
		Eventually(func(g Gomega) {
			s := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx, key(retained.Name), s)).To(Succeed())
			g.Expect(metav1.IsControlledBy(s, cluster)).To(BeTrue())
			g.Expect(string(s.Data[sensitivePropsKeyDataKey])).To(Equal("the-old-key-0123456789"))
			g.Expect(s.Labels).To(HaveKeyWithValue(labelAppManagedBy, managedByValue))
		}, timeout, interval).Should(Succeed())
	})

	// the key is kept for the PVCs of a node group removed before the delete, so a
	// recreate without that group must still adopt it, or it could never start.
	It("adopts a retained key whose only PVCs belong to a node group the recreated cluster lacks", func() {
		const name = "ret-gone"
		retained := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sensitive-props", Namespace: namespace,
				Labels:      map[string]string{labelAppInstance: name, labelAppManagedBy: managedByValue},
				Annotations: map[string]string{retainedForAnnotation: name}},
			StringData: map[string]string{sensitivePropsKeyDataKey: "the-gone-key-0123456789"},
		}
		Expect(k8sClient.Create(ctx, retained)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, retained) })
		claimFor(name, name+"-removed", false)

		Expect(k8sClient.Create(ctx, newCluster(name, nil, nifiv1alpha1.NodeGroup{Name: "default", Replicas: 1, Storage: "1Gi"}))).To(Succeed())
		DeferCleanup(func() { deleteLeftovers(name) })
		cluster := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
		Eventually(func(g Gomega) {
			s := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx, key(retained.Name), s)).To(Succeed())
			g.Expect(metav1.IsControlledBy(s, cluster)).To(BeTrue())
			g.Expect(string(s.Data[sensitivePropsKeyDataKey])).To(Equal("the-gone-key-0123456789"))
		}, timeout, interval).Should(Succeed())
		Eventually(stsPolicy(name+"-default"), timeout, interval).Should(Equal(policy(retain, retain)))
	})
})
