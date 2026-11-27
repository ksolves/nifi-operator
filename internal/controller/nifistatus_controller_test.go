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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// The real reconcile path for NiFi status. envtest runs no pods, so NiFi is never
// reachable here: the cluster must still reconcile fully and report ClusterHealthy=Unknown.
var _ = Describe("NiFi cluster status", func() {
	It("reports ClusterHealthy=Unknown/NotReady without a ready node, and still reconciles", func() {
		const name = "status-notready"
		c := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "a", Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			deleteLeftovers(name)
		})
		key := types.NamespacedName{Name: name, Namespace: "default"}
		Eventually(func(g Gomega) {
			latest := &nifiv1alpha1.NiFiCluster{}
			g.Expect(k8sClient.Get(ctx, key, latest)).To(Succeed())
			cond := apimeta.FindStatusCondition(latest.Status.Conditions, clusterHealthyConditionType)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			g.Expect(cond.Reason).To(Equal("NotReady"))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-a", Namespace: "default"}, &appsv1.StatefulSet{})).To(Succeed())
	})

	// Seen on OKD: a killed node rejoined under a new ID while NiFi still listed the old one at the
	// same address. Keyed by address, the CRD rejected every status write from then on.
	It("accepts two node entries at the same address", func() {
		const name = "status-dupaddr"
		c := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "a", Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			deleteLeftovers(name)
		})
		addr := name + "-a-0." + name + "-a-headless.default.svc.cluster.local"
		key := types.NamespacedName{Name: name, Namespace: "default"}
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key, latest); err != nil {
				return err
			}
			latest.Status.Nodes = []nifiv1alpha1.NiFiNodeStatus{
				{NodeID: "new", Address: addr, State: "CONNECTED"},
				{NodeID: "old", Address: addr, State: "DISCONNECTED"},
			}
			return k8sClient.Status().Update(ctx, latest)
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
	})
})
