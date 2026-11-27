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
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// The CRD's own validation rules. This envtest suite runs without the admission webhook,
// which is exactly the case these rules exist for: they must hold with the webhook disabled.
var _ = Describe("CRD validation rules", func() {
	const namespace = "default"

	group := func(name string) nifiv1alpha1.NodeGroup {
		return nifiv1alpha1.NodeGroup{Name: name, Replicas: 2, Storage: "1Gi"}
	}
	newCluster := func(name string, mutate func(*nifiv1alpha1.NiFiClusterSpec)) *nifiv1alpha1.NiFiCluster {
		c := &nifiv1alpha1.NiFiCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: nifiv1alpha1.NiFiClusterSpec{
				Image:      nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.12.0"},
				NodeGroups: []nifiv1alpha1.NodeGroup{group("a")},
			},
		}
		if mutate != nil {
			mutate(&c.Spec)
		}
		return c
	}
	expectRejected := func(c *nifiv1alpha1.NiFiCluster, msg string) {
		err := k8sClient.Create(ctx, c)
		if err == nil {
			_ = k8sClient.Delete(ctx, c)
		}
		ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "want Invalid, got %v", err)
		ExpectWithOffset(1, err.Error()).To(ContainSubstring(msg))
	}

	DescribeTable("rejects an invalid spec on create",
		func(mutate func(*nifiv1alpha1.NiFiClusterSpec), msg string) {
			expectRejected(newCluster("cel-reject", mutate), msg)
		},
		Entry("more than one node group", func(s *nifiv1alpha1.NiFiClusterSpec) {
			b := group("b")
			b.Replicas = 1
			s.NodeGroups = append(s.NodeGroups, b)
		}, "must have at most 1 item"),
		Entry("unparsable storage", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.NodeGroups[0].Storage = "lots"
		}, "must be a Kubernetes quantity"),
		Entry("unknown PVC retention type on the cluster", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.PersistentVolumeClaimRetentionPolicy = &nifiv1alpha1.PVCRetentionPolicy{WhenDeleted: "Archive"}
		}, `Unsupported value: "Archive"`),
		Entry("unknown PVC retention type on a node group", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.NodeGroups[0].PersistentVolumeClaimRetentionPolicy = &nifiv1alpha1.PVCRetentionPolicy{WhenScaled: "delete"}
		}, `Unsupported value: "delete"`),
		Entry("repository duration not in NiFi's format", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.Repositories = &nifiv1alpha1.RepositoriesSpec{Provenance: &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageTime: "30d ago"}}
		}, "should match"),
		Entry("archive usage over 100%", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.NodeGroups[0].Repositories = &nifiv1alpha1.RepositoriesSpec{ContentArchive: &nifiv1alpha1.ContentArchiveSpec{MaxUsagePercentage: ptr.To(int32(101))}}
		}, "less than or equal to 100"),
		Entry("provenance size not a positive quantity", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.Repositories = &nifiv1alpha1.RepositoriesSpec{Provenance: &nifiv1alpha1.ProvenanceRepositorySpec{MaxStorageSize: "0"}}
		}, "must be a positive Kubernetes quantity"),
		Entry("more than 3 replicas in one node group", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.NodeGroups[0].Replicas = 4
		}, "at most 3 replicas in one node group"),
		Entry("PDB minAvailable not below replicas", func(s *nifiv1alpha1.NiFiClusterSpec) {
			n := int32(2)
			s.NodeGroups[0].PodDisruptionBudget = &nifiv1alpha1.PodDisruptionBudgetSpec{MinAvailable: &n}
		}, "minAvailable must be lower than replicas"),
		Entry("certificate validity over 10 years", func(s *nifiv1alpha1.NiFiClusterSpec) {
			d := int32(4000)
			s.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: &d}
		}, "certificateValidityDays must be at most 3650"),
		Entry("ZooKeeper coordination", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.Coordination = &nifiv1alpha1.CoordinationSpec{Backend: nifiv1alpha1.CoordinationBackendZooKeeper,
				ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: "zk-0.zk:2181"}}
		}, `Unsupported value: "ZooKeeper"`),
		Entry("zookeeper settings on Kubernetes coordination", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.Coordination = &nifiv1alpha1.CoordinationSpec{Backend: nifiv1alpha1.CoordinationBackendKubernetes,
				ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: "zk-0.zk:2181"}}
		}, "zookeeper must be set when backend is ZooKeeper, and only then"),
		Entry("NodePort service type", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.ServiceType = corev1.ServiceTypeNodePort
		}, `Unsupported value: "NodePort"`),
		Entry("reserved env variable", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{Env: []corev1.EnvVar{{Name: "NIFI_AUTH_PASSWORD", Value: "x"}}}
		}, "env must not set a variable the operator sets itself"),
		Entry("reserved volume name", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{Volumes: []corev1.Volume{{Name: "tls"}}}
		}, "volumes must not use a name the operator uses itself"),
		Entry("sidecar named nifi", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{Sidecars: []corev1.Container{{Name: "nifi", Image: "x"}}}
		}, "must not be named nifi"),
		Entry("empty logback override", func(s *nifiv1alpha1.NiFiClusterSpec) {
			empty := ""
			s.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{Logback: &empty}
		}, "spec.configOverrides.logback"),
		Entry("singleUser with both modes", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.Authentication = &nifiv1alpha1.AuthenticationSpec{SingleUser: &nifiv1alpha1.SingleUserAuthSpec{
				Username: "admin", Password: "long-enough-password", SecretName: "login"}}
		}, "exactly one of username/password or secretName"),
		Entry("singleUser short password", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.Authentication = &nifiv1alpha1.AuthenticationSpec{SingleUser: &nifiv1alpha1.SingleUserAuthSpec{
				Username: "admin", Password: "short"}}
		}, "password must be at least 12 characters"),
	)

	It("rejects the operator's own login Secret as singleUser.secretName", func() {
		expectRejected(newCluster("cel-own", func(s *nifiv1alpha1.NiFiClusterSpec) {
			s.Authentication = &nifiv1alpha1.AuthenticationSpec{SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "cel-own-auth-credentials"}}
		}), "operator-managed login Secret")
	})

	It("accepts every sample manifest", func() {
		files, err := filepath.Glob("../../config/samples/*.yaml")
		Expect(err).NotTo(HaveOccurred())
		checked := 0
		for _, f := range files {
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			for _, doc := range strings.Split(string(data), "\n---") {
				if !strings.Contains(doc, "kind: NiFiCluster") {
					continue
				}
				c := &nifiv1alpha1.NiFiCluster{}
				Expect(yaml.Unmarshal([]byte(doc), c)).To(Succeed(), f)
				c.Namespace = namespace
				c.Name = "sample-" + c.Name
				Expect(k8sClient.Create(ctx, c, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})).To(Succeed(), f)
				checked++
			}
		}
		Expect(checked).To(BeNumerically(">=", 5))
	})

	It("keeps an existing node group's volume sizes, also through a rename", func() {
		c := newCluster("cel-immutable", nil)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			deleteLeftovers(c.Name)
		})
		// Retried on conflicts: the operator writes the object too (finalizer, status).
		update := func(mutate func(*nifiv1alpha1.NiFiClusterSpec)) error {
			var err error
			for range 20 {
				latest := &nifiv1alpha1.NiFiCluster{}
				if err = k8sClient.Get(ctx, types.NamespacedName{Name: c.Name, Namespace: namespace}, latest); err != nil {
					return err
				}
				mutate(&latest.Spec)
				if err = k8sClient.Update(ctx, latest); !apierrors.IsConflict(err) {
					return err
				}
			}
			return err
		}

		err := update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].Storage = "512Mi" })
		Expect(err).To(MatchError(ContainSubstring("storage: storage size changes are not supported in the Community edition")))
		err = update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].LogStorage = "1Mi" })
		Expect(err).To(MatchError(ContainSubstring("logStorage: storage size changes are not supported")))
		err = update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].Storage = "2Gi" })
		Expect(err).To(MatchError(ContainSubstring("storage: storage size changes are not supported")))
		err = update(func(s *nifiv1alpha1.NiFiClusterSpec) { sc := "fast"; s.NodeGroups[0].StorageClassName = &sc })
		Expect(err).To(MatchError(ContainSubstring("storageClassName cannot be changed")))

		// The same size written differently is no change.
		Expect(update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].Storage = "1024Mi" })).To(Succeed())
		// Renaming the only node group doesn't change the sizes either; a rename alone is allowed.
		err = update(func(s *nifiv1alpha1.NiFiClusterSpec) {
			b := group("b")
			b.Storage = "5Gi"
			s.NodeGroups = []nifiv1alpha1.NodeGroup{b}
		})
		Expect(err).To(MatchError(ContainSubstring("storage: a renamed node group keeps the storage size")))
		Expect(update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups = []nifiv1alpha1.NodeGroup{group("b")} })).To(Succeed())
	})

	// NIFI-248 B9: a cluster already over the node limit - as happens when a CRD upgrade adds the
	// limit on top of an existing, previously-unconstrained install - must stay usable: the ratchet
	// rule has to permit no-op and unrelated updates, not just a strict step down, or the first
	// write the operator itself makes (a status/finalizer update touches the resource too) would be
	// permanently rejected.
	It("lets an already over-limit cluster (from before a CRD upgrade added the node limit) keep being updated", func() {
		crdClient, err := apiextensionsclientset.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		crds := crdClient.ApiextensionsV1().CustomResourceDefinitions()

		original, err := crds.Get(ctx, "nificlusters.nifi.ksolves.com", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		restore := func() {
			Eventually(func() error {
				current, err := crds.Get(ctx, "nificlusters.nifi.ksolves.com", metav1.GetOptions{})
				if err != nil {
					return err
				}
				current.Spec = original.Spec
				_, err = crds.Update(ctx, current, metav1.UpdateOptions{})
				return err
			}, "10s", "200ms").Should(Succeed())
		}
		DeferCleanup(restore)

		// Simulate the pre-upgrade CRD: the same schema with both node-limit CEL rules removed, as
		// if this were a version from before NIFI-248 B8 added them.
		withoutLimit, err := crds.Get(ctx, "nificlusters.nifi.ksolves.com", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		for i := range withoutLimit.Spec.Versions {
			v := &withoutLimit.Spec.Versions[i]
			specProps := v.Schema.OpenAPIV3Schema.Properties["spec"]
			rules := specProps.XValidations[:0]
			for _, r := range specProps.XValidations {
				if !strings.Contains(r.Message, "at most 3 NiFi nodes") {
					rules = append(rules, r)
				}
			}
			specProps.XValidations = rules
			replicas := specProps.Properties["nodeGroups"].Items.Schema.Properties["replicas"]
			replicas.Maximum = nil
			replicasRules := replicas.XValidations[:0]
			for _, r := range replicas.XValidations {
				if !strings.Contains(r.Message, "at most 3 replicas") {
					replicasRules = append(replicasRules, r)
				}
			}
			replicas.XValidations = replicasRules
			specProps.Properties["nodeGroups"].Items.Schema.Properties["replicas"] = replicas
			v.Schema.OpenAPIV3Schema.Properties["spec"] = specProps
		}
		_, err = crds.Update(ctx, withoutLimit, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// A CRD schema update does not take effect in the apiserver's validation the instant
		// Update() returns - the CRD controller re-establishes it asynchronously - so retry the
		// create until the relaxed schema has actually propagated, rather than assuming it has.
		c := newCluster("cel-legacy-over-limit", func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].Replicas = 5 })
		Eventually(func() error { return k8sClient.Create(ctx, c) }, "10s", "100ms").Should(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, c)
			deleteLeftovers(c.Name)
		})

		// The CRD upgrade: the limit's CEL rules land on top of the already over-limit object.
		restore()

		latest := &nifiv1alpha1.NiFiCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: c.Name, Namespace: namespace}, latest)).To(Succeed())
		Expect(latest.Spec.NodeGroups[0].Replicas).To(Equal(int32(5)), "the stored object is never re-validated by the CRD upgrade alone")

		// Retried on conflicts: NIFI-248 B1(b) made the operator keep reconciling (status,
		// ConfigMaps, ...) even over the node limit, instead of freezing - this cluster's own
		// resourceVersion can now move between this test's Get and Update far more often than
		// before that fix.
		updateObj := func(mutate func(*nifiv1alpha1.NiFiCluster)) error {
			var err error
			for range 20 {
				current := &nifiv1alpha1.NiFiCluster{}
				if err = k8sClient.Get(ctx, types.NamespacedName{Name: c.Name, Namespace: namespace}, current); err != nil {
					return err
				}
				mutate(current)
				if err = k8sClient.Update(ctx, current); !apierrors.IsConflict(err) {
					return err
				}
			}
			return err
		}
		update := func(mutate func(*nifiv1alpha1.NiFiClusterSpec)) error {
			return updateObj(func(c *nifiv1alpha1.NiFiCluster) { mutate(&c.Spec) })
		}

		By("an unrelated update (e.g. a label, or the operator's own status/finalizer write)")
		Expect(updateObj(func(c *nifiv1alpha1.NiFiCluster) { c.Labels = map[string]string{"test": "unrelated"} })).To(Succeed())

		By("a no-op update that leaves replicas at its already over-limit value")
		Expect(update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].Replicas = 5 })).To(Succeed())

		By("a further increase is still rejected")
		err = update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].Replicas = 6 })
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "increase past an already over-limit value: %v", err)

		By("a step down is still allowed")
		Expect(update(func(s *nifiv1alpha1.NiFiClusterSpec) { s.NodeGroups[0].Replicas = 4 })).To(Succeed())
	})

	// Earlier versions stored tls.enabled: false as an absent field. The CRD defaults it to false,
	// never true, or every such running node group would switch to TLS.
	It("leaves a tls block without enabled as TLS off", func() {
		c := newCluster("cel-tls-absent", nil)
		obj := map[string]any{}
		data, err := yaml.Marshal(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(yaml.Unmarshal(data, &obj)).To(Succeed())
		groups := obj["spec"].(map[string]any)["nodeGroups"].([]any)
		groups[0].(map[string]any)["tls"] = map[string]any{"extraSANs": []any{"nifi.example.com"}}
		u := &unstructured.Unstructured{Object: obj}
		u.SetGroupVersionKind(nifiv1alpha1.GroupVersion.WithKind("NiFiCluster"))
		Expect(k8sClient.Create(ctx, u, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})).To(Succeed())
		tls, found, err := unstructured.NestedMap(u.Object["spec"].(map[string]any)["nodeGroups"].([]any)[0].(map[string]any), "tls")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(tls).To(HaveKeyWithValue("enabled", false))
	})
})

// The CRD's CEL rules repeat the operator's reserved-name lists; this keeps the copies in step.
var _ = Describe("CRD validation rules stay in sync with the operator's reserved names", func() {
	It("lists every reserved env variable and volume name", func() {
		data, err := os.ReadFile("../../config/crd/bases/nifi.ksolves.com_nificlusters.yaml")
		Expect(err).NotTo(HaveOccurred())
		crd := string(data)
		for name := range ReservedPodEnvNames {
			Expect(crd).To(ContainSubstring("'"+name+"'"), "env %s missing from the CRD rule", name)
		}
		for name := range ReservedVolumeNames {
			Expect(crd).To(ContainSubstring("'"+name+"'"), "volume %s missing from the CRD rule", name)
		}
	})
})
