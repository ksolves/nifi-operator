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
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// csiDriverTestServiceAccount is the CSI driver ServiceAccount the envtest reconciler is given.
var csiDriverTestServiceAccount = types.NamespacedName{Namespace: "nifi-operator-system", Name: "nifi-operator-csi-driver"}

var _ = Describe("CSI driver CA access", func() {
	const (
		namespace = "default"
		timeout   = 10 * time.Second
		interval  = 100 * time.Millisecond
	)

	var clusterName string
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

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

	It("grants the CSI driver get on this cluster's CA Secret only, and revokes it when TLS is turned off", func() {
		clusterName = "csi-ca"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		readerKey := key(clusterName + "-csi-ca-reader")
		role := &rbacv1.Role{}
		Eventually(func() error { return k8sClient.Get(ctx, readerKey, role) }, timeout, interval).Should(Succeed())
		binding := &rbacv1.RoleBinding{}
		Eventually(func() error { return k8sClient.Get(ctx, readerKey, binding) }, timeout, interval).Should(Succeed())

		Expect(role.Rules).To(Equal([]rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: []string{clusterName + "-ca-certificate"},
			Verbs:         []string{"get"},
		}}))
		Expect(binding.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: readerKey.Name}))
		Expect(binding.Subjects).To(ConsistOf(rbacv1.Subject{
			Kind: rbacv1.ServiceAccountKind, Name: csiDriverTestServiceAccount.Name, Namespace: csiDriverTestServiceAccount.Namespace,
		}))
		latest := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, key(clusterName), latest)).To(Succeed())
		Expect(metav1.IsControlledBy(role, latest)).To(BeTrue())
		Expect(metav1.IsControlledBy(binding, latest)).To(BeTrue())

		Eventually(func() error {
			if err := k8sClient.Get(ctx, key(clusterName), latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].TLS = nil
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, readerKey, &rbacv1.RoleBinding{})) &&
				apierrors.IsNotFound(k8sClient.Get(ctx, readerKey, &rbacv1.Role{}))
		}, timeout, interval).Should(BeTrue())
	})

	It("creates no CSI CA grant for a cluster without TLS", func() {
		clusterName = "csi-no-tls"
		Expect(k8sClient.Create(ctx, &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
			},
		})).To(Succeed())

		// The coordination Role appearing means the reconcile reached the RBAC steps.
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-nifi"), &rbacv1.Role{}) }, timeout, interval).Should(Succeed())
		Consistently(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName+"-csi-ca-reader"), &rbacv1.Role{}))
		}, time.Second, interval).Should(BeTrue())
	})
})
