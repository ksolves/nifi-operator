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

// a Secret named <cluster>-auth-credentials that the operator didn't create must be
// neither adopted, overwritten nor deleted, whichever auth mode the cluster uses.
var _ = Describe("Auth Secret ownership", func() {
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
		_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-auth-credentials", Namespace: namespace}})
	})

	DescribeTable("leaves a foreign Secret of the operator's name untouched",
		func(name string, auth *nifiv1alpha1.AuthenticationSpec) {
			clusterName = name
			foreign := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-auth-credentials", Namespace: namespace},
				StringData: map[string]string{"username": "someone-else", "password": "not-the-operators"},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			Expect(k8sClient.Create(ctx, &nifiv1alpha1.NiFiCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
				Spec: nifiv1alpha1.NiFiClusterSpec{
					Image:          nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
					NodeGroups:     []nifiv1alpha1.NodeGroup{{Name: "default", Replicas: 1, Storage: "1Gi"}},
					Authentication: auth,
				},
			})).To(Succeed())

			// The cluster has been reconciled at least once it has its finalizer.
			Eventually(func() []string {
				c := &nifiv1alpha1.NiFiCluster{}
				_ = k8sClient.Get(ctx, key(clusterName), c)
				return c.Finalizers
			}, timeout, interval).ShouldNot(BeEmpty())

			Consistently(func() string {
				s := &corev1.Secret{}
				if err := k8sClient.Get(ctx, key(foreign.Name), s); err != nil {
					return "gone: " + err.Error()
				}
				if len(s.OwnerReferences) > 0 {
					return "adopted"
				}
				return string(s.Data["username"]) + "/" + string(s.Data["password"])
			}, 2*time.Second, interval).Should(Equal("someone-else/not-the-operators"))

			// Literal and default mode refuse to proceed rather than building pods on someone
			// else's login; secretName mode doesn't use that Secret and carries on.
			if auth == nil || auth.SingleUser.SecretName == "" {
				Consistently(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, key(clusterName+"-default"), &appsv1.StatefulSet{}))
				}, time.Second, interval).Should(BeTrue())
			} else {
				Eventually(func() error {
					return k8sClient.Get(ctx, key(clusterName+"-default"), &appsv1.StatefulSet{})
				}, timeout, interval).Should(Succeed())
			}
		},
		Entry("literal username/password", "own-literal", &nifiv1alpha1.AuthenticationSpec{
			SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "operator-password"},
		}),
		Entry("secretName pointing elsewhere", "own-ref", &nifiv1alpha1.AuthenticationSpec{
			SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "my-login"},
		}),
		Entry("default generated login", "own-default", nil),
	)
})
