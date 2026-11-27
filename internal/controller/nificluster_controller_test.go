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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/bcrypt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/certmetrics"
)

var _ = Describe("NiFiCluster controller", func() {
	const (
		clusterName = "test-cluster"
		namespace   = "default"
		nodeGroup   = "default"
		timeout     = 10 * time.Second
		interval    = 100 * time.Millisecond
	)

	AfterEach(func() {
		cluster := &nifiv1alpha1.NiFiCluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, cluster); err == nil {
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, cluster)
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
		}
		// envtest runs no garbage collector, and every spec here reuses clusterName. Stand in for
		// it on the RBAC objects, which the operator refuses to take over from a previous
		// NiFiCluster (see getOwned).
		deleteLeftovers(clusterName)
	})

	It("creates a ConfigMap, Services and a StatefulSet for each node group", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 2, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		sts := &appsv1.StatefulSet{}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, sts)
		}, timeout, interval).Should(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(2)))
		Expect(sts.Spec.Template.Annotations).To(HaveKey(configHashAnnotation))
		Expect(sts.Spec.Template.Annotations[configHashAnnotation]).NotTo(BeEmpty())

		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-" + nodeGroup + "-headless", Namespace: namespace}, &corev1.Service{})
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}, &corev1.Service{})
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-" + nodeGroup + "-config", Namespace: namespace}, &corev1.ConfigMap{})
		}, timeout, interval).Should(Succeed())
	})

	It("scales the StatefulSet when Replicas changes", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].Replicas = 3
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() int32 {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return -1
			}
			return *sts.Spec.Replicas
		}, timeout, interval).Should(Equal(int32(3)))
	})

	It("removes the finalizer and allows deletion", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		key := types.NamespacedName{Name: clusterName, Namespace: namespace}
		Eventually(func() []string {
			latest := &nifiv1alpha1.NiFiCluster{}
			_ = k8sClient.Get(ctx, key, latest)
			return latest.Finalizers
		}, timeout, interval).Should(ContainElement(nifiClusterFinalizer))

		Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, key, &nifiv1alpha1.NiFiCluster{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())
	})

	It("creates a PodDisruptionBudget by default and removes it when disabled", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 3, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		pdbKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() int {
			pdb := &policyv1.PodDisruptionBudget{}
			if err := k8sClient.Get(ctx, pdbKey, pdb); err != nil || pdb.Spec.MinAvailable == nil {
				return -1
			}
			return pdb.Spec.MinAvailable.IntValue()
		}, timeout, interval).Should(Equal(2))

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			disabled := false
			latest.Spec.NodeGroups[0].PodDisruptionBudget = &nifiv1alpha1.PodDisruptionBudgetSpec{Enabled: &disabled}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, pdbKey, &policyv1.PodDisruptionBudget{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())
	})

	It("does not rotate the sensitive-props Secret on a later reconcile", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		secretKey := types.NamespacedName{Name: clusterName + "-sensitive-props", Namespace: namespace}
		secret := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, secretKey, secret)
		}, timeout, interval).Should(Succeed())

		originalData := map[string][]byte{}
		for k, v := range secret.Data {
			originalData[k] = append([]byte(nil), v...)
		}

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].Replicas = 2
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() int32 {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return -1
			}
			return *sts.Spec.Replicas
		}, timeout, interval).Should(Equal(int32(2)))

		reconciled := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, reconciled)).To(Succeed())
		Expect(reconciled.Data).To(Equal(originalData))
	})

	It("creates the TLS CA Secret once and never rewrites it on later reconciles", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		caKey := types.NamespacedName{Name: clusterName + "-ca-certificate", Namespace: namespace}
		ca := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, caKey, ca)
		}, timeout, interval).Should(Succeed())
		Expect(ca.Type).To(Equal(corev1.SecretTypeTLS))
		Expect(ca.Data).To(HaveKey(corev1.TLSCertKey))
		Expect(ca.Data).To(HaveKey(corev1.TLSPrivateKeyKey))

		originalResourceVersion := ca.ResourceVersion

		// Trigger several more reconciles.
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].Replicas = 2
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() int32 {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return -1
			}
			return *sts.Spec.Replicas
		}, timeout, interval).Should(Equal(int32(2)))

		reconciled := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, caKey, reconciled)).To(Succeed())
		Expect(reconciled.ResourceVersion).To(Equal(originalResourceVersion))
	})

	It("reports CAExpiringSoon and the CA expiry metric, once the CA is close to expiring (NIFI-248 B3)", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		caKey := types.NamespacedName{Name: tlsCASecretName(cluster), Namespace: namespace}
		ca := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, caKey, ca)
		}, timeout, interval).Should(Succeed())

		// A brand-new 5-year CA is nowhere near tlsCAWarningFractionThreshold yet.
		Eventually(func() *metav1.Condition {
			latest := &nifiv1alpha1.NiFiCluster{}
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest)
			return apimeta.FindStatusCondition(latest.Status.Conditions, caExpiringSoonConditionType)
		}, timeout, interval).Should(And(Not(BeNil()), HaveField("Status", metav1.ConditionFalse)))
		Eventually(func() float64 {
			return testutil.ToFloat64(certmetrics.CAExpiryGauge.WithLabelValues(namespace, clusterName))
		}, timeout, interval).Should(BeNumerically("~", time.Now().AddDate(5, 0, 0).Unix(), 60))

		// Swap in a CA 50 days into a 60-day lifetime (83% elapsed, past
		// tlsCAWarningFractionThreshold's 80%) and 10 days from expiry: Owns(&corev1.Secret{})
		// means patching it re-triggers reconciliation.
		key, err := rsa.GenerateKey(rand.Reader, tlsRSAKeyBits)
		Expect(err).NotTo(HaveOccurred())
		soonExpiry := time.Now().Add(10 * 24 * time.Hour)
		caTemplate := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
			NotBefore: time.Now().Add(-50 * 24 * time.Hour), NotAfter: soonExpiry,
			IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
		Expect(err).NotTo(HaveOccurred())
		parsed, err := x509.ParseCertificate(der)
		Expect(err).NotTo(HaveOccurred())

		patch := client.MergeFrom(ca.DeepCopy())
		ca.Data[corev1.TLSCertKey] = encodeCertPEM(der)
		ca.Data[corev1.TLSPrivateKeyKey] = encodeKeyPEM(key)
		Expect(k8sClient.Patch(ctx, ca, patch)).To(Succeed())

		Eventually(func() *metav1.Condition {
			latest := &nifiv1alpha1.NiFiCluster{}
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest)
			return apimeta.FindStatusCondition(latest.Status.Conditions, caExpiringSoonConditionType)
		}, timeout, interval).Should(And(Not(BeNil()), HaveField("Status", metav1.ConditionTrue)))
		Eventually(func() float64 {
			return testutil.ToFloat64(certmetrics.CAExpiryGauge.WithLabelValues(namespace, clusterName))
		}, timeout, interval).Should(BeNumerically("~", float64(parsed.NotAfter.Unix()), 2))
	})

	It("relabels its create-once Secrets without touching their data", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		secretKeys := []types.NamespacedName{
			{Name: sensitivePropsSecretName(cluster), Namespace: namespace},
			{Name: tlsCASecretName(cluster), Namespace: namespace},
			{Name: authSecretName(cluster), Namespace: namespace},
		}
		originalData := map[string]map[string][]byte{}
		for _, key := range secretKeys {
			secret := &corev1.Secret{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, secret)
			}, timeout, interval).Should(Succeed())
			data := map[string][]byte{}
			for k, v := range secret.Data {
				data[k] = append([]byte(nil), v...)
			}
			originalData[key.Name] = data
		}

		Eventually(func() error {
			ca := &corev1.Secret{}
			if err := k8sClient.Get(ctx, secretKeys[1], ca); err != nil {
				return err
			}
			delete(ca.Labels, labelAppName)
			return k8sClient.Update(ctx, ca)
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.Labels = map[string]string{"team": "data"}
			latest.Spec.Annotations = map[string]string{"example.com/owner": "nifi-team"}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		for _, key := range secretKeys {
			Eventually(func(g Gomega) {
				secret := &corev1.Secret{}
				g.Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
				g.Expect(secret.Labels).To(HaveKeyWithValue("team", "data"))
				g.Expect(secret.Labels).To(HaveKeyWithValue(labelAppName, appNameValue))
				g.Expect(secret.Annotations).To(HaveKeyWithValue("example.com/owner", "nifi-team"))
				g.Expect(secret.Data).To(Equal(originalData[key.Name]))
			}, timeout, interval).Should(Succeed())
		}
	})

	It("propagates cluster and node-group labels/annotations to the generated objects", func() {
		const name = "labels-cluster"
		key := types.NamespacedName{Name: name, Namespace: namespace}
		pdbEnabled := true
		minAvailable := int32(1)
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:       nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				Labels:      map[string]string{"team": "data", "tier": "cluster", labelAppName: "hacked", labelNodeGroup: "bogus"},
				Annotations: map[string]string{"example.com/owner": "nifi-team"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{
						Name:                nodeGroup,
						Replicas:            2,
						Storage:             "1Gi",
						Labels:              map[string]string{"tier": "ng"},
						Annotations:         map[string]string{"example.com/owner": "ng-team"},
						PodDisruptionBudget: &nifiv1alpha1.PodDisruptionBudgetSpec{Enabled: &pdbEnabled, MinAvailable: &minAvailable},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key, latest); err == nil {
				Expect(k8sClient.Delete(ctx, latest)).To(Succeed())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, key, &nifiv1alpha1.NiFiCluster{}))
				}, timeout, interval).Should(BeTrue())
			}
			deleteLeftovers(name)
		})

		ng := &cluster.Spec.NodeGroups[0]
		expectMeta := func(obj client.Object, objName string, wantLabels, wantAnn map[string]string) {
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: objName, Namespace: namespace}, obj)).To(Succeed())
				for k, v := range wantLabels {
					g.Expect(obj.GetLabels()).To(HaveKeyWithValue(k, v), "label %s on %T %s", k, obj, objName)
				}
				for k, v := range wantAnn {
					g.Expect(obj.GetAnnotations()).To(HaveKeyWithValue(k, v), "annotation %s on %T %s", k, obj, objName)
				}
			}, timeout, interval).Should(Succeed())
		}
		assertAll := func(team string, extraAnn map[string]string) {
			ngLabels := map[string]string{"team": team, "tier": "ng", labelAppName: appNameValue, labelNodeGroup: ng.Name}
			ngAnn := mergeStringMaps(map[string]string{"example.com/owner": "ng-team"}, extraAnn)
			expectMeta(&corev1.ConfigMap{}, configMapName(cluster, ng), ngLabels, ngAnn)
			expectMeta(&corev1.Service{}, headlessServiceName(cluster, ng), ngLabels, ngAnn)
			expectMeta(&corev1.Service{}, clientServiceName(cluster, ng), ngLabels, ngAnn)
			expectMeta(&appsv1.StatefulSet{}, statefulSetName(cluster, ng), ngLabels, ngAnn)
			expectMeta(&policyv1.PodDisruptionBudget{}, statefulSetName(cluster, ng), ngLabels, ngAnn)

			clusterLbls := map[string]string{"team": team, "tier": "cluster", labelAppName: appNameValue}
			clusterAnn := mergeStringMaps(map[string]string{"example.com/owner": "nifi-team"}, extraAnn)
			clusterSvc := &corev1.Service{}
			expectMeta(clusterSvc, clusterServiceName(cluster), clusterLbls, clusterAnn)
			Expect(clusterSvc.GetLabels()).NotTo(HaveKey(labelNodeGroup))
			sensitiveSecret := &corev1.Secret{}
			expectMeta(sensitiveSecret, sensitivePropsSecretName(cluster), clusterLbls, clusterAnn)
			Expect(sensitiveSecret.GetLabels()).NotTo(HaveKey(labelNodeGroup))
			authSecret := &corev1.Secret{}
			expectMeta(authSecret, authSecretName(cluster), clusterLbls, clusterAnn)
			Expect(authSecret.GetLabels()).NotTo(HaveKey(labelNodeGroup))

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: statefulSetName(cluster, ng), Namespace: namespace}, sts)).To(Succeed())
			Expect(sts.Spec.Template.Labels).NotTo(HaveKey("team"))
			Expect(sts.Spec.Template.Labels).To(HaveKeyWithValue(labelAppName, appNameValue))
		}

		sensitiveKey := types.NamespacedName{Name: sensitivePropsSecretName(cluster), Namespace: namespace}
		sensitive := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, sensitiveKey, sensitive)
		}, timeout, interval).Should(Succeed())
		sensitiveUID := sensitive.UID

		assertAll("data", nil)

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, key, latest); err != nil {
				return err
			}
			latest.Spec.Labels["team"] = "data2"
			latest.Spec.Annotations["example.com/extra"] = "x"
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		assertAll("data2", map[string]string{"example.com/extra": "x"})

		after := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, sensitiveKey, after)).To(Succeed())
		Expect(after.UID).To(Equal(sensitiveUID))
	})

	It("creates and keeps an auth Secret in sync with literal username/password, then keeps it (unrotated) as the default login when unset", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
				Authentication: &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "first-password"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		authKey := types.NamespacedName{Name: clusterName + "-auth-credentials", Namespace: namespace}
		authSecret := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, authKey, authSecret)
		}, timeout, interval).Should(Succeed())
		Expect(string(authSecret.Data["username"])).To(Equal("admin"))
		Expect(string(authSecret.Data["password"])).To(Equal("first-password"))

		hashKey := types.NamespacedName{Name: clusterName + "-single-user-hash", Namespace: namespace}
		hashSecret := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, hashKey, hashSecret)
		}, timeout, interval).Should(Succeed())
		Expect(string(hashSecret.Data["username"])).To(Equal("admin"))
		Expect(bcrypt.CompareHashAndPassword(hashSecret.Data["password-bcrypt"], []byte("first-password"))).To(Succeed())
		Expect(metav1.IsControlledBy(hashSecret, cluster)).To(BeTrue())

		firstHash := hashSecret.Data["password-bcrypt"]
		Consistently(func() []byte {
			current := &corev1.Secret{}
			if err := k8sClient.Get(ctx, hashKey, current); err != nil {
				return nil
			}
			return current.Data["password-bcrypt"]
		}, 2*time.Second, 250*time.Millisecond).Should(Equal(firstHash))

		// Unlike the TLS CA/sensitive-props Secrets, this one must track the CR: editing the
		// literal password should actually change the Secret, not be silently ignored.
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.Authentication.SingleUser.Password = "second-password"
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() string {
			updated := &corev1.Secret{}
			if err := k8sClient.Get(ctx, authKey, updated); err != nil {
				return ""
			}
			return string(updated.Data["password"])
		}, timeout, interval).Should(Equal("second-password"))

		Eventually(func() error {
			updated := &corev1.Secret{}
			if err := k8sClient.Get(ctx, hashKey, updated); err != nil {
				return err
			}
			return bcrypt.CompareHashAndPassword(updated.Data["password-bcrypt"], []byte("second-password"))
		}, timeout, interval).Should(Succeed())

		// Removing Authentication entirely falls back to the operator's default-login behavior,
		// which is create-once: since the Secret already exists (from the literal-credentials
		// path above), it must be left untouched rather than deleted or rotated - there's no CR
		// field tracking this password, so silently changing it would just log out whoever is
		// already using it.
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.Authentication = nil
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Consistently(func() string {
			updated := &corev1.Secret{}
			if err := k8sClient.Get(ctx, authKey, updated); err != nil {
				return ""
			}
			return string(updated.Data["password"])
		}, timeout, interval).Should(Equal("second-password"))
	})

	It("rolls the StatefulSet on a literal password change via the auth Secret's version, never a password hash", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
				Authentication: &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "first-password"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		authKey := types.NamespacedName{Name: clusterName + "-auth-credentials", Namespace: namespace}
		authHashOf := func() string {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return ""
			}
			return sts.Spec.Template.Annotations[authHashAnnotation]
		}
		secretVersionHash := func() string {
			secret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, authKey, secret); err != nil {
				return "<no secret>"
			}
			return referencedAuthSecretHash(secret)
		}

		Eventually(func() bool {
			hash := authHashOf()
			return hash != "" && hash == secretVersionHash()
		}, timeout, interval).Should(BeTrue())
		firstHash := authHashOf()

		// Re-reconciling an unchanged CR must not rewrite the Secret: a new resourceVersion on
		// every pass would roll the pods forever.
		Consistently(authHashOf, "3s", interval).Should(Equal(firstHash))

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.Authentication.SingleUser.Password = "second-password"
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(authHashOf, timeout, interval).ShouldNot(Equal(firstHash))
		Eventually(func() bool { return authHashOf() == secretVersionHash() }, timeout, interval).Should(BeTrue())
	})

	It("generates a default Secret-backed login when Authentication is left unset entirely", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		authKey := types.NamespacedName{Name: clusterName + "-auth-credentials", Namespace: namespace}
		authSecret := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, authKey, authSecret)
		}, timeout, interval).Should(Succeed())
		Expect(string(authSecret.Data["username"])).To(Equal("admin"))
		Expect(len(authSecret.Data["password"])).To(BeNumerically(">=", 12))

		originalPassword := string(authSecret.Data["password"])

		// Create-once: a later reconcile must not rotate the generated password.
		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].Replicas = 2
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Consistently(func() string {
			updated := &corev1.Secret{}
			if err := k8sClient.Get(ctx, authKey, updated); err != nil {
				return ""
			}
			return string(updated.Data["password"])
		}, timeout, interval).Should(Equal(originalPassword))
	})

	It("does not create an auth Secret when a user-supplied secretName is used instead", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
				Authentication: &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "user-owned-login"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())

		authKey := types.NamespacedName{Name: clusterName + "-auth-credentials", Namespace: namespace}
		Consistently(func() bool {
			err := k8sClient.Get(ctx, authKey, &corev1.Secret{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())
	})

	It("deletes the synthesized auth Secret when switching from literal credentials to secretName", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
				Authentication: &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "first-password"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		authKey := types.NamespacedName{Name: clusterName + "-auth-credentials", Namespace: namespace}
		Eventually(func() error {
			return k8sClient.Get(ctx, authKey, &corev1.Secret{})
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.Authentication.SingleUser = &nifiv1alpha1.SingleUserAuthSpec{SecretName: "user-owned-login"}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, authKey, &corev1.Secret{}))
		}, timeout, interval).Should(BeTrue())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() string {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return ""
			}
			for _, env := range sts.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "NIFI_AUTH_PASSWORD" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
					return env.ValueFrom.SecretKeyRef.Name
				}
			}
			return ""
		}, timeout, interval).Should(Equal("user-owned-login"))
	})

	It("rolls the StatefulSet when a user-supplied auth Secret is created or rotated", func() {
		const userSecretName = "rotating-login"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
				Authentication: &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: userSecretName},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		authHashOf := func() string {
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, sts); err != nil {
				return "<no sts>"
			}
			return sts.Spec.Template.Annotations[authHashAnnotation]
		}

		// Secret doesn't exist yet: StatefulSet is created, but without an auth hash.
		Eventually(authHashOf, timeout, interval).Should(Equal(""))

		// Creating the Secret later must be picked up through the Secret watch alone.
		userSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: userSecretName, Namespace: namespace},
			StringData: map[string]string{"username": "admin", "password": "first-password"},
		}
		Expect(k8sClient.Create(ctx, userSecret)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, userSecret) })

		var firstHash string
		Eventually(func() string {
			firstHash = authHashOf()
			return firstHash
		}, timeout, interval).ShouldNot(Or(Equal(""), Equal("<no sts>")))

		// The user Secret carries no managed-by label, so the manager cache strips its Data: the
		// hash Secret must still be built from the real credentials (uncached read).
		hashKey := types.NamespacedName{Name: clusterName + "-single-user-hash", Namespace: namespace}
		hashVerifies := func(password string) func() error {
			return func() error {
				hashSecret := &corev1.Secret{}
				if err := k8sClient.Get(ctx, hashKey, hashSecret); err != nil {
					return err
				}
				if string(hashSecret.Data["username"]) != "admin" {
					return fmt.Errorf("hash Secret username %q, want %q", hashSecret.Data["username"], "admin")
				}
				return bcrypt.CompareHashAndPassword(hashSecret.Data["password-bcrypt"], []byte(password))
			}
		}
		Eventually(hashVerifies("first-password"), timeout, interval).Should(Succeed())

		// Rotating the password must change the pod template, which is what rolls the pods.
		Eventually(func() error {
			latest := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: userSecretName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.StringData = map[string]string{"password": "second-password"}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(authHashOf, timeout, interval).ShouldNot(Equal(firstHash))
		Eventually(hashVerifies("second-password"), timeout, interval).Should(Succeed())
	})

	It("keeps reconciling and reporting status for healthy node groups after another one fails", func() {
		const badNodeGroup = "broken"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: nodeGroup, Replicas: 1, Storage: "1Gi"}},
			},
		}
		// A ConfigMap of the bad group's name, controlled by something else: the operator refuses
		// to take it over, so that node group fails at reconcile time - the trigger
		// for the fail-safe behavior under test. (An unparsable storage size used to do this; the
		// CRD's CEL rules now reject it at the API.)
		foreign := foreignControlledConfigMap(clusterName+"-"+badNodeGroup+"-config", namespace)
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		createNodeGroupObject(namespace, clusterName, nifiv1alpha1.NodeGroup{Name: badNodeGroup, Replicas: 1, Storage: "1Gi"})

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())

		badStsKey := types.NamespacedName{Name: clusterName + "-" + badNodeGroup, Namespace: namespace}
		Consistently(func() bool {
			err := k8sClient.Get(ctx, badStsKey, &appsv1.StatefulSet{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())

		// The bad node group never blocks the healthy one's status from being reported - that's
		// the whole point of fail-safe (not fail-fast) reconciliation. It still counts, as 0
		// ready, and makes the cluster Degraded and not Available.
		latest := &nifiv1alpha1.NiFiCluster{}
		Eventually(func() []nifiv1alpha1.NodeGroupStatus {
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest)
			return latest.Status.NodeGroupStatuses
		}, timeout, interval).Should(ConsistOf(
			HaveField("Name", nodeGroup),
			And(HaveField("Name", badNodeGroup), HaveField("Replicas", int32(1)), HaveField("ReadyReplicas", int32(0))),
		))
		Expect(latest.Status.Replicas).To(Equal(int32(2)))
		degraded := apimeta.FindStatusCondition(latest.Status.Conditions, degradedConditionType)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Message).To(ContainSubstring(badNodeGroup))
		Expect(apimeta.IsStatusConditionTrue(latest.Status.Conditions, "Available")).To(BeFalse())
	})

	// This only proves the pod template annotation value changes, not that a pod actually restarts:
	// envtest here runs kube-apiserver+etcd alone, with no kubelet or StatefulSet controller to act
	// on the template change.
	It("recomputes the StatefulSet's config-hash annotation when node group configuration changes", func() {
		// A real configuration change (a ConfigOverrides property) must roll the pods; see the
		// next spec for why a replica change must not.
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		sts := &appsv1.StatefulSet{}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, sts)
		}, timeout, interval).Should(Succeed())
		originalHash := sts.Spec.Template.Annotations[configHashAnnotation]
		Expect(originalHash).NotTo(BeEmpty())

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups[0].ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
				NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "45 sec"},
			}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		Eventually(func() string {
			updated := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, updated); err != nil {
				return originalHash
			}
			return updated.Spec.Template.Annotations[configHashAnnotation]
		}, timeout, interval).ShouldNot(Equal(originalHash))
	})

	It("sets a retrying preStop decommission command on the NiFi container", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		sts := &appsv1.StatefulSet{}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, sts)
		}, timeout, interval).Should(Succeed())

		Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(sts.Spec.Template.Spec.Containers[0].Lifecycle).NotTo(BeNil())
		Expect(sts.Spec.Template.Spec.Containers[0].Lifecycle.PreStop).NotTo(BeNil())
		Expect(sts.Spec.Template.Spec.Containers[0].Lifecycle.PreStop.Exec).NotTo(BeNil())
		// Retries nifi.sh decommission (it can intermittently fail with
		// IllegalNodeDisconnectionException during close-together scale-downs) rather than
		// running it once - see preStopHandler's doc for why.
		cmd := strings.Join(sts.Spec.Template.Spec.Containers[0].Lifecycle.PreStop.Exec.Command, " ")
		Expect(cmd).To(ContainSubstring("/opt/nifi/nifi-current/bin/nifi.sh decommission"))
		Expect(cmd).To(ContainSubstring("max_attempts=5"))
	})

	It("runs the default node image when spec.image is unset", func() {
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		sts := &appsv1.StatefulSet{}
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, sts)
		}, timeout, interval).Should(Succeed())

		var image string
		for _, c := range sts.Spec.Template.Spec.Containers {
			if c.Name == NifiContainerName {
				image = c.Image
			}
		}
		Expect(image).To(Equal(BuiltinNiFiImage))
	})

	It("creates the failing node group's ConfigMap even though its StatefulSet build fails", func() {
		// An unparsable storage size is the one way buildStatefulSet fails, and the CRD's CEL
		// rules keep it out of the API, so this drives reconcileNodeGroup directly with
		// an in-memory cluster: the ConfigMap is written before the StatefulSet build fails.
		r := &NiFiClusterReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-mem", Namespace: namespace, UID: "in-memory-cluster"},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: "broken", Replicas: 1, Storage: "not-a-quantity"}},
			},
		}
		_, _, err := r.reconcileNodeGroup(ctx, cluster, &cluster.Spec.NodeGroups[0], false)
		Expect(err).To(MatchError(ContainSubstring("parsing storage quantity")))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-mem-broken-config", Namespace: namespace}, cm)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cm) })
		err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-mem-broken", Namespace: namespace}, &appsv1.StatefulSet{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	// One node group at a time: renaming it removes the old one.
	It("prunes a removed node group's generated resources instead of leaking them", func() {
		const removedNodeGroup = "removed"
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{{Name: removedNodeGroup, Replicas: 1, Storage: "1Gi"}},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		removedStsKey := types.NamespacedName{Name: clusterName + "-" + removedNodeGroup, Namespace: namespace}
		removedConfigMapKey := types.NamespacedName{Name: clusterName + "-" + removedNodeGroup + "-config", Namespace: namespace}
		removedSecretKey := types.NamespacedName{Name: clusterName + "-" + removedNodeGroup + "-sensitive-props", Namespace: namespace}

		// The old node group's resources exist before the spec is edited.
		Eventually(func() error {
			return k8sClient.Get(ctx, removedStsKey, &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: nodeGroup, Replicas: 1, Storage: "1Gi"}}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		// Every object generated for the removed node group is gone, not just left orphaned
		// until the whole NiFiCluster is deleted.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, removedStsKey, &appsv1.StatefulSet{}))
		}, timeout, interval).Should(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, removedConfigMapKey, &corev1.ConfigMap{}))
		}, timeout, interval).Should(BeTrue())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, removedSecretKey, &corev1.Secret{}))
		}, timeout, interval).Should(BeTrue())

		// The node group now in the spec is created.
		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}
		Eventually(func() error { return k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{}) }, timeout, interval).Should(Succeed())
	})

	// removing a node group must not decommission all its nodes at once.
	It("drains a removed node group one pod at a time, keeping its objects until the StatefulSet is gone", func() {
		const removed = "drained"
		clusterKey := types.NamespacedName{Name: clusterName, Namespace: namespace}
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: removed, Replicas: 2, Storage: "1Gi", PersistentVolumeClaimRetentionPolicy: &nifiv1alpha1.PVCRetentionPolicy{
						WhenDeleted: nifiv1alpha1.PVCRetain, WhenScaled: nifiv1alpha1.PVCDelete,
					}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		stsKey := types.NamespacedName{Name: clusterName + "-" + removed, Namespace: namespace}
		configKey := types.NamespacedName{Name: clusterName + "-" + removed + "-config", Namespace: namespace}
		sts := &appsv1.StatefulSet{}
		Eventually(func() error { return k8sClient.Get(ctx, stsKey, sts) }, timeout, interval).Should(Succeed())

		// envtest runs no StatefulSet controller: stand in for its two pods.
		pods := make([]*corev1.Pod, 2)
		for i := range pods {
			pods[i] = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", stsKey.Name, i), Namespace: namespace, Labels: sts.Spec.Selector.MatchLabels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nifi", Image: "apache/nifi:2.0.0"}}},
			}
			Expect(k8sClient.Create(ctx, pods[i])).To(Succeed())
			pod := pods[i]
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
		}
		// Any write to the cluster re-queues it; with no StatefulSet controller, nothing else does
		// when a stand-in pod goes.
		nudge := func(value string) {
			Eventually(func() error {
				latest := &nifiv1alpha1.NiFiCluster{}
				if err := k8sClient.Get(ctx, clusterKey, latest); err != nil {
					return err
				}
				latest.Annotations = map[string]string{"test/nudge": value}
				return k8sClient.Update(ctx, latest)
			}, timeout, interval).Should(Succeed())
		}
		replicas := func() int32 {
			got := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, stsKey, got); err != nil || got.Spec.Replicas == nil {
				return -1
			}
			return *got.Spec.Replicas
		}

		Eventually(func() error {
			latest := &nifiv1alpha1.NiFiCluster{}
			if err := k8sClient.Get(ctx, clusterKey, latest); err != nil {
				return err
			}
			latest.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: nodeGroup, Replicas: 1, Storage: "1Gi"}}
			return k8sClient.Update(ctx, latest)
		}, timeout, interval).Should(Succeed())

		By("removing one pod, then waiting for it to be gone")
		Eventually(replicas, timeout, interval).Should(Equal(int32(1)))
		nudge("1")
		Consistently(replicas, 2*time.Second, interval).Should(Equal(int32(1)))
		Expect(k8sClient.Get(ctx, configKey, &corev1.ConfigMap{})).To(Succeed(), "a draining group's ConfigMap was pruned")
		got := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, stsKey, got)).To(Succeed())
		Expect(got.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled).To(Equal(appsv1.RetainPersistentVolumeClaimRetentionPolicyType),
			"draining must apply whenDeleted (Retain), not whenScaled (Delete), to the group's PVCs")

		By("removing the next pod once the first is gone")
		Expect(k8sClient.Delete(ctx, pods[1])).To(Succeed())
		nudge("2")
		Eventually(replicas, timeout, interval).Should(Equal(int32(0)))
		Expect(k8sClient.Get(ctx, configKey, &corev1.ConfigMap{})).To(Succeed(), "a draining group's ConfigMap was pruned")

		By("deleting the StatefulSet and the group's objects once no pod is left")
		Expect(k8sClient.Delete(ctx, pods[0])).To(Succeed())
		nudge("3")
		Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})) }, timeout, interval).Should(BeTrue())
		Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, configKey, &corev1.ConfigMap{})) }, timeout, interval).Should(BeTrue())
	})

	It("evicts exactly one pod past its TLS certificate renewal window, one at a time", func() {
		validityDays := int32(2)
		cluster := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{
						Name: nodeGroup, Replicas: 2, Storage: "1Gi",
						TLS: &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: &validityDays},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		// envtest has no real kubelet, so no StatefulSet controller ever creates real Pods here -
		// fabricate the two this node group would have, past their renewal window (2 days
		// validity, 50% threshold = 1 day; started 2 days ago) and Ready, matching exactly what
		// reconcileTLSRotation's label selector looks for.
		makePod := func(name string) *corev1.Pod {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: namespace,
					Labels: map[string]string{labelAppInstance: clusterName, labelNodeGroup: nodeGroup},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nifi", Image: "apache/nifi:2.0.0"}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			pod.Status = corev1.PodStatus{
				StartTime:  &metav1.Time{Time: time.Now().Add(-48 * time.Hour)},
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			return pod
		}
		pod0 := makePod(clusterName + "-" + nodeGroup + "-0")
		_ = makePod(clusterName + "-" + nodeGroup + "-1")

		// Acceptance: a pod past its renewal window gets deleted (evicted).
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: pod0.Name, Namespace: namespace}, &corev1.Pod{}))
		}, timeout, interval).Should(BeTrue())

		// the rotation that just evicted pod0 also records a CertRotated Event.
		Eventually(func() bool {
			events := &corev1.EventList{}
			if err := k8sClient.List(ctx, events, client.InNamespace(namespace)); err != nil {
				return false
			}
			for _, e := range events.Items {
				if e.InvolvedObject.Name == clusterName && e.Reason == reasonCertRotated && e.Type == corev1.EventTypeNormal {
					return true
				}
			}
			return false
		}, timeout, interval).Should(BeTrue(), "no CertRotated event recorded for the evicted pod")

		// Acceptance: one at a time - the second pod is still there, since deleting the first
		// left it not-yet-existing/not-Ready, and the Ready-gate stops a second eviction in the
		// same or a subsequent reconcile until a replacement for pod0 exists and is Ready
		// (which never happens in this test, since there's no kubelet to create one).
		Consistently(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: clusterName + "-" + nodeGroup + "-1", Namespace: namespace}, &corev1.Pod{})
		}, "2s", interval).Should(Succeed())
	})

	It("recreates the StatefulSet after the NiFiCluster is deleted and recreated with the same name and config", func() {
		// Reproduces a real bug: the reconciler's in-memory templateHashCache was keyed only by
		// StatefulSet name+namespace, not tied to the owning NiFiCluster's identity. Deleting a
		// cluster and recreating one with the same name and identical config computes the exact
		// same template hash/replicas, so a stale cache entry from before the delete looked like
		// a match on the very first reconcile of the new object - the reconciler believed the
		// StatefulSet was already up to date and never actually created it, even though envtest
		// (like a real cluster's garbage collector) had already removed the old one. Only
		// restarting the operator process cleared the (pure in-memory) cache. Fixed by tying
		// each cache entry to the owning cluster's UID, so a recreated object - which always gets
		// a new UID - can never match a stale entry left over from the deleted one.
		buildSpec := func() nifiv1alpha1.NiFiClusterSpec {
			return nifiv1alpha1.NiFiClusterSpec{
				Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{
					{Name: nodeGroup, Replicas: 1, Storage: "1Gi"},
				},
			}
		}
		stsKey := types.NamespacedName{Name: clusterName + "-" + nodeGroup, Namespace: namespace}

		first := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec:       buildSpec(),
		}
		Expect(k8sClient.Create(ctx, first)).To(Succeed())

		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())
		firstUID := first.UID
		Expect(firstUID).NotTo(BeEmpty())

		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, &nifiv1alpha1.NiFiCluster{}))
		}, timeout, interval).Should(BeTrue())

		// envtest does not run Kubernetes' real garbage-collector controller, so an owned
		// StatefulSet is never actually cascade-deleted just because its owner is gone - unlike
		// a real cluster. Delete it explicitly here to simulate exactly what real GC would have
		// already done by this point, so the test reproduces the real-world starting condition
		// (old StatefulSet truly gone) rather than an envtest-only artifact (owner gone, child
		// still present, which would trivially pass for the wrong reason).
		// The finalizer may already have deleted it (it stops a Kubernetes-coordinated cluster's
		// pods before removing NiFi's Leases), hence IgnoreNotFound.
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsKey.Name, Namespace: stsKey.Namespace}}))).To(Succeed())
		deleteLeftovers(clusterName)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{}))
		}, timeout, interval).Should(BeTrue())

		second := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
			Spec:       buildSpec(), // identical config - same template hash, same replicas
		}
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		Expect(second.UID).NotTo(Equal(firstUID), "the recreated object must get a genuinely new UID for this test to be meaningful")

		// The actual regression check: without the UID fix, this would time out - the cache's
		// early-return on a "matching" name+namespace+hash+replicas entry meant the StatefulSet
		// was never (re)created at all.
		Eventually(func() error {
			return k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())
	})
})

// deleteLeftovers deletes the objects a deleted NiFiCluster left behind, which garbage
// collection would remove on a real cluster: the RBAC objects and the cluster-level Service the
// operator refuses to take over from a previous NiFiCluster of the same name (see getOwned).
func deleteLeftovers(clusterName string) {
	const namespace = "default"
	selector := client.MatchingLabels{labelAppInstance: clusterName, labelAppManagedBy: managedByValue}
	for _, obj := range []client.Object{&corev1.ServiceAccount{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}, &corev1.ConfigMap{}, &corev1.Secret{}} {
		Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, obj, client.InNamespace(namespace), selector))).To(Succeed())
	}
	// Services have no DeleteCollection endpoint.
	svcs := &corev1.ServiceList{}
	Expect(k8sClient.List(ctx, svcs, client.InNamespace(namespace), selector)).To(Succeed())
	for i := range svcs.Items {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &svcs.Items[i]))).To(Succeed())
	}
}
