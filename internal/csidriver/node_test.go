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

package csidriver

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/controller"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := scheme.Scheme
	if err := nifiv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding nifiv1alpha1 to scheme: %v", err)
	}
	return s
}

func TestNodePublishVolume(t *testing.T) {
	validity := int32(365)
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test", Namespace: "default"},
		Spec: nifiv1alpha1.NiFiClusterSpec{
			Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
			NodeGroups: []nifiv1alpha1.NodeGroup{
				{
					Name: "default", Replicas: 3, Storage: "1Gi",
					TLS: &nifiv1alpha1.TLSSpec{
						Enabled:                 true,
						ExtraSANs:               []string{"extra.example.com"},
						CertificateValidityDays: &validity,
					},
				},
			},
			ProxyHosts: []string{"proxy.example.com:9443"},
		},
	}

	caCertPEM, caKeyPEM, err := controller.GenerateCA(5)
	if err != nil {
		t.Fatalf("controller.GenerateCA() error = %v", err)
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test-ca-certificate", Namespace: "default", OwnerReferences: []metav1.OwnerReference{clusterControllerRef("regression-test")}},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       caCertPEM,
			corev1.TLSPrivateKeyKey: caKeyPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append(ownedPodFixtures(cluster), cluster, caSecret)...).
		Build()

	server := &NodeServer{Client: fakeClient}

	targetPath := t.TempDir()
	req := &csi.NodePublishVolumeRequest{
		TargetPath: targetPath,
		VolumeContext: map[string]string{
			volumeAttrClusterName:              "regression-test",
			volumeAttrNamespace:                "default",
			volumeAttrNodeGroupName:            "default",
			"csi.storage.k8s.io/pod.name":      "regression-test-default-0",
			"csi.storage.k8s.io/pod.namespace": "default",
			"csi.storage.k8s.io/pod.uid":       testPodUID,
		},
	}

	if _, err := server.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume() error = %v", err)
	}

	keystorePath := filepath.Join(targetPath, controller.TLSKeystoreFileName)
	truststorePath := filepath.Join(targetPath, controller.TLSTruststoreFileName)

	keystoreData, err := os.ReadFile(keystorePath)
	if err != nil {
		t.Fatalf("reading %s: %v", keystorePath, err)
	}
	// DecodeChain, not Decode: the keystore holds three bags (leaf cert, leaf key, and the CA
	// cert included for chain-serving - see writeKeystoreAndTruststore), and plain Decode only
	// supports the simpler two-bag (cert+key) case.
	_, leafCert, _, err := pkcs12.DecodeChain(keystoreData, "")
	if err != nil {
		t.Fatalf("decoding keystore: %v", err)
	}

	// Acceptance: correct FQDN derivation - the leaf's SAN matches this specific pod's FQDN.
	wantFQDN := "regression-test-default-0.regression-test-default-headless.default.svc.cluster.local"
	if !contains(leafCert.DNSNames, wantFQDN) {
		t.Errorf("leaf cert DNSNames = %v, want to contain %q", leafCert.DNSNames, wantFQDN)
	}
	if !contains(leafCert.DNSNames, "extra.example.com") {
		t.Errorf("leaf cert DNSNames = %v, want to contain ExtraSANs entry %q", leafCert.DNSNames, "extra.example.com")
	}
	// Every hostname clients use (NiFi 2 rejects a TLS request whose SNI is not on its
	// certificate) and the cluster Service names reencrypting routers verify against.
	for _, want := range []string{
		"proxy.example.com",
		"regression-test.default.svc", "regression-test.default.svc.cluster.local",
	} {
		if !contains(leafCert.DNSNames, want) {
			t.Errorf("leaf cert DNSNames = %v, want to contain %q", leafCert.DNSNames, want)
		}
	}
	if leafCert.Subject.CommonName != "regression-test-default-0" {
		t.Errorf("leaf cert CommonName = %q, want %q", leafCert.Subject.CommonName, "regression-test-default-0")
	}

	// Acceptance: correct file contents - the truststore holds the CA, and the leaf verifies
	// against it.
	truststoreData, err := os.ReadFile(truststorePath)
	if err != nil {
		t.Fatalf("reading %s: %v", truststorePath, err)
	}
	trustedCerts, err := pkcs12.DecodeTrustStore(truststoreData, "")
	if err != nil {
		t.Fatalf("decoding truststore: %v", err)
	}
	if len(trustedCerts) != 1 {
		t.Fatalf("truststore has %d certs, want 1", len(trustedCerts))
	}
	roots := x509.NewCertPool()
	roots.AddCert(trustedCerts[0])
	if _, err := leafCert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Errorf("leaf cert does not verify against truststore CA: %v", err)
	}
}

// TestNodePublishVolume_MissingCA is the acceptance case for "correct error surfaced when the CA
// Secret doesn't exist yet" - the NiFiCluster exists, but reconcileTLSCA hasn't created its CA
// Secret yet (a real, expected race: NodePublishVolume can be called before the operator's next
// reconcile).
func TestNodePublishVolume_MissingCA(t *testing.T) {
	validity := int32(365)
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test", Namespace: "default"},
		Spec: nifiv1alpha1.NiFiClusterSpec{
			Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
			NodeGroups: []nifiv1alpha1.NodeGroup{
				{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: &validity}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(append(ownedPodFixtures(cluster), cluster)...).Build()
	server := &NodeServer{Client: fakeClient}

	req := &csi.NodePublishVolumeRequest{
		TargetPath: t.TempDir(),
		VolumeContext: map[string]string{
			volumeAttrClusterName:              "regression-test",
			volumeAttrNamespace:                "default",
			volumeAttrNodeGroupName:            "default",
			"csi.storage.k8s.io/pod.name":      "regression-test-default-0",
			"csi.storage.k8s.io/pod.namespace": "default",
			"csi.storage.k8s.io/pod.uid":       testPodUID,
		},
	}

	_, err := server.NodePublishVolume(context.Background(), req)
	if err == nil {
		t.Fatal("NodePublishVolume() with no CA Secret = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "TLS CA Secret") {
		t.Errorf("NodePublishVolume() error = %q, want it to mention the missing TLS CA Secret", err.Error())
	}
}

// TestNodePublishVolume_NamespaceMismatch is the acceptance case for the cross-namespace
// impersonation fix: volumeAttrNamespace is attacker-controlled (any pod spec can set it), but
// the kubelet-injected pod.namespace is not - a mismatch means the mounting pod is claiming to
// belong to a NiFiCluster in a namespace it doesn't actually run in, and must be rejected before
// ever reaching the CA Secret lookup.
func TestNodePublishVolume_NamespaceMismatch(t *testing.T) {
	validity := int32(365)
	victim := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-cluster", Namespace: "victim-ns"},
		Spec: nifiv1alpha1.NiFiClusterSpec{
			Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
			NodeGroups: []nifiv1alpha1.NodeGroup{
				{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: &validity}},
			},
		},
	}
	caCertPEM, caKeyPEM, err := controller.GenerateCA(5)
	if err != nil {
		t.Fatalf("controller.GenerateCA() error = %v", err)
	}
	victimCASecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-cluster-ca-certificate", Namespace: "victim-ns"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       caCertPEM,
			corev1.TLSPrivateKeyKey: caKeyPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(victim, victimCASecret).Build()
	server := &NodeServer{Client: fakeClient}

	req := &csi.NodePublishVolumeRequest{
		TargetPath: t.TempDir(),
		VolumeContext: map[string]string{
			// Attacker-controlled: claims the victim's cluster/namespace...
			volumeAttrClusterName:         "victim-cluster",
			volumeAttrNamespace:           "victim-ns",
			volumeAttrNodeGroupName:       "default",
			"csi.storage.k8s.io/pod.name": "attacker-pod-0",
			// ...but kubelet says this pod actually runs in "attacker-ns".
			"csi.storage.k8s.io/pod.namespace": "attacker-ns",
		},
	}

	_, err = server.NodePublishVolume(context.Background(), req)
	if err == nil {
		t.Fatal("NodePublishVolume() with mismatched namespace = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "does not match the mounting pod's actual namespace") {
		t.Errorf("NodePublishVolume() error = %q, want it to mention the namespace mismatch", err.Error())
	}
}

// TestNodePublishVolume_CleansUpOnPartialWriteFailure covers a gap found alongside the
// NodeUnpublishVolume fix above: writeKeystoreAndTruststore writes the keystore file before the
// truststore file, so a failure on the second write must not leave the first file's stale
// contents behind for kubelet's retry (or NodeUnpublishVolume) to find.
func TestNodePublishVolume_CleansUpOnPartialWriteFailure(t *testing.T) {
	validity := int32(365)
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test", Namespace: "default"},
		Spec: nifiv1alpha1.NiFiClusterSpec{
			Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
			NodeGroups: []nifiv1alpha1.NodeGroup{
				{
					Name: "default", Replicas: 1, Storage: "1Gi",
					TLS: &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: &validity},
				},
			},
		},
	}

	caCertPEM, caKeyPEM, err := controller.GenerateCA(5)
	if err != nil {
		t.Fatalf("controller.GenerateCA() error = %v", err)
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test-ca-certificate", Namespace: "default", OwnerReferences: []metav1.OwnerReference{clusterControllerRef("regression-test")}},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       caCertPEM,
			corev1.TLSPrivateKeyKey: caKeyPEM,
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(append(ownedPodFixtures(cluster), cluster, caSecret)...).Build()
	server := &NodeServer{Client: fakeClient}

	targetPath := t.TempDir()
	// Force the truststore write to fail after the keystore write has already succeeded: a
	// directory at that path makes os.WriteFile fail with "is a directory", reproducing the
	// exact partial-write ordering without needing to fake disk I/O.
	if err := os.MkdirAll(filepath.Join(targetPath, controller.TLSTruststoreFileName), 0o750); err != nil {
		t.Fatalf("pre-creating truststore path as a directory: %v", err)
	}

	req := &csi.NodePublishVolumeRequest{
		TargetPath: targetPath,
		VolumeContext: map[string]string{
			volumeAttrClusterName:              "regression-test",
			volumeAttrNamespace:                "default",
			volumeAttrNodeGroupName:            "default",
			"csi.storage.k8s.io/pod.name":      "regression-test-default-0",
			"csi.storage.k8s.io/pod.namespace": "default",
			"csi.storage.k8s.io/pod.uid":       testPodUID,
		},
	}

	if _, err := server.NodePublishVolume(context.Background(), req); err == nil {
		t.Fatal("NodePublishVolume() = nil error, want the forced truststore write failure to surface")
	}

	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Errorf("targetPath %s still exists after a partial-write failure, stat err = %v - stale keystore left behind", targetPath, err)
	}
}

const (
	testPodUID     = "pod-uid-0"
	testClusterUID = "cluster-uid"
	testStsUID     = "sts-uid"
)

// ownedPodFixtures builds the objects verifyPodOwnership needs for ordinal 0 of the cluster's
// first node group ("<cluster>-<nodeGroup>-0") to be a legitimate operator-managed pod: a StatefulSet named after that node
// group and controlled by the cluster (by UID), plus the pod itself controlled by that
// StatefulSet (by UID). It also sets the cluster's UID, so callers must pass the cluster to
// WithObjects after calling this.
func ownedPodFixtures(cluster *nifiv1alpha1.NiFiCluster) []client.Object {
	cluster.UID = testClusterUID
	nodeGroup := cluster.Spec.NodeGroups[0]
	replicas := nodeGroup.Replicas
	stsName := controller.StatefulSetName(cluster.Name, nodeGroup.Name)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: stsName, Namespace: cluster.Namespace, UID: testStsUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: nifiv1alpha1.GroupVersion.String(), Kind: "NiFiCluster",
				Name: cluster.Name, UID: cluster.UID, Controller: ptrTo(true),
			}},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: &replicas},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: stsName + "-0", Namespace: cluster.Namespace, UID: testPodUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "StatefulSet",
				Name: stsName, UID: sts.UID, Controller: ptrTo(true),
			}},
		},
	}
	return []client.Object{sts, pod}
}

func ptrTo[T any](v T) *T { return &v }

// NIFI-248 B1(a): the webhook and the CRD keep a node group within MaxClusterNodes, but /scale
// (kubectl scale, an autoscaler) bypasses both, since the CRD only validates the NiFiCluster/
// NiFiNodeGroup object, never the StatefulSet itself. Before this check, a StatefulSet scaled
// straight to 5 with kubectl scale still got every one of its 5 pods a working certificate - the
// acceptance case for that regression is below. This check caps the ordinal at MaxClusterNodes
// regardless of the StatefulSet's own declared replicas, so a node past the limit can never start
// as a TLS cluster member, even once the StatefulSet itself was pushed over it. It does not help a
// TLS-off cluster (no CSI step runs at all there); per the ticket, the node limit is a product
// boundary, not a security control, so that residual gap is accepted, not closed by this check.
func TestNodePublishVolume_RefusesOrdinalPastClusterLimit(t *testing.T) {
	validity := int32(365)
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test", Namespace: "default", UID: testClusterUID},
		Spec: nifiv1alpha1.NiFiClusterSpec{
			Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
			NodeGroups: []nifiv1alpha1.NodeGroup{
				// The CRD-enforced limit: at most 3 replicas. The StatefulSet below is scaled past
				// it directly, as the CRD cannot see or constrain the StatefulSet itself.
				{Name: "default", Replicas: 3, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: &validity}},
			},
		},
	}
	stsName := controller.StatefulSetName(cluster.Name, cluster.Spec.NodeGroups[0].Name)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: stsName, Namespace: cluster.Namespace, UID: testStsUID,
			OwnerReferences: []metav1.OwnerReference{clusterControllerRef(cluster.Name)},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: ptrTo(int32(5))},
	}
	caCertPEM, caKeyPEM, err := controller.GenerateCA(5)
	if err != nil {
		t.Fatalf("controller.GenerateCA() error = %v", err)
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test-ca-certificate", Namespace: "default", OwnerReferences: []metav1.OwnerReference{clusterControllerRef("regression-test")}},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: caCertPEM, corev1.TLSPrivateKeyKey: caKeyPEM},
	}

	issueFor := func(ordinal int) error {
		podName := fmt.Sprintf("%s-%d", stsName, ordinal)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: podName, Namespace: cluster.Namespace, UID: testPodUID,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "StatefulSet", Name: stsName, UID: sts.UID, Controller: ptrTo(true),
				}},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sts, pod, cluster, caSecret).Build()
		server := &NodeServer{Client: fakeClient}
		req := &csi.NodePublishVolumeRequest{
			TargetPath: t.TempDir(),
			VolumeContext: map[string]string{
				volumeAttrClusterName:              cluster.Name,
				volumeAttrNamespace:                cluster.Namespace,
				volumeAttrNodeGroupName:            cluster.Spec.NodeGroups[0].Name,
				"csi.storage.k8s.io/pod.name":      podName,
				"csi.storage.k8s.io/pod.namespace": cluster.Namespace,
				"csi.storage.k8s.io/pod.uid":       testPodUID,
			},
		}
		_, err := server.NodePublishVolume(context.Background(), req)
		return err
	}

	for ordinal := 0; ordinal < 3; ordinal++ {
		if err := issueFor(ordinal); err != nil {
			t.Errorf("NodePublishVolume() for ordinal %d (within MaxClusterNodes) = %v, want nil", ordinal, err)
		}
	}
	for ordinal := 3; ordinal < 5; ordinal++ {
		if err := issueFor(ordinal); err == nil || !strings.Contains(err.Error(), "node limit") {
			t.Errorf("NodePublishVolume() for ordinal %d (at/past MaxClusterNodes) = %v, want an error mentioning the node limit", ordinal, err)
		}
	}
}

// TestNodePublishVolume_RejectsUnownedPod is the acceptance case for owner checks: the namespace
// check alone let any pod in the cluster's namespace with an ordinal-style name get a leaf
// certificate signed by the cluster CA. Each case breaks exactly one link of the ownership chain
// verifyPodOwnership checks, and must be refused before any keystore is written.
func TestNodePublishVolume_RejectsUnownedPod(t *testing.T) {
	const podName = "regression-test-default-0"
	tests := []struct {
		name    string
		podName string
		podUID  string
		mutate  func(sts *appsv1.StatefulSet, pod *corev1.Pod)
		dropPod bool
		wantErr string
	}{
		{name: "missing pod.uid", podName: podName, podUID: "", wantErr: "missing kubelet-injected pod.uid"},
		{name: "name not derived from StatefulSet", podName: "attacker-pod-0", podUID: testPodUID, wantErr: "is not named after StatefulSet"},
		{name: "ordinal outside replicas", podName: "regression-test-default-3", podUID: testPodUID, wantErr: "outside StatefulSet"},
		{name: "non-canonical ordinal", podName: "regression-test-default-01", podUID: testPodUID, wantErr: "valid ordinal suffix"},
		{name: "pod does not exist", podName: podName, podUID: testPodUID, dropPod: true, wantErr: "getting pod"},
		{name: "pod UID mismatch (recreated pod)", podName: podName, podUID: "some-other-uid", wantErr: "does not match the mounting pod's UID"},
		{
			name: "pod has no controller", podName: podName, podUID: testPodUID,
			mutate:  func(_ *appsv1.StatefulSet, pod *corev1.Pod) { pod.OwnerReferences = nil },
			wantErr: "is not controlled by StatefulSet",
		},
		{
			name: "pod controlled by a different object with the same name", podName: podName, podUID: testPodUID,
			mutate:  func(_ *appsv1.StatefulSet, pod *corev1.Pod) { pod.OwnerReferences[0].UID = "impostor-uid" },
			wantErr: "is not controlled by StatefulSet",
		},
		{
			name: "pod controlled by a non-StatefulSet kind", podName: podName, podUID: testPodUID,
			mutate:  func(_ *appsv1.StatefulSet, pod *corev1.Pod) { pod.OwnerReferences[0].Kind = "ReplicaSet" },
			wantErr: "is not controlled by StatefulSet",
		},
		{
			name: "StatefulSet not controlled by the NiFiCluster", podName: podName, podUID: testPodUID,
			mutate: func(sts *appsv1.StatefulSet, _ *corev1.Pod) {
				sts.OwnerReferences[0].UID = types.UID("other-cluster-uid")
			},
			wantErr: "is not controlled by NiFiCluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validity := int32(365)
			cluster := &nifiv1alpha1.NiFiCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "regression-test", Namespace: "default"},
				Spec: nifiv1alpha1.NiFiClusterSpec{
					Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
					NodeGroups: []nifiv1alpha1.NodeGroup{
						{Name: "default", Replicas: 3, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true, CertificateValidityDays: &validity}},
					},
				},
			}
			caCertPEM, caKeyPEM, err := controller.GenerateCA(5)
			if err != nil {
				t.Fatalf("controller.GenerateCA() error = %v", err)
			}
			caSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "regression-test-ca-certificate", Namespace: "default", OwnerReferences: []metav1.OwnerReference{clusterControllerRef("regression-test")}},
				Type:       corev1.SecretTypeTLS,
				Data:       map[string][]byte{corev1.TLSCertKey: caCertPEM, corev1.TLSPrivateKeyKey: caKeyPEM},
			}

			fixtures := ownedPodFixtures(cluster)
			sts, pod := fixtures[0].(*appsv1.StatefulSet), fixtures[1].(*corev1.Pod)
			if tt.mutate != nil {
				tt.mutate(sts, pod)
			}
			objs := []client.Object{cluster, caSecret, sts}
			if !tt.dropPod {
				objs = append(objs, pod)
			}
			server := &NodeServer{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()}

			targetPath := filepath.Join(t.TempDir(), "mount")
			req := &csi.NodePublishVolumeRequest{
				TargetPath: targetPath,
				VolumeContext: map[string]string{
					volumeAttrClusterName:              "regression-test",
					volumeAttrNamespace:                "default",
					volumeAttrNodeGroupName:            "default",
					"csi.storage.k8s.io/pod.name":      tt.podName,
					"csi.storage.k8s.io/pod.namespace": "default",
					"csi.storage.k8s.io/pod.uid":       tt.podUID,
				},
			}

			_, err = server.NodePublishVolume(context.Background(), req)
			if err == nil {
				t.Fatal("NodePublishVolume() = nil error, want the unowned pod to be refused")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NodePublishVolume() error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
			if _, statErr := os.Stat(targetPath); !os.IsNotExist(statErr) {
				t.Errorf("targetPath %s exists after a refused publish (stat err = %v) - a keystore may have been written", targetPath, statErr)
			}
		})
	}
}

// TestNodePublishVolume_NiFiNodeGroup covers node groups defined as NiFiNodeGroup objects: the
// driver must resolve them the way the controller's mergeNodeGroups does, not only from the
// cluster's inline nodeGroups.
func TestNodePublishVolume_NiFiNodeGroup(t *testing.T) {
	tests := []struct {
		name        string
		group       string
		clusterRef  string
		deleting    bool
		tlsDisabled bool
		forbidGet   bool
		wantErr     string
	}{
		{name: "references the cluster", group: "ingest", clusterRef: "nng-test"},
		{name: "references another cluster", group: "ingest", clusterRef: "other-cluster", wantErr: "not found"},
		{name: "being deleted", group: "ingest", clusterRef: "nng-test", deleting: true, wantErr: "not found"},
		{name: "inline group of the same name wins", group: "default", clusterRef: "nng-test", wantErr: "does not have TLS enabled"},
		{name: "TLS disabled", group: "ingest", clusterRef: "nng-test", tlsDisabled: true, wantErr: "does not have TLS enabled"},
		{name: "Get forbidden", group: "ingest", clusterRef: "nng-test", forbidGet: true, wantErr: "forbidden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validity := int32(365)
			cluster := &nifiv1alpha1.NiFiCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "nng-test", Namespace: "default", UID: testClusterUID},
				Spec: nifiv1alpha1.NiFiClusterSpec{
					Image: nifiv1alpha1.ImageSpec{Repository: "apache/nifi", Tag: "2.0.0"},
					NodeGroups: []nifiv1alpha1.NodeGroup{
						{Name: "default", Replicas: 1, Storage: "1Gi"},
					},
				},
			}
			nng := &nifiv1alpha1.NiFiNodeGroup{
				ObjectMeta: metav1.ObjectMeta{Name: tt.group, Namespace: "default"},
				Spec: nifiv1alpha1.NiFiNodeGroupSpec{
					ClusterRef: tt.clusterRef,
					NodeGroup: nifiv1alpha1.NodeGroup{
						Name: tt.group, Replicas: 1, Storage: "1Gi",
						TLS: &nifiv1alpha1.TLSSpec{Enabled: !tt.tlsDisabled, ExtraSANs: []string{"ingest.example.test"}, CertificateValidityDays: &validity},
					},
				},
			}
			if tt.deleting {
				now := metav1.Now()
				nng.DeletionTimestamp = &now
				nng.Finalizers = []string{"test.nifi.ksolves.com/hold"}
			}

			caCertPEM, caKeyPEM, err := controller.GenerateCA(5)
			if err != nil {
				t.Fatalf("controller.GenerateCA() error = %v", err)
			}
			caSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "nng-test-ca-certificate", Namespace: "default", OwnerReferences: []metav1.OwnerReference{clusterControllerRef("nng-test")}},
				Type:       corev1.SecretTypeTLS,
				Data:       map[string][]byte{corev1.TLSCertKey: caCertPEM, corev1.TLSPrivateKeyKey: caKeyPEM},
			}

			// The StatefulSet and pod of the requested group, controlled by the NiFiCluster like
			// the operator creates them for a NiFiNodeGroup.
			fixturesCluster := cluster.DeepCopy()
			fixturesCluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: tt.group, Replicas: 1}}
			objs := append(ownedPodFixtures(fixturesCluster), cluster, nng, caSecret)
			builder := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...)
			if tt.forbidGet {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*nifiv1alpha1.NiFiNodeGroup); ok {
						return apierrors.NewForbidden(schema.GroupResource{Group: "nifi.ksolves.com", Resource: "nifinodegroups"}, key.Name, errors.New("rbac"))
					}
					return c.Get(ctx, key, obj, opts...)
				}})
			}
			server := &NodeServer{Client: builder.Build()}

			targetPath := filepath.Join(t.TempDir(), "mount")
			podName := controller.StatefulSetName("nng-test", tt.group) + "-0"
			req := &csi.NodePublishVolumeRequest{
				TargetPath: targetPath,
				VolumeContext: map[string]string{
					volumeAttrClusterName:              "nng-test",
					volumeAttrNamespace:                "default",
					volumeAttrNodeGroupName:            tt.group,
					"csi.storage.k8s.io/pod.name":      podName,
					"csi.storage.k8s.io/pod.namespace": "default",
					"csi.storage.k8s.io/pod.uid":       testPodUID,
				},
			}

			_, err = server.NodePublishVolume(context.Background(), req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("NodePublishVolume() error = %v", err)
				}
				keystoreData, err := os.ReadFile(filepath.Join(targetPath, controller.TLSKeystoreFileName))
				if err != nil {
					t.Fatalf("reading keystore: %v", err)
				}
				_, leafCert, _, err := pkcs12.DecodeChain(keystoreData, "")
				if err != nil {
					t.Fatalf("decoding keystore: %v", err)
				}
				if leafCert.Subject.CommonName != podName {
					t.Errorf("leaf cert CommonName = %q, want %q", leafCert.Subject.CommonName, podName)
				}
				if !contains(leafCert.DNSNames, "ingest.example.test") {
					t.Errorf("leaf cert DNSNames = %v, want to contain ExtraSANs entry %q", leafCert.DNSNames, "ingest.example.test")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NodePublishVolume() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if _, statErr := os.Stat(targetPath); !os.IsNotExist(statErr) {
				t.Errorf("targetPath %s exists after a refused publish (stat err = %v) - a keystore may have been written", targetPath, statErr)
			}
		})
	}
}

func TestNodeUnpublishVolume_RemovesFiles(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "pods", "uid", "volumes", "kubernetes.io~csi", "tls", "mount")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("creating %s: %v", target, err)
	}
	for _, name := range []string{controller.TLSKeystoreFileName, controller.TLSTruststoreFileName} {
		if err := os.WriteFile(filepath.Join(target, name), []byte("data"), 0o640); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	server := &NodeServer{}
	req := &csi.NodeUnpublishVolumeRequest{VolumeId: "v1", TargetPath: target}
	if _, err := server.NodeUnpublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeUnpublishVolume() error = %v", err)
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) error = %v, want not-exist", target, err)
	}
	parent := filepath.Dir(target)
	if _, err := os.Stat(parent); err != nil {
		t.Errorf("os.Stat(%s) error = %v, want parent dir to still exist", parent, err)
	}
}

func TestNodeUnpublishVolume_Idempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "pods", "uid", "volumes", "kubernetes.io~csi", "tls", "mount")

	server := &NodeServer{}
	req := &csi.NodeUnpublishVolumeRequest{VolumeId: "v1", TargetPath: target}
	if _, err := server.NodeUnpublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeUnpublishVolume() on missing path error = %v, want nil", err)
	}
}

func TestNodeUnpublishVolume_RejectsUnexpectedPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "not-a-csi-dir")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("creating %s: %v", target, err)
	}
	file := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(file, []byte("data"), 0o640); err != nil {
		t.Fatalf("writing %s: %v", file, err)
	}

	server := &NodeServer{}
	req := &csi.NodeUnpublishVolumeRequest{VolumeId: "v1", TargetPath: target}
	if _, err := server.NodeUnpublishVolume(context.Background(), req); err == nil {
		t.Fatal("NodeUnpublishVolume() on unexpected path = nil error, want an error")
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("os.Stat(%s) error = %v, want file to still exist", file, err)
	}
}

func TestNodeUnpublishVolume_EmptyPath(t *testing.T) {
	server := &NodeServer{}
	if _, err := server.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "v1"}); err == nil {
		t.Fatal("NodeUnpublishVolume() with empty target path = nil error, want an error")
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// clusterControllerRef is the controller reference the operator puts on the cluster's CA Secret;
// the driver refuses a CA without it.
func clusterControllerRef(clusterName string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: nifiv1alpha1.GroupVersion.String(), Kind: "NiFiCluster",
		Name: clusterName, UID: testClusterUID, Controller: ptrTo(true),
	}
}

// a pre-planted CA Secret the NiFiCluster doesn't control is never used to sign.
func TestNodePublishVolumeRefusesUnownedCA(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test", Namespace: "default"},
		Spec: nifiv1alpha1.NiFiClusterSpec{NodeGroups: []nifiv1alpha1.NodeGroup{
			{Name: "default", Replicas: 1, Storage: "1Gi", TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
		}},
	}
	caCertPEM, caKeyPEM, err := controller.GenerateCA(5)
	if err != nil {
		t.Fatal(err)
	}
	planted := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "regression-test-ca-certificate", Namespace: "default"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: caCertPEM, corev1.TLSPrivateKeyKey: caKeyPEM},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(append(ownedPodFixtures(cluster), cluster, planted)...).Build()
	targetPath := t.TempDir()
	_, err = (&NodeServer{Client: c}).NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		TargetPath: targetPath,
		VolumeContext: map[string]string{
			volumeAttrClusterName:              "regression-test",
			volumeAttrNamespace:                "default",
			volumeAttrNodeGroupName:            "default",
			"csi.storage.k8s.io/pod.name":      "regression-test-default-0",
			"csi.storage.k8s.io/pod.namespace": "default",
			"csi.storage.k8s.io/pod.uid":       testPodUID,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "is not controlled by NiFiCluster") {
		t.Fatalf("NodePublishVolume() err = %v, want refusal of the unowned CA", err)
	}
	if _, statErr := os.Stat(filepath.Join(targetPath, controller.TLSKeystoreFileName)); statErr == nil {
		t.Error("a keystore was written with the planted CA")
	}
}
