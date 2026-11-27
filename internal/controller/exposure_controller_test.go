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

var _ = Describe("NiFiCluster exposure", func() {
	const (
		namespace = "default"
		timeout   = 10 * time.Second
		interval  = 100 * time.Millisecond
	)
	var clusterName string

	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	tlsCluster := func(name string) *nifiv1alpha1.NiFiCluster {
		tls := &nifiv1alpha1.TLSSpec{Enabled: true}
		return &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: "a", Replicas: 1, Storage: "1Gi", TLS: tls},
				},
			},
		}
	}

	AfterEach(func() {
		c := &nifiv1alpha1.NiFiCluster{}
		if err := k8sClient.Get(ctx, key(clusterName), c); err == nil {
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName), c)) }, timeout, interval).Should(BeTrue())
		}
		deleteLeftovers(clusterName)
	})

	It("creates one Service across all node groups", func() {
		clusterName = "expo-svc"
		Expect(k8sClient.Create(ctx, tlsCluster(clusterName))).To(Succeed())

		svc := &corev1.Service{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName), svc) }, timeout, interval).Should(Succeed())
		Expect(svc.Spec.Selector).To(Equal(map[string]string{labelAppName: appNameValue, labelAppInstance: clusterName}))
		Expect(svc.Spec.SessionAffinity).To(Equal(corev1.ServiceAffinityClientIP))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(nifiHTTPSPort)))
	})

	It("rolls TLS pods when a new proxy host must go on their certificates", func() {
		clusterName = "expo-sans"
		Expect(k8sClient.Create(ctx, tlsCluster(clusterName))).To(Succeed())

		sts := &appsv1.StatefulSet{}
		Eventually(func() error { return k8sClient.Get(ctx, key(clusterName+"-a"), sts) }, timeout, interval).Should(Succeed())
		before := sts.Spec.Template.Annotations[certSANsHashAnnotation]
		Expect(before).NotTo(BeEmpty())

		Eventually(func() error {
			c := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key(clusterName), c); err != nil {
				return err
			}
			c.Spec.ProxyHosts = []string{"new.apps.example.com"}
			return k8sClient.Update(ctx, c)
		}, timeout, interval).Should(Succeed())
		Eventually(func() string {
			if err := k8sClient.Get(ctx, key(clusterName+"-a"), sts); err != nil {
				return before
			}
			return sts.Spec.Template.Annotations[certSANsHashAnnotation]
		}, timeout, interval).ShouldNot(Equal(before))
	})
})
