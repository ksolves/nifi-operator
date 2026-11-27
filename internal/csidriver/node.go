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
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/controller"
)

// Volume attribute keys set statically in the pod template by buildStatefulSet (see
// statefulset.go) - these identify which NiFiCluster/NodeGroup a mounting pod belongs
// to, since the CSI driver has no other way to know that (kubelet only ever tells it the pod's
// own name/namespace/UID, never which application object created the pod).
const (
	volumeAttrClusterName   = "clusterName"
	volumeAttrNamespace     = "namespace"
	volumeAttrNodeGroupName = "nodeGroupName"
)

var nodeLog = logf.Log.WithName("csi-node")

// NodeServer implements the CSI Node service. Only NodePublishVolume/NodeUnpublishVolume are
// needed - see the package doc for why (ephemeral inline volumes have no other lifecycle calls).
type NodeServer struct {
	csi.UnimplementedNodeServer

	// Client reads the CA Secret (internal/controller's reconcileTLSCA creates it) and the
	// NiFiCluster CR (for ExtraSANs/CertificateValidityDays) - a direct client is used
	// deliberately, not a cached/manager one: this driver runs as a bare gRPC server with no
	// manager/informer machinery, and each NodePublishVolume call is infrequent enough (once
	// per pod start, not once per reconcile loop) that an uncached read every time is the
	// right tradeoff, not a performance concern worth adding a cache for.
	Client client.Client

	// NodeID identifies the underlying Kubernetes node this plugin instance runs on, reported
	// via NodeGetInfo. Must come from the DaemonSet's own NODE_NAME downward-API env var
	// (spec.nodeName) - os.Hostname() inside a container returns the *pod's* hostname, not the
	// node's, which would make NodeGetInfo report a meaningless, pod-scoped identity instead of
	// the actual node identity kubelet/csinode expect.
	NodeID string
}

func (s *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	// No STAGE_UNSTAGE_VOLUME, no VOLUME_CONDITION, etc.: an ephemeral inline volume never
	// gets staged, and this driver reports no extra optional behavior.
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (s *NodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if s.NodeID == "" {
		return nil, fmt.Errorf("NodeGetInfo: NodeID is unset - the driver must be started with NodeID set from the NODE_NAME downward-API env var")
	}
	return &csi.NodeGetInfoResponse{NodeId: s.NodeID}, nil
}

// NodePublishVolume is called by kubelet once, when a pod mounting this driver's ephemeral
// volume starts. It resolves which pod is asking (from volume_context - see the constants
// above and the package doc), reads the NiFiCluster's CA Secret, mints a fresh leaf certificate
// for that specific pod, and writes it out as a PKCS12 keystore/truststore pair - PKCS12, not
// PEM, because NiFi 2.0 has no PEM KeyStore provider at all. The private key is written only to this pod's own ephemeral volume directory
// on the node's disk (not tmpfs - NodeUnpublishVolume deletes it when the pod goes away) - it is
// never stored as a Kubernetes Secret, unlike the CA itself.
func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, fmt.Errorf("NodePublishVolume: target_path is required")
	}

	attrs := req.GetVolumeContext()
	clusterName := attrs[volumeAttrClusterName]
	namespace := attrs[volumeAttrNamespace]
	nodeGroupName := attrs[volumeAttrNodeGroupName]
	// Injected by kubelet because the CSIDriver object sets podInfoOnMount: true (config/csi) -
	// not something this driver can derive any other way.
	podName := attrs["csi.storage.k8s.io/pod.name"]
	// Also kubelet-injected, and - unlike clusterName/namespace/nodeGroupName above - not
	// settable by whatever pod spec mounts this volume: it comes from the pod's own real
	// metadata, not from attacker-controlled volumeAttributes.
	podNamespace := attrs["csi.storage.k8s.io/pod.namespace"]

	if clusterName == "" || namespace == "" || nodeGroupName == "" || podName == "" {
		return nil, fmt.Errorf(
			"NodePublishVolume: missing required volume context (clusterName=%q namespace=%q nodeGroupName=%q pod.name=%q)",
			clusterName, namespace, nodeGroupName, podName)
	}

	// buildStatefulSet always sets volumeAttrNamespace to the pod's own real namespace (it's
	// templated from cluster.Namespace, the same namespace the StatefulSet itself creates the
	// pod in) - so for any legitimate operator-managed pod this always matches. Reject when it
	// doesn't: without this check, any pod on the cluster able to mount this driver's ephemeral
	// CSI volume at all (no extra RBAC gates that by default) could set volumeAttrNamespace to
	// an unrelated namespace and this driver would fetch that namespace's TLS CA Secret and
	// mint a valid mTLS cert for it - a cross-tenant impersonation, not just a cross-cluster one,
	// on a cluster where multiple unrelated clients share namespaces.
	if podNamespace == "" || podNamespace != namespace {
		return nil, fmt.Errorf(
			"NodePublishVolume: volume_context namespace %q does not match the mounting pod's actual namespace %q",
			namespace, podNamespace)
	}

	// Validates podName actually looks like a StatefulSet pod (fails loudly here rather than
	// producing a certificate for a malformed identity later) - the ordinal itself isn't
	// needed beyond this check, since PodFQDN below builds the FQDN from podName directly.
	if _, err := podOrdinal(podName); err != nil {
		return nil, fmt.Errorf("NodePublishVolume: %w", err)
	}

	cluster := &nifiv1alpha1.NiFiCluster{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: clusterName, Namespace: namespace}, cluster); err != nil {
		return nil, fmt.Errorf("NodePublishVolume: getting NiFiCluster %s/%s: %w", namespace, clusterName, err)
	}
	nodeGroup, err := s.findNodeGroup(ctx, cluster, nodeGroupName)
	if err != nil {
		return nil, fmt.Errorf("NodePublishVolume: %w", err)
	}
	if nodeGroup == nil {
		return nil, fmt.Errorf("NodePublishVolume: node group %q not found in NiFiCluster %s/%s or a NiFiNodeGroup referencing it",
			nodeGroupName, namespace, clusterName)
	}
	if nodeGroup.TLS == nil || !nodeGroup.TLS.Enabled {
		return nil, fmt.Errorf("NodePublishVolume: node group %q does not have TLS enabled", nodeGroupName)
	}

	if err := s.verifyPodOwnership(ctx, cluster, nodeGroup, podName, podNamespace, attrs["csi.storage.k8s.io/pod.uid"]); err != nil {
		return nil, fmt.Errorf("NodePublishVolume: %w", err)
	}

	caSecret := &corev1.Secret{}
	caKey := client.ObjectKey{Name: controller.TLSCASecretName(clusterName), Namespace: namespace}
	if err := s.Client.Get(ctx, caKey, caSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("NodePublishVolume: TLS CA Secret %s not found yet - the operator creates it on its next reconcile", caKey.Name)
		}
		return nil, fmt.Errorf("NodePublishVolume: getting TLS CA Secret %s: %w", caKey.Name, err)
	}
	// Only a CA the NiFiCluster controls is used to sign; a pre-planted Secret of this name is
	// refused.
	if !metav1.IsControlledBy(caSecret, cluster) {
		return nil, fmt.Errorf("NodePublishVolume: TLS CA Secret %s is not controlled by NiFiCluster %s/%s", caKey.Name, namespace, clusterName)
	}
	caCertPEM := caSecret.Data[corev1.TLSCertKey]
	caKeyPEM := caSecret.Data[corev1.TLSPrivateKeyKey]
	keystorePassword := string(caSecret.Data[controller.TLSKeystorePasswordDataKey])

	// The pod's own FQDN, the cluster Service names and every proxy host plus extraSANs - shared with the operator, which rolls the pods when these
	// change.
	sans := controller.CertificateSANs(cluster, nodeGroup, podName)

	validityDays := 365
	if nodeGroup.TLS.CertificateValidityDays != nil {
		validityDays = int(*nodeGroup.TLS.CertificateValidityDays)
	}

	leafCertPEM, leafKeyPEM, err := controller.GenerateLeafCert(
		caCertPEM, caKeyPEM, pkix.Name{CommonName: podName}, sans, validityDays)
	if err != nil {
		return nil, fmt.Errorf("NodePublishVolume: generating leaf certificate for pod %s: %w", podName, err)
	}

	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		return nil, fmt.Errorf("NodePublishVolume: creating target path %s: %w", targetPath, err)
	}
	if err := writeKeystoreAndTruststore(targetPath, leafCertPEM, leafKeyPEM, caCertPEM, keystorePassword); err != nil {
		// writeKeystoreAndTruststore can fail after already writing the keystore but before the
		// truststore - clean up rather than leave a stale/partial keystore behind for kubelet's
		// retry to find. Best-effort: a cleanup failure here must not mask the real error above.
		_ = os.RemoveAll(targetPath)
		return nil, fmt.Errorf("NodePublishVolume: %w", err)
	}

	nodeLog.Info("published volume", "volumeID", req.GetVolumeId(), "targetPath", targetPath, "pod", podName, "namespace", podNamespace)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume deletes everything NodePublishVolume created at target_path. The
// keystore/truststore are written straight into target_path on the node's own disk (there is no
// tmpfs mount behind it), so nothing else ever cleans them up: if this call left them in place,
// kubelet could not remove the now non-empty mount directory (the pod would be stuck
// terminating) and the pod's leaf private key would linger on the node indefinitely. The CSI
// spec also requires the SP to delete what it created at target_path, and to be idempotent -
// os.RemoveAll returns nil for a path that is already gone, so repeated calls still succeed.
// As a guard against deleting anything outside kubelet's own CSI volume layout
// (.../volumes/kubernetes.io~csi/<volume-name>/mount), the path must end in "mount" with a
// "kubernetes.io~csi" grandparent directory; any other path is refused and nothing is deleted.
func (s *NodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, fmt.Errorf("NodeUnpublishVolume: target_path is required")
	}

	clean := filepath.Clean(targetPath)
	if filepath.Base(clean) != "mount" || filepath.Base(filepath.Dir(filepath.Dir(clean))) != "kubernetes.io~csi" {
		return nil, fmt.Errorf(
			"NodeUnpublishVolume: target path %s does not match the kubelet CSI volume layout (.../kubernetes.io~csi/<volume-name>/mount) - refusing to delete it",
			clean)
	}

	if err := os.RemoveAll(clean); err != nil {
		return nil, fmt.Errorf("NodeUnpublishVolume: removing target path %s: %w", clean, err)
	}

	nodeLog.Info("unpublished volume, removed target path", "volumeID", req.GetVolumeId(), "targetPath", clean)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// writeKeystoreAndTruststore encodes the leaf cert/key as a PKCS12 keystore (including the CA in
// its chain, so NiFi can serve the full chain to peers) and the CA cert alone as a PKCS12
// truststore, writing both into dir under controller.TLSKeystoreFileName/controller.TLSTruststoreFileName - the exact form
// verified against a real NiFi.
func writeKeystoreAndTruststore(dir string, leafCertPEM, leafKeyPEM, caCertPEM []byte, keystorePassword string) error {
	leafCert, err := parseCertPEM(leafCertPEM)
	if err != nil {
		return fmt.Errorf("parsing leaf certificate: %w", err)
	}
	leafKey, err := parseRSAKeyPEM(leafKeyPEM)
	if err != nil {
		return fmt.Errorf("parsing leaf private key: %w", err)
	}
	caCert, err := parseCertPEM(caCertPEM)
	if err != nil {
		return fmt.Errorf("parsing CA certificate: %w", err)
	}

	// pkcs12.Legacy, not the deprecated package-level Encode/EncodeTrustStore: same encoding
	// behavior (this package's own docs say so), just via the explicit encoder the library now
	// wants callers to name - not pkcs12.Modern, since that changes the actual bytes produced
	// (AES instead of RC2/3DES), and only the classic/legacy-style PKCS12 file was verified
	// against NiFi's JVM, not this newer encoding.
	keystoreData, err := pkcs12.Legacy.Encode(leafKey, leafCert, []*x509.Certificate{caCert}, keystorePassword)
	if err != nil {
		return fmt.Errorf("encoding PKCS12 keystore: %w", err)
	}
	// 0640, not 0600: NiFi runs with an arbitrary UID but GID 0 on OpenShift, so the file is
	// readable through the group, not the owner.
	if err := os.WriteFile(filepath.Join(dir, controller.TLSKeystoreFileName), keystoreData, 0o640); err != nil { //nolint:gosec // G306: see above
		return fmt.Errorf("writing %s: %w", controller.TLSKeystoreFileName, err)
	}

	truststoreData, err := pkcs12.Legacy.EncodeTrustStore([]*x509.Certificate{caCert}, keystorePassword)
	if err != nil {
		return fmt.Errorf("encoding PKCS12 truststore: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, controller.TLSTruststoreFileName), truststoreData, 0o640); err != nil { //nolint:gosec // G306: group-readable, as the keystore
		return fmt.Errorf("writing %s: %w", controller.TLSTruststoreFileName, err)
	}

	return nil
}

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseRSAKeyPEM(keyPEM []byte) (any, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// podOrdinal extracts the StatefulSet ordinal suffix from a pod name of the form
// "<statefulSetName>-<ordinal>" (matching statefulSetName's own naming pattern).
func podOrdinal(podName string) (int, error) {
	idx := strings.LastIndex(podName, "-")
	if idx == -1 || idx == len(podName)-1 {
		return 0, fmt.Errorf("pod name %q does not end in a StatefulSet ordinal suffix", podName)
	}
	ordinal, err := strconv.Atoi(podName[idx+1:])
	if err != nil {
		return 0, fmt.Errorf("parsing ordinal from pod name %q: %w", podName, err)
	}
	return ordinal, nil
}

// verifyPodOwnership refuses to issue a certificate unless the mounting pod really is one of the
// pods the operator's StatefulSet for this node group created. clusterName/nodeGroupName come
// from volume attributes that any pod spec can set, and the namespace check in
// NodePublishVolume only proves the pod runs in the cluster's namespace - without this, any pod
// in that namespace named like "<anything>-<n>" could claim to be a NiFi node and get a leaf
// certificate signed by the cluster CA. The chain checked, all matched on UID rather than name
// (a name alone survives delete/recreate, a UID does not):
//
//   - the pod kubelet is mounting for (kubelet-injected pod.uid) is the pod the API server has
//     under that name right now
//   - that pod's controller is the node group's StatefulSet, by name and UID
//   - that StatefulSet's controller is this NiFiCluster, by UID
//   - the pod's name is "<statefulSetName>-<ordinal>" with ordinal inside spec.replicas
//
// Residual risk: ownerReferences are writable by whoever creates a pod, so a principal that can
// create pods in the cluster's namespace can still forge this chain while the real pod for an
// in-range ordinal is absent (e.g. mid-restart). Closing that needs admission control on who may
// create pods controlled by the StatefulSet - out of scope for the driver itself.
func (s *NodeServer) verifyPodOwnership(
	ctx context.Context,
	cluster *nifiv1alpha1.NiFiCluster,
	nodeGroup *nifiv1alpha1.NodeGroup,
	podName, podNamespace, podUID string,
) error {
	if podUID == "" {
		return fmt.Errorf("missing kubelet-injected pod.uid in volume context - the CSIDriver object must set podInfoOnMount: true")
	}

	stsName := controller.StatefulSetName(cluster.Name, nodeGroup.Name)
	sts := &appsv1.StatefulSet{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: stsName, Namespace: cluster.Namespace}, sts); err != nil {
		return fmt.Errorf("getting StatefulSet %s/%s: %w", cluster.Namespace, stsName, err)
	}
	if ref := metav1.GetControllerOf(sts); ref == nil || ref.UID != cluster.UID {
		return fmt.Errorf("StatefulSet %s/%s is not controlled by NiFiCluster %s - refusing to issue a certificate",
			cluster.Namespace, stsName, cluster.Name)
	}

	suffix, found := strings.CutPrefix(podName, stsName+"-")
	if !found {
		return fmt.Errorf("pod %s is not named after StatefulSet %s - refusing to issue a certificate", podName, stsName)
	}
	ordinal, err := strconv.Atoi(suffix)
	if err != nil || ordinal < 0 || strconv.Itoa(ordinal) != suffix {
		return fmt.Errorf("pod %s does not have a valid ordinal suffix for StatefulSet %s", podName, stsName)
	}
	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	if int64(ordinal) >= int64(replicas) {
		return fmt.Errorf("pod %s ordinal %d is outside StatefulSet %s replicas (%d) - refusing to issue a certificate",
			podName, ordinal, stsName, replicas)
	}
	// The webhook and the CRD keep a node group within MaxClusterNodes, but /scale (kubectl
	// scale, an autoscaler) bypasses both: scaling a StatefulSet straight past the limit still
	// gets every pod up to its own (over-limit) replicas a certificate without this check
	// (NIFI-248 B1). A node beyond MaxClusterNodes never gets one, so it cannot start as a TLS
	// cluster member - the limit still holds for TLS clusters even when the StatefulSet itself
	// was pushed over it. A non-TLS cluster has no CSI step to block here at all; per the
	// ticket, the node limit is a product boundary, not a security control, so that gap is
	// accepted, not closed by this check.
	if int64(ordinal) >= int64(nifiv1alpha1.MaxClusterNodes) {
		return fmt.Errorf("pod %s ordinal %d is at or past the %d-node limit on this edition - refusing to issue a certificate",
			podName, ordinal, nifiv1alpha1.MaxClusterNodes)
	}

	pod := &corev1.Pod{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: podName, Namespace: podNamespace}, pod); err != nil {
		return fmt.Errorf("getting pod %s/%s: %w", podNamespace, podName, err)
	}
	if string(pod.UID) != podUID {
		return fmt.Errorf("pod %s/%s UID %s does not match the mounting pod's UID %s", podNamespace, podName, pod.UID, podUID)
	}
	ref := metav1.GetControllerOf(pod)
	if ref == nil || ref.Kind != "StatefulSet" || ref.Name != stsName || ref.UID != sts.UID {
		return fmt.Errorf("pod %s/%s is not controlled by StatefulSet %s - refusing to issue a certificate",
			podNamespace, podName, stsName)
	}
	return nil
}

// findNodeGroup returns the node group named name of cluster, or nil if it has none: an inline
// nodeGroups entry first, else a NiFiNodeGroup object of that name whose clusterRef is cluster.
// Same rules as the controller's mergeNodeGroups, so the driver only issues certificates for node
// groups the operator actually runs: an inline group wins a name conflict, and a NiFiNodeGroup
// being deleted is left out.
func (s *NodeServer) findNodeGroup(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, name string) (*nifiv1alpha1.NodeGroup, error) {
	for i := range cluster.Spec.NodeGroups {
		if cluster.Spec.NodeGroups[i].Name == name {
			return &cluster.Spec.NodeGroups[i], nil
		}
	}

	obj := &nifiv1alpha1.NiFiNodeGroup{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: cluster.Namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting NiFiNodeGroup %s/%s: %w", cluster.Namespace, name, err)
	}
	if obj.Spec.ClusterRef != cluster.Name || !obj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	ng := obj.Spec.NodeGroup.DeepCopy()
	ng.Name = obj.Name
	return ng, nil
}
