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

package v1alpha1

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/controller"
)

const invalidQuantity = "not-a-quantity"

func ptrBool(b bool) *bool    { return &b }
func ptrInt32(i int32) *int32 { return &i }

func minimalNodeGroup(name string) nifiv1alpha1.NodeGroup {
	return nifiv1alpha1.NodeGroup{
		Name:     name,
		Replicas: 3,
		Storage:  "10Gi",
	}
}

var _ = Describe("NiFiCluster Webhook", func() {
	var (
		obj       *nifiv1alpha1.NiFiCluster
		oldObj    *nifiv1alpha1.NiFiCluster
		validator NiFiClusterCustomValidator
		defaulter NiFiClusterCustomDefaulter
	)

	BeforeEach(func() {
		obj = &nifiv1alpha1.NiFiCluster{
			Spec: nifiv1alpha1.NiFiClusterSpec{
				NodeGroups: []nifiv1alpha1.NodeGroup{minimalNodeGroup("default")},
			},
		}
		oldObj = obj.DeepCopy()
		validator = NiFiClusterCustomValidator{}
		defaulter = NiFiClusterCustomDefaulter{}
	})

	Context("When creating NiFiCluster under Defaulting Webhook", func() {
		It("should fill in requests and limits when none are set", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			ng := obj.Spec.NodeGroups[0]
			Expect(ng.Resources.Requests[corev1.ResourceCPU]).To(Equal(defaultCPURequest))
			Expect(ng.Resources.Requests[corev1.ResourceMemory]).To(Equal(defaultMemoryRequest))
			Expect(ng.Resources.Limits[corev1.ResourceCPU]).To(Equal(defaultCPULimit))
			Expect(ng.Resources.Limits[corev1.ResourceMemory]).To(Equal(defaultMemoryLimit))
		})

		It("should leave a user-specified resource value untouched", func() {
			customCPURequest := resource.MustParse("1")
			obj.Spec.NodeGroups[0].Resources.Requests = corev1.ResourceList{
				corev1.ResourceCPU: customCPURequest,
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			ng := obj.Spec.NodeGroups[0]
			Expect(ng.Resources.Requests[corev1.ResourceCPU]).To(Equal(customCPURequest))
			Expect(ng.Resources.Requests[corev1.ResourceMemory]).To(Equal(defaultMemoryRequest))
		})

		It("should raise a defaulted limit to a larger user request", func() {
			obj.Spec.NodeGroups[0].Resources.Requests = corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			ng := obj.Spec.NodeGroups[0]
			Expect(ng.Resources.Limits[corev1.ResourceCPU]).To(Equal(resource.MustParse("3")))
			Expect(ng.Resources.Limits[corev1.ResourceMemory]).To(Equal(resource.MustParse("4Gi")))
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should lower a defaulted request to a smaller user limit", func() {
			obj.Spec.NodeGroups[0].Resources.Limits = corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			ng := obj.Spec.NodeGroups[0]
			Expect(ng.Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("250m")))
			Expect(ng.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("512Mi")))
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should fill in the default node image when none is set", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Image).To(Equal(controller.DefaultNiFiImage))
		})

		It("should keep a user-specified image", func() {
			image := nifiv1alpha1.ImageSpec{Repository: "repo/nifi", Tag: "2.12.0"}
			obj.Spec.Image = image
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Image).To(Equal(image))
		})
	})

	Context("validateImage", func() {
		withImage := func(repository, tag string) *nifiv1alpha1.NiFiCluster {
			c := obj.DeepCopy()
			c.Spec.Image = nifiv1alpha1.ImageSpec{Repository: repository, Tag: tag}
			return c
		}

		It("should deny a repository without a tag", func() {
			_, errs := validateImage(nil, withImage("repo/nifi", ""))
			Expect(errs).NotTo(BeEmpty())
		})

		It("should deny a NiFi version older than 2.11.0", func() {
			warnings, errs := validateImage(nil, withImage("repo/nifi", "2.10.0"))
			Expect(warnings).To(BeEmpty())
			Expect(errs).NotTo(BeEmpty())
			Expect(errs.ToAggregate().Error()).To(ContainSubstring("2.11.0"))
		})

		It("should admit NiFi 2.11.0 or later", func() {
			for _, tag := range []string{"2.11.0", "nifi-node-2.12.0-0.12.0"} {
				warnings, errs := validateImage(nil, withImage("repo/nifi", tag))
				Expect(errs).To(BeEmpty(), "tag %s", tag)
				Expect(warnings).To(BeEmpty(), "tag %s", tag)
			}
		})

		It("should warn, not deny, a tag without a version", func() {
			warnings, errs := validateImage(nil, withImage("repo/nifi", "latest"))
			Expect(errs).To(BeEmpty())
			Expect(warnings).To(ContainElement(ContainSubstring("names no NiFi version")))
		})

		It("should admit an unset image", func() {
			warnings, errs := validateImage(nil, withImage("", ""))
			Expect(errs).To(BeEmpty())
			Expect(warnings).To(BeEmpty())
		})

		It("should not re-check an unchanged image on update", func() {
			warnings, errs := validateImage(withImage("repo/nifi", "2.10.0"), withImage("repo/nifi", "2.10.0"))
			Expect(errs).To(BeEmpty())
			Expect(warnings).To(BeEmpty())
		})

		It("should deny changing the image to a too-old NiFi on update", func() {
			_, errs := validateImage(withImage("repo/nifi", "2.12.0"), withImage("repo/nifi", "2.10.0"))
			Expect(errs).NotTo(BeEmpty())
		})

		zk := func(tag string) *nifiv1alpha1.NiFiCluster {
			c := withImage("repo/nifi", tag)
			c.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{
				Backend:   nifiv1alpha1.CoordinationBackendZooKeeper,
				ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: "zk:2181"},
			}
			return c
		}

		It("should admit NiFi 1.14.0 and later with ZooKeeper coordination", func() {
			for _, tag := range []string{"1.14.0", "nifi-node-1.28.1-0.13.1", "2.6.0", "2.10.0", "2.12.0"} {
				_, errs := validateImage(nil, zk(tag))
				Expect(errs).To(BeEmpty(), "tag %s", tag)
			}
			_, errs := validateImage(nil, zk("1.13.2"))
			Expect(errs.ToAggregate().Error()).To(ContainSubstring("1.14.0"))
		})

		It("should reject NiFi before 2.11.0, which needs ZooKeeper coordination", func() {
			_, errs := validateImage(nil, withImage("repo/nifi", "1.28.1"))
			Expect(errs.ToAggregate().Error()).To(And(ContainSubstring("2.11.0"),
				ContainSubstring("the only coordination the Community edition supports")))
		})

		It("should re-check the image when the coordination backend changes", func() {
			_, errs := validateImage(zk("1.28.1"), withImage("repo/nifi", "1.28.1"))
			Expect(errs).NotTo(BeEmpty())
		})

		It("should warn about moving between NiFi major versions", func() {
			warnings, errs := validateImage(zk("1.28.1"), zk("2.12.0"))
			Expect(errs).To(BeEmpty())
			Expect(warnings).To(ContainElement(ContainSubstring("two major versions")))
			warnings, _ = validateImage(zk("1.27.0"), zk("1.28.1"))
			Expect(warnings).To(BeEmpty())
		})
	})

	Context("When creating or updating NiFiCluster under Validating Webhook", func() {
		It("should admit a valid spec", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should deny node groups with different TLS settings", func() {
			secured := minimalNodeGroup("secured")
			secured.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			obj.Spec.NodeGroups = append(obj.Spec.NodeGroups, secured)
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.nodeGroups[1].tls.enabled"))
		})

		It("should admit multiple node groups that share a TLS setting", func() {
			first := minimalNodeGroup("default")
			first.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			first.Replicas = 2
			second := minimalNodeGroup("workers")
			second.Replicas = 1
			second.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			obj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{first, second}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should deny more than 3 NiFi nodes across node groups", func() {
			second := minimalNodeGroup("workers")
			second.Replicas = 1
			obj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{minimalNodeGroup("default"), second}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("would have 4 NiFi nodes; at most 3"))
		})

		It("should admit an update that shrinks a cluster already over 3 nodes, or deletes it", func() {
			big := minimalNodeGroup("workers")
			oldObj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{minimalNodeGroup("default"), big}
			smaller := minimalNodeGroup("workers")
			smaller.Replicas = 2
			obj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{minimalNodeGroup("default"), smaller}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())

			obj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{minimalNodeGroup("default"), minimalNodeGroup("workers")}
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Spec.NodeGroups[1].Replicas = 3
			_, err = validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should deny overriding the coordination properties the operator manages", func() {
			obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{NifiProperties: map[string]string{
				"nifi.cluster.leader.election.kubernetes.lease.prefix": "other",
			}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
		})

		It("should validate the external ZooKeeper settings", func() {
			zk := func(cs, root string) {
				obj.Spec.Coordination = &nifiv1alpha1.CoordinationSpec{
					Backend:   nifiv1alpha1.CoordinationBackendZooKeeper,
					ZooKeeper: &nifiv1alpha1.ZooKeeperSpec{ConnectString: cs, RootNode: root},
				}
			}
			zk("zk-0.zk:2181,10.0.0.5:2181", "/teams/nifi")
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			for _, bad := range [][2]string{
				{"zk-0.zk:2181/chroot", ""}, {"zk-0.zk", ""}, {"zk-0.zk:99999", ""},
				{"zk-0.zk:2181", "/zookeeper/x"}, {"zk-0.zk:2181", "/a/../b"},
			} {
				zk(bad[0], bad[1])
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred(), "connectString=%q rootNode=%q", bad[0], bad[1])
			}
		})

		It("should validate the cluster Service options", func() {
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			obj.Spec.Service = &nifiv1alpha1.ClusterServiceSpec{LoadBalancerSourceRanges: []string{"10.0.0.0/8"}}
			obj.Spec.ServiceType = corev1.ServiceTypeLoadBalancer
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())

			obj.Spec.Service = &nifiv1alpha1.ClusterServiceSpec{LoadBalancerSourceRanges: []string{"not-a-cidr"}}
			_, err = validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
		})

		It("should warn about an unrestricted LoadBalancer and ignored settings", func() {
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			obj.Spec.ServiceType = corev1.ServiceTypeLoadBalancer
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ContainElement(ContainSubstring("accepts connections from any address")))

			obj.Spec.ServiceType = corev1.ServiceTypeClusterIP
			obj.Spec.Service = &nifiv1alpha1.ClusterServiceSpec{ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal}
			warnings, err = validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ContainElement(ContainSubstring("externalTrafficPolicy only applies")))
		})

		It("should deny duplicate NodeGroup names", func() {
			obj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{
				minimalNodeGroup("dup"),
				minimalNodeGroup("dup"),
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("dup"))
		})

		It("should deny an invalid Storage quantity", func() {
			obj.Spec.NodeGroups[0].Storage = invalidQuantity
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("storage"))
		})

		It("should deny an invalid LogStorage quantity", func() {
			obj.Spec.NodeGroups[0].LogStorage = invalidQuantity
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("logStorage"))
		})

		It("should deny a PDB minAvailable that is not less than Replicas", func() {
			obj.Spec.NodeGroups[0].PodDisruptionBudget = &nifiv1alpha1.PodDisruptionBudgetSpec{
				Enabled:      ptrBool(true),
				MinAvailable: ptrInt32(3),
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("minAvailable"))
		})

		It("should deny a request above its limit", func() {
			obj.Spec.NodeGroups[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("resources.requests[memory]"))
		})

		It("should deny a negative PDB minAvailable", func() {
			obj.Spec.NodeGroups[0].PodDisruptionBudget = &nifiv1alpha1.PodDisruptionBudgetSpec{
				Enabled:      ptrBool(true),
				MinAvailable: ptrInt32(-1),
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("minAvailable"))
		})

		It("should admit a PDB minAvailable that is less than Replicas", func() {
			obj.Spec.NodeGroups[0].PodDisruptionBudget = &nifiv1alpha1.PodDisruptionBudgetSpec{
				Enabled:      ptrBool(true),
				MinAvailable: ptrInt32(2),
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should deny a zero CertificateValidityDays when TLS is enabled", func() {
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: ptrInt32(0)}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("certificateValidityDays"))
		})

		It("should deny a negative CertificateValidityDays when TLS is enabled", func() {
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: ptrInt32(-1)}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("certificateValidityDays"))
		})

		It("should deny an absurdly large CertificateValidityDays when TLS is enabled", func() {
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: ptrInt32(100000)}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("certificateValidityDays"))
		})

		It("should admit a sane CertificateValidityDays when TLS is enabled", func() {
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: ptrInt32(365)}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not validate CertificateValidityDays when TLS is disabled", func() {
			enabled := false
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: enabled, CertificateValidityDays: ptrInt32(-1)}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should validate updates using the same rules", func() {
			obj.Spec.NodeGroups[0].Storage = invalidQuantity
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
		})

		It("should warn, not deny, when an update removes or renames a NodeGroup", func() {
			tlsOnGroup := func(name string) nifiv1alpha1.NodeGroup {
				g := minimalNodeGroup(name)
				g.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
				return g
			}
			oldObj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{tlsOnGroup("default"), tlsOnGroup("extra")}
			obj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{tlsOnGroup("renamed")}
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(2))
			Expect(warnings[0]).To(ContainSubstring(`nodeGroup "default" removed`))
			Expect(warnings[1]).To(ContainSubstring(`nodeGroup "extra" removed`))
		})

		It("should not warn when an update keeps every NodeGroup", func() {
			obj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			oldObj.Spec.NodeGroups[0].TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			obj.Spec.NodeGroups[0].Replicas = 2
			added := minimalNodeGroup("added")
			added.Replicas = 1
			added.TLS = &nifiv1alpha1.TLSSpec{Enabled: true}
			obj.Spec.NodeGroups = append(obj.Spec.NodeGroups, added)
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("should not error on delete", func() {
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("SplitRepositoryStorage immutability", func() {
			It("should admit enabling SplitRepositoryStorage on a brand-new node group", func() {
				oldObj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{minimalNodeGroup("default")}
				newGroup := minimalNodeGroup("added")
				newGroup.Replicas = 1
				newGroup.SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true}
				existing := minimalNodeGroup("default")
				existing.Replicas = 2
				obj.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{existing, newGroup}
				_, err := validator.ValidateUpdate(ctx, oldObj, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny flipping SplitRepositoryStorage.Enabled on an existing node group", func() {
				oldObj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: false}
				obj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true}
				_, err := validator.ValidateUpdate(ctx, oldObj, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("splitRepositoryStorage.enabled"))
			})

			It("should admit an update that leaves SplitRepositoryStorage.Enabled unchanged", func() {
				oldObj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true}
				obj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true}
				_, err := validator.ValidateUpdate(ctx, oldObj, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			// Sizes are fixed; growing is covered by TestValidateStorageChanges.
			It("should deny shrinking a repository on an existing node group", func() {
				oldObj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true, ContentRepositoryStorage: "50Gi"}
				obj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{Enabled: true, ContentRepositoryStorage: "10Gi"}
				_, err := validator.ValidateUpdate(ctx, oldObj, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("splitRepositoryStorage.contentRepositoryStorage: Forbidden: cannot change from 50Gi to 10Gi"))
			})
		})

		Context("SplitRepositoryStorage size validation", func() {
			It("should admit valid custom sizes for every repository", func() {
				obj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{
					Enabled:                     true,
					FlowFileRepositoryStorage:   "7Gi",
					ContentRepositoryStorage:    "50Gi",
					ProvenanceRepositoryStorage: "3Gi",
					DatabaseRepositoryStorage:   "4Gi",
					ExtensionsStorage:           "11Gi",
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should admit an unset SplitRepositoryStorage (defaults apply later)", func() {
				obj.Spec.NodeGroups[0].SplitRepositoryStorage = nil
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny an invalid content repository size", func() {
				obj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{
					Enabled: true, ContentRepositoryStorage: invalidQuantity,
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("splitRepositoryStorage.contentRepositoryStorage"))
			})

			It("should deny an invalid extensions storage size", func() {
				obj.Spec.NodeGroups[0].SplitRepositoryStorage = &nifiv1alpha1.SplitRepositoryStorageSpec{
					Enabled: true, ExtensionsStorage: invalidQuantity,
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("splitRepositoryStorage.extensionsStorage"))
			})
		})

		Context("with a Client available for StorageClass lookups", func() {
			var scValidator NiFiClusterCustomValidator

			BeforeEach(func() {
				scValidator = NiFiClusterCustomValidator{Client: k8sClient}
			})

			AfterEach(func() {
				_ = k8sClient.Delete(ctx, &storagev1.StorageClass{
					ObjectMeta: metav1.ObjectMeta{Name: "nfs-backed"},
				})
				_ = k8sClient.Delete(ctx, &storagev1.StorageClass{
					ObjectMeta: metav1.ObjectMeta{Name: "regular-csi"},
				})
			})

			It("should deny a NodeGroup using an NFS-backed StorageClass", func() {
				sc := &storagev1.StorageClass{
					ObjectMeta:  metav1.ObjectMeta{Name: "nfs-backed"},
					Provisioner: "nfs.csi.k8s.io",
				}
				Expect(k8sClient.Create(ctx, sc)).To(Succeed())

				storageClassName := "nfs-backed"
				obj.Spec.NodeGroups[0].StorageClassName = &storageClassName
				_, err := scValidator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("NFS/CIFS"))
			})

			It("should admit a NodeGroup using a non-NFS/CIFS StorageClass", func() {
				sc := &storagev1.StorageClass{
					ObjectMeta:  metav1.ObjectMeta{Name: "regular-csi"},
					Provisioner: "kubernetes.io/aws-ebs",
				}
				Expect(k8sClient.Create(ctx, sc)).To(Succeed())

				storageClassName := "regular-csi"
				obj.Spec.NodeGroups[0].StorageClassName = &storageClassName
				_, err := scValidator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should admit a NodeGroup referencing a StorageClass that does not exist yet", func() {
				storageClassName := "does-not-exist"
				obj.Spec.NodeGroups[0].StorageClassName = &storageClassName
				_, err := scValidator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("ProxyHosts validation", func() {
			It("should admit valid proxyHosts entries, including hostname:port", func() {
				obj.Spec.ProxyHosts = []string{"nifi.example.com", "10.0.0.5:30080"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny a proxyHosts entry with an invalid port", func() {
				obj.Spec.ProxyHosts = []string{"nifi.example.com:not-a-port"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("proxyHosts"))
			})

			It("should deny a malformed proxyHosts hostname", func() {
				obj.Spec.ProxyHosts = []string{"not_a_valid_hostname"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("proxyHosts"))
			})
		})

		Context("Auth Secret name", func() {
			It("should deny a secretName that names the operator-managed login Secret", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: obj.Name + "-auth-credentials"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("operator-managed login Secret"))
			})
		})

		Context("ConfigOverrides validation", func() {
			It("should admit a non-reserved cluster-level nifi.properties override", func() {
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "60 secs"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should admit a non-reserved node-group-level nifi.properties override", func() {
				obj.Spec.NodeGroups[0].ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					NifiProperties: map[string]string{"nifi.ui.autorefresh.interval": "60 secs"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny a cluster-level override of an operator-managed key", func() {
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					NifiProperties: map[string]string{"nifi.zookeeper.connect.string": "evil:2181"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("configOverrides.nifiProperties"))
			})

			It("should deny a line break in an override value (java.arg / reserved-key injection)", func() {
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					NifiProperties: map[string]string{"nifi.ui.banner.text": "hi\nnifi.cluster.is.node=false"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("line breaks"))
			})

			It("should deny an escaped key that Java would decode to a reserved key", func() {
				obj.Spec.NodeGroups[0].ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					NifiProperties: map[string]string{"nifi\\u002ecluster.is.node": "false"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("configOverrides.nifiProperties"))
			})

			It("should deny a bootstrap.conf key with whitespace and a value with a line break", func() {
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					BootstrapConf: map[string]string{"java.arg.20 ": "-Xmx1g", "java.arg.21": "-Xms1g\njava.arg.99=-Devil"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("configOverrides.bootstrapConf"))
				Expect(err.Error()).To(ContainSubstring("line breaks"))
			})

			It("should admit a backslash in an override value (Windows-style path)", func() {
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					NifiProperties: map[string]string{"nifi.ui.banner.text": `C:\data`},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny a node-group-level override of an operator-managed key", func() {
				obj.Spec.NodeGroups[0].ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					NifiProperties: map[string]string{"nifi.security.keystore": "/tmp/evil.p12"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("configOverrides.nifiProperties"))
			})

			It("should admit a bootstrap.conf override", func() {
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{
					BootstrapConf: map[string]string{"java.arg.3": "-Xmx2g"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should admit a non-empty logback/authorizers override", func() {
				content := "<configuration></configuration>"
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{Logback: &content, Authorizers: &content}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny an empty logback override", func() {
				empty := ""
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{Logback: &empty}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("configOverrides.logback"))
			})

			It("should deny an empty authorizers override", func() {
				empty := ""
				obj.Spec.ConfigOverrides = &nifiv1alpha1.ConfigOverridesSpec{Authorizers: &empty}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("configOverrides.authorizers"))
			})
		})

		Context("PodOverrides validation", func() {
			It("should admit non-colliding PodOverrides", func() {
				obj.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{
					Env:            []corev1.EnvVar{{Name: "MY_VAR", Value: "hello"}},
					InitContainers: []corev1.Container{{Name: "wait-for-dns", Image: "busybox"}},
					Sidecars:       []corev1.Container{{Name: "log-shipper", Image: "fluent-bit"}},
					Volumes:        []corev1.Volume{{Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny an Env name reserved by the operator", func() {
				obj.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{
					Env: []corev1.EnvVar{{Name: "POD_NAME", Value: "evil"}},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("podOverrides.env"))
			})

			It("should deny an InitContainer named the same as the operator's main container", func() {
				obj.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{
					InitContainers: []corev1.Container{{Name: "nifi", Image: "evil"}},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("podOverrides.initContainers"))
			})

			It("should deny a Sidecar named the same as the operator's main container", func() {
				obj.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{
					Sidecars: []corev1.Container{{Name: "nifi", Image: "evil"}},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("podOverrides.sidecars"))
			})

			It("should deny a Volume name reserved by the operator", func() {
				obj.Spec.NodeGroups[0].PodOverrides = &nifiv1alpha1.PodOverridesSpec{
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("podOverrides.volumes"))
			})
		})

		Context("Authentication validation", func() {
			It("should admit a valid username/password pair", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "SuperSecret123!"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should admit a valid secretName reference when no Client is available", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "nifi-login"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should deny setting both username/password and secretName", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{
						Username: "admin", Password: "SuperSecret123!", SecretName: "nifi-login",
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exactly one of username/password or secretName"))
			})

			It("should deny setting neither username/password nor secretName", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exactly one of username/password or secretName"))
			})

			It("should deny a username with no password", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("password"))
			})

			It("should deny a literal password shorter than 12 characters without echoing it", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "tooShort1!"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("at least 12 characters"))
				Expect(err.Error()).NotTo(ContainSubstring("tooShort1!"))
			})

			It("should deny a literal password longer than 72 bytes without echoing it", func() {
				pw := strings.Repeat("a", 73)
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: pw},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("at most 72 bytes"))
				Expect(err.Error()).NotTo(ContainSubstring(pw))
			})

			It("should admit a literal password of exactly 72 bytes", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: strings.Repeat("a", 72)},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should apply the password rules on update too", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Username: "admin", Password: "tooShort1!"},
				}
				_, err := validator.ValidateUpdate(ctx, oldObj, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("at least 12 characters"))
			})

			It("should deny a password with no username", func() {
				obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
					SingleUser: &nifiv1alpha1.SingleUserAuthSpec{Password: "SuperSecret123!"},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("username"))
			})

			Context("with a Client available for Secret lookups", func() {
				var authValidator NiFiClusterCustomValidator

				BeforeEach(func() {
					authValidator = NiFiClusterCustomValidator{Client: k8sClient}
					obj.Namespace = "default"
				})

				AfterEach(func() {
					_ = k8sClient.Delete(ctx, &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "nifi-login-full", Namespace: "default"},
					})
					_ = k8sClient.Delete(ctx, &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "nifi-login-missing-password", Namespace: "default"},
					})
				})

				It("should admit a secretName referencing a Secret with both keys populated", func() {
					secret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "nifi-login-full", Namespace: "default"},
						Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("SuperSecret123!")},
					}
					Expect(k8sClient.Create(ctx, secret)).To(Succeed())

					obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
						SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "nifi-login-full"},
					}
					_, err := authValidator.ValidateCreate(ctx, obj)
					Expect(err).NotTo(HaveOccurred())
				})

				It("should deny a secretName referencing a Secret missing the password key", func() {
					secret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "nifi-login-missing-password", Namespace: "default"},
						Data:       map[string][]byte{"username": []byte("admin")},
					}
					Expect(k8sClient.Create(ctx, secret)).To(Succeed())

					obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
						SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "nifi-login-missing-password"},
					}
					_, err := authValidator.ValidateCreate(ctx, obj)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("password"))
				})

				It("should admit a secretName referencing a Secret that does not exist yet", func() {
					obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
						SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "does-not-exist"},
					}
					warnings, err := authValidator.ValidateCreate(ctx, obj)
					Expect(err).NotTo(HaveOccurred())
					Expect(warnings).To(ContainElement(ContainSubstring(`"does-not-exist" not found`)))
				})

				It("should deny a secretName referencing a Secret whose password is too short", func() {
					secret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "nifi-login-short-password", Namespace: "default"},
						Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("short")},
					}
					Expect(k8sClient.Create(ctx, secret)).To(Succeed())
					DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

					obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
						SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "nifi-login-short-password"},
					}
					_, err := authValidator.ValidateCreate(ctx, obj)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("at least 12 characters"))
				})

				It("should deny a secretName referencing a Secret whose password is too long", func() {
					secret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "nifi-login-long-password", Namespace: "default"},
						Data:       map[string][]byte{"username": []byte("admin"), "password": []byte(strings.Repeat("a", 73))},
					}
					Expect(k8sClient.Create(ctx, secret)).To(Succeed())
					DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

					obj.Spec.Authentication = &nifiv1alpha1.AuthenticationSpec{
						SingleUser: &nifiv1alpha1.SingleUserAuthSpec{SecretName: "nifi-login-long-password"},
					}
					_, err := authValidator.ValidateCreate(ctx, obj)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("at most 72 bytes"))
				})
			})
		})
	})
})
