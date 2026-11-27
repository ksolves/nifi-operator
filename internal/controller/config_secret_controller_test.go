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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

var _ = Describe("ConfigOverrides Secrets", func() {
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

	It("keeps Secret-sourced overrides out of the ConfigMap and rolls the pods when the Secret rotates", func() {
		clusterName = "cfg-secret"
		userSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg-secret-props", Namespace: namespace},
			StringData: map[string]string{"nifi.ui.banner.text": "top-secret-banner"},
		}
		Expect(k8sClient.Create(ctx, userSecret)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, userSecret) })

		Expect(k8sClient.Create(ctx, &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{
					Name: "default", Replicas: 1, Storage: "1Gi",
					ConfigOverrides: &nifiv1alpha1.ConfigOverridesSpec{SecretName: userSecret.Name},
				}},
			},
		})).To(Succeed())

		configSecret := &corev1.Secret{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-default-config-secret"), configSecret) }, timeout, interval).Should(Succeed())
		Expect(string(configSecret.Data[nifiPropertiesOverridesKey])).To(Equal("nifi.ui.banner.text=top-secret-banner\n"))

		cm := &corev1.ConfigMap{}
		// Eventually, not a bare Expect: the ConfigMap is written in the same reconcile pass as
		// the config Secret above, right after it, but the two are independent informers with no
		// guaranteed delivery order - the Secret's own watch event reaching this cache first does
		// not mean the ConfigMap's has too.
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-default-config"), cm) }, timeout, interval).Should(Succeed())
		Expect(cm.Data[nifiPropertiesOverridesKey]).NotTo(ContainSubstring("top-secret-banner"))

		sts := &appsv1.StatefulSet{}
		configHashOf := func() string {
			if err := k8sClient.Get(ctx, key(clusterName+"-default"), sts); err != nil {
				return ""
			}
			return sts.Spec.Template.Annotations[configHashAnnotation]
		}
		Eventually(configHashOf, timeout, interval).ShouldNot(BeEmpty())
		firstHash := configHashOf()
		Expect(sts.Spec.Template.Spec.Volumes).To(ContainElement(HaveField("Name", sensitiveConfigVolumeName)))
		for _, v := range sts.Spec.Template.Annotations {
			Expect(v).NotTo(ContainSubstring("top-secret-banner"))
		}

		// M10: rotating the referenced Secret must reach the pods, through the Secret watch alone.
		Eventually(func() error {
			latest := &corev1.Secret{}
			if err := k8sClient.Get(ctx, key(userSecret.Name), latest); err != nil {
				return err
			}
			latest.StringData = map[string]string{"nifi.ui.banner.text": "rotated-banner"}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())
		Eventually(configHashOf, timeout, interval).ShouldNot(Equal(firstHash))
		Eventually(func() string {
			if err := k8sClient.Get(ctx, key(clusterName+"-default-config-secret"), configSecret); err != nil {
				return ""
			}
			return string(configSecret.Data[nifiPropertiesOverridesKey])
		}, timeout, interval).Should(Equal("nifi.ui.banner.text=rotated-banner\n"))

		// Dropping the reference removes the operator's config Secret and the volume.
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].ConfigOverrides = nil
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName+"-default-config-secret"), &corev1.Secret{}))
		}, timeout, interval).Should(BeTrue())
		Eventually(func() []corev1.Volume {
			_ = k8sClient.Get(ctx, key(clusterName+"-default"), sts)
			return sts.Spec.Template.Spec.Volumes
		}, timeout, interval).ShouldNot(ContainElement(HaveField("Name", sensitiveConfigVolumeName)))
	})
})
