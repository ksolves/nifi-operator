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
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/certmetrics"
)

// tlsCAValidityYears is the self-signed root CA's validity period - long-lived and, like the
// sensitive-props key, essentially never rotated in the ordinary course of operation (see
// reconcileTLSCA).
const tlsCAValidityYears = 5

// TLSKeystorePasswordDataKey is the key, in the cluster's TLS CA Secret (tlsCASecretName), holding
// the PKCS12 keystore/truststore password used by both the NiFi pods (keystorePasswordEnvVar) and the
// CSI driver (internal/csidriver, which actually encodes the PKCS12 files) - stored alongside the
// CA so both read it from the one Secret they already fetch, and so the value is generated once,
// per cluster, rather than a single word shared by every cluster everywhere (the previous
// "changeit" constant this field replaces). Read once, when the CA Secret is first created, and
// never rotated afterward - the same create-once rule as the CA itself, since changing the
// password after pods already have keystores encrypted with the old one would lock them out until
// every pod's certificate is reissued.
//
// This password only protects the on-disk PKCS12 encoding, not the key material itself (the
// private key is exactly as sensitive with or without a password on the container around it);
// NiFi simply requires *some* non-empty password to open a PKCS12 keystore. Generating one
// per-cluster rather than reusing a fixed value is still worth doing - it closes off the
// (admittedly narrow) risk of that fixed value being relied on anywhere a per-cluster secret
// should have been - and nodeGroup.TLS.KeystorePasswordSecretName lets an operator supply their
// own instead, for organizations whose own tooling needs to own every password in the system.
const TLSKeystorePasswordDataKey = "keystore-password"

// Keystore/truststore file names the CSI driver (internal/csidriver) writes into the CSI-mounted
// volume - defined here, not in internal/csidriver, so this package's tlsProperties (below) can
// reference them without an import cycle (internal/csidriver already imports this package for
// GenerateLeafCert/PodFQDN/TLSCASecretName). The exact form was verified against a real NiFi 2.0
// instance.
const (
	TLSKeystoreFileName   = "keystore.p12"
	TLSTruststoreFileName = "truststore.p12"
)

// tlsRSAKeyBits matches the key size used by NiFi's own toolkit-generated keystores and by
// Stackable's secret-operator - 2048 bits is the common baseline for RSA TLS certificates and
// keeps generation fast, which matters here since leaf certs are minted on every pod start.
const tlsRSAKeyBits = 2048

// GenerateCA creates a new self-signed root CA certificate and private key, both PEM-encoded.
// Called once per NiFiCluster by reconcileTLSCA (create-once, like sensitivePropsSecretName) -
// never regenerated afterward, since replacing an already-trusted CA would break every node's
// trust chain until every leaf certificate it signed is also reissued.
//
// validityYears is currently always tlsCAValidityYears (the one production caller); kept as a
// parameter, not a hardcoded constant inside this function, so unit tests can exercise other
// validity periods without duplicating the certificate-building logic.
func GenerateCA(validityYears int) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, tlsRSAKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generating CA private key: %w", err)
	}

	serial, err := randomSerialNumber()
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "nifi-operator self-signed CA"},
		NotBefore:             time.Now().Add(-5 * time.Minute), // clock-skew tolerance
		NotAfter:              time.Now().AddDate(validityYears, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		// The CA only ever signs node certificates, never further CAs.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating CA certificate: %w", err)
	}

	return encodeCertPEM(der), encodeKeyPEM(key), nil
}

// GenerateLeafCert creates a new certificate signed by the given CA, PEM-encoded, with the
// given SANs and an EKU covering both server and client auth - required for mutual TLS, since
// each NiFi node is a TLS server (REST API, incoming cluster-protocol connections) and a TLS
// client (outgoing cluster-protocol connections to its peers) at once.
//
// caCertPEM/caKeyPEM are the CA's own PEM-encoded cert/key, as produced by GenerateCA and read
// back from the CA Secret. Called both by the operator's own reconciler (never - the CA is
// create-once, it never issues leaf certs itself) and by the CSI driver's NodePublishVolume,
// which calls this fresh on every pod mount - see internal/csidriver.
//
// keyPEM's production consumer is the CSI driver's NodePublishVolume (internal/csidriver/node.go),
// which writes it into the pod's own keystore alongside certPEM.
func GenerateLeafCert(caCertPEM, caKeyPEM []byte, subject pkix.Name, sans []string, validityDays int) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := decodeCAPEM(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}

	key, err := rsa.GenerateKey(rand.Reader, tlsRSAKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generating leaf private key: %w", err)
	}

	serial, err := randomSerialNumber()
	if err != nil {
		return nil, nil, err
	}

	notAfter := time.Now().AddDate(0, 0, validityDays)
	// A leaf can never outlive the CA that signs it (NIFI-248 B3): past caCert.NotAfter, no
	// client will accept it regardless of its own dates, so handing out a leaf that claims to be
	// valid past that point is simply wrong, not just optimistic.
	if notAfter.After(caCert.NotAfter) {
		notAfter = caCert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating leaf certificate: %w", err)
	}

	return encodeCertPEM(der), encodeKeyPEM(key), nil
}

// decodeCAPEM parses a CA's PEM-encoded certificate and RSA private key back into their Go
// types, as read from the CA Secret.
func decodeCAPEM(certPEM, keyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("decoding CA certificate PEM: no PEM block found")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("decoding CA private key PEM: no PEM block found")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA private key: %w", err)
	}

	return cert, key, nil
}

// CertExpiringSoon reports whether a PEM-encoded certificate has passed the given fraction of
// its total validity lifetime (e.g. 0.5 for "half its lifetime has elapsed"), mirroring the
// proactive-renewal pattern Stackable's secret-operator uses instead of waiting for a
// certificate to actually expire.
func CertExpiringSoon(certPEM []byte, lifetimeFractionThreshold float64) (bool, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false, fmt.Errorf("decoding certificate PEM: no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, fmt.Errorf("parsing certificate: %w", err)
	}

	totalLifetime := cert.NotAfter.Sub(cert.NotBefore)
	elapsed := time.Since(cert.NotBefore)
	return float64(elapsed) >= float64(totalLifetime)*lifetimeFractionThreshold, nil
}

// randomSerialNumber returns a cryptographically random certificate serial number, sized per
// CA/Browser Forum guidance (at least 64 bits of entropy, kept under the 20-octet ceiling most
// TLS implementations expect).
func randomSerialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating certificate serial number: %w", err)
	}
	return serial, nil
}

func encodeCertPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKeyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// tlsProperties returns the nifi.properties entries controlling web/cluster-protocol TLS for a
// node group - the unsecured defaults when TLS is disabled/unset (byte-for-byte the same values
// the original, unconditional block in buildNifiProperties always set), or the PKCS12
// configuration verified against a real NiFi when enabled.
//
// Mutual TLS for the cluster protocol needs no separate property: configuring
// nifi.security.keystore and nifi.security.truststore is what enables it - every node is
// simultaneously a TLS server (for incoming peer connections) and a TLS client presenting its own
// cert (for outgoing ones), which is what makes it mutual. The former
// nifi.cluster.protocol.is.secure property never controlled this and was removed in NiFi 2.7.0
// (NIFI-15140), so it is deliberately not set.
func tlsProperties(nodeGroup *nifiv1alpha1.NodeGroup) map[string]string {
	if nodeGroup.TLS == nil || !nodeGroup.TLS.Enabled {
		return map[string]string{
			nifiPropWebHTTPHost: "0.0.0.0",
			nifiPropWebHTTPPort: fmt.Sprintf("%d", nifiHTTPPort),
			// The image's own default has an HTTPS port configured too; NiFi refuses to start
			// with both an HTTP and an HTTPS port set, so this must be explicitly blanked to
			// get the unsecured HTTP-only listener this phase calls for.
			nifiPropWebHTTPSPort: "",
			// Defaults to true, which then requires nifiPropWebHTTPSPort to be set (which we
			// just blanked above) for remote input site-to-site - unused and out of scope for
			// an unsecured node group anyway.
			nifiPropRemoteInputSecure: "false",
			// The image's defaults point these at a keystore/truststore that's only generated
			// by the base image's own start.sh (which we don't run) - blank them so nothing
			// tries to load a TLS keystore at all when this node group is unsecured.
			nifiPropSecurityKeystore:   "",
			nifiPropSecurityTruststore: "",
		}
	}

	keystorePath := confTLSMountPath + "/" + TLSKeystoreFileName
	truststorePath := confTLSMountPath + "/" + TLSTruststoreFileName
	return map[string]string{
		nifiPropWebHTTPHost:  "",
		nifiPropWebHTTPPort:  "",
		nifiPropWebHTTPSHost: "0.0.0.0",
		nifiPropWebHTTPSPort: fmt.Sprintf("%d", nifiHTTPSPort),
		// Both must be explicitly disabled together: with nifiPropWebHTTPPort blank,
		// nifiPropRemoteInputHTTPEnabled defaulting to true makes NiFi refuse to start
		// ("Remote input HTTP is enabled but nifi.web.http.port is not specified") - confirmed
		// against a real NiFi. Site-to-site remains out of scope for this feature either way.
		nifiPropRemoteInputSecure:      "false",
		nifiPropRemoteInputHTTPEnabled: "false",
		nifiPropSecurityKeystore:       keystorePath,
		nifiPropSecurityKeystoreType:   "PKCS12",
		nifiPropSecurityTruststore:     truststorePath,
		nifiPropSecurityTruststoreType: "PKCS12",
		// nifiPropSecurityKeystorePasswd and nifiPropSecurityTruststorePasswd come from the
		// NIFI_KEYSTORE_PASSWORD env var (a secretKeyRef, see keystorePasswordEnvVar), which the
		// entrypoint applies: the password never goes into this ConfigMap.
	}
}

// tlsRenewalFractionThreshold mirrors Stackable's own secret-operator: proactively rotate once a
// pod has run for roughly half of its certificate's intended validity, rather than waiting until
// the certificate is actually close to expiring.
const tlsRenewalFractionThreshold = 0.5

// tlsCAWarningFractionThreshold is higher than tlsRenewalFractionThreshold (NIFI-248 B3): the CA
// never rotates on its own the way a leaf certificate does, so the warning needs enough lead time
// for someone to plan and run the manual replacement (docs/tls.md#rotation) before expiry, not
// just a nudge that something will self-heal shortly. 0.8 of the 5-year lifetime leaves about a
// year.
const tlsCAWarningFractionThreshold = 0.8

// caExpiringSoonConditionType is True once the cluster's TLS CA has passed
// tlsCAWarningFractionThreshold of its lifetime: unlike a leaf certificate, it is never rotated
// automatically, so this is the only signal (besides the matching certmetrics.CAExpiryGauge) that
// someone needs to plan the manual replacement in docs/tls.md#rotation before the CA actually
// expires and takes every certificate it signed down with it.
const caExpiringSoonConditionType = "CAExpiringSoon"

// reconcileTLSCAExpiry sets caExpiringSoonConditionType and certmetrics.CAExpiryGauge from the
// cluster's existing CA Secret (reconcileTLSCA has already created it by this point). Read back
// from the Secret, not computed from tlsCAValidityYears: a cluster can be running the operator
// after an upgrade that changed that constant, or on a CA some old client created with a
// different value, so the Secret's own NotAfter is the only reliable source.
func (r *NiFiClusterReconciler) reconcileTLSCAExpiry(ctx context.Context, cluster *nifiv1alpha1.NiFiCluster) error {
	ca := &corev1.Secret{}
	key := client.ObjectKey{Name: tlsCASecretName(cluster), Namespace: cluster.Namespace}
	if err := r.Get(ctx, key, ca); err != nil {
		if apierrors.IsNotFound(err) {
			// reconcileTLSCA, just above, may have created this Secret moments ago in this same
			// reconcile: the cached client's informer can legitimately not have observed it yet.
			// The next reconcile (the watch event for the create itself) picks this back up.
			return nil
		}
		return fmt.Errorf("getting TLS CA Secret for its expiry: %w", err)
	}
	block, _ := pem.Decode(ca.Data[corev1.TLSCertKey])
	if block == nil {
		return fmt.Errorf("decoding TLS CA certificate PEM: no PEM block found")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parsing TLS CA certificate: %w", err)
	}

	certmetrics.SetCAExpiry(cluster.Namespace, cluster.Name, caCert.NotAfter)

	expiringSoon, err := CertExpiringSoon(ca.Data[corev1.TLSCertKey], tlsCAWarningFractionThreshold)
	if err != nil {
		return fmt.Errorf("checking TLS CA expiry: %w", err)
	}
	status, reason, message := metav1.ConditionFalse, "CANotExpiringSoon", "The cluster's TLS CA is not close to expiring."
	if expiringSoon {
		status, reason = metav1.ConditionTrue, "CAExpiringSoon"
		message = fmt.Sprintf("The cluster's TLS CA expires %s. It is never rotated automatically; "+
			"see docs/tls.md#rotation to replace it manually before then.", caCert.NotAfter.Format(time.RFC3339))
	}
	_, err = r.setCondition(ctx, cluster, caExpiringSoonConditionType, status, reason, message)
	return err
}

// reconcileTLSRotation deletes at most one pod per reconcile once it's run past
// tlsRenewalFractionThreshold of TLS.CertificateValidityDays, so the StatefulSet controller
// replaces it and the CSI driver (internal/csidriver) mints it a fresh certificate on the way
// back up - see the CSI driver's NodePublishVolume for why a fresh mount always gets a fresh cert.
//
// No certificate is ever read back here to check its actual expiry: the certificate only ever
// exists inside a pod's own ephemeral, tmpfs-backed mount (see internal/csidriver) and is never
// persisted as a Kubernetes object the reconciler could read. Pod age (Status.StartTime) is used
// as a reliable proxy instead - a pod that has been running since before the renewal threshold
// necessarily has a certificate at least that old too, since a fresh one is only ever minted at
// mount time (pod start).
//
// Ready-gate: if any pod in this node group is not currently Ready, no further rotation is
// triggered this reconcile - a stuck or failing CSI mount on a just-rotated pod must halt further
// rotations instead of the reconciler cascading into deleting every pod in the group at once.
// Uses the Eviction API (not a plain Delete), so an in-progress voluntary disruption still
// respects the node group's own PodDisruptionBudget exactly as any other eviction would.
func (r *NiFiClusterReconciler) reconcileTLSRotation(
	ctx context.Context, cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup,
) error {
	if nodeGroup.TLS == nil || !nodeGroup.TLS.Enabled {
		return nil
	}

	validityDays := 365
	if nodeGroup.TLS.CertificateValidityDays != nil {
		validityDays = int(*nodeGroup.TLS.CertificateValidityDays)
	}
	renewalThreshold := time.Duration(float64(validityDays) * 24 * float64(time.Hour) * tlsRenewalFractionThreshold)

	pods := &corev1.PodList{}
	selector := client.MatchingLabels{
		labelAppInstance: cluster.Name,
		labelNodeGroup:   nodeGroup.Name,
	}
	// APIReader, not the cached r.Client: the whole point of the Ready-gate below is to check
	// what's true *right now*, immediately after a previous reconcile's own eviction - the
	// cached client can briefly still show an already-evicted pod as present and Ready
	// (informer propagation lag), which would silently defeat the gate and let a second pod get
	// evicted before the first's removal is even reflected. Same reasoning as
	// reconcileStatefulSet's own APIReader use - see NiFiClusterReconciler's field comment.
	if err := r.APIReader.List(ctx, pods, client.InNamespace(cluster.Namespace), selector); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	// Fewer pods than desired means a previous eviction's replacement hasn't even been created
	// yet - a real, common transient state (the gap between a pod being deleted and the
	// StatefulSet controller creating its replacement), not just "a replacement exists but isn't
	// Ready yet" below. Both cases must gate rotation the same way.
	if len(pods.Items) < int(nodeGroup.Replicas) {
		return nil
	}
	for i := range pods.Items {
		if !podReady(&pods.Items[i]) {
			return nil
		}
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.StartTime == nil {
			continue
		}
		if time.Since(pod.Status.StartTime.Time) < renewalThreshold {
			continue
		}

		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
		if err := r.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
			return fmt.Errorf("evicting pod %s for certificate rotation: %w", pod.Name, err)
		}
		r.event(cluster, corev1.EventTypeNormal, reasonCertRotated, fmt.Sprintf(
			"evicted pod %s to rotate its TLS certificate, due for renewal after %s", pod.Name, renewalThreshold))
		// Exactly one pod per reconcile: the Ready-gate above is what actually prevents a
		// cascade, but stopping here too means a single reconcile never tries to evict more
		// than one pod even before the replacement has had any chance to become un-Ready.
		return nil
	}
	return nil
}

// podReady reports whether pod's PodReady condition is currently True.
func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// buildTLSCASecret renders a Secret holding a freshly generated self-signed CA for cluster.
// Callers must only use this to *create* the Secret, never to overwrite an existing one - see
// reconcileTLSCA for why. Named and shaped (type kubernetes.io/tls, keys tls.crt/tls.key)
// following the wider Kubernetes ecosystem's convention for TLS material Secrets (cert-manager,
// MinIO Operator, and others all converge on this three-key shape); since this CA is
// self-signed, its own cert doubles as its trust anchor, so a separate ca.crt key isn't needed.
// keystorePassword is the per-cluster PKCS12 password to store alongside the CA (see
// TLSKeystorePasswordDataKey) - the caller resolves it first, either reading the user-supplied
// nodeGroup.TLS.KeystorePasswordSecretName or generating one, since this function only builds the
// Secret and must stay side-effect-free like the rest of this file's build* functions.
func buildTLSCASecret(cluster *nifiv1alpha1.NiFiCluster, keystorePassword string) (*corev1.Secret, error) {
	certPEM, keyPEM, err := GenerateCA(tlsCAValidityYears)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tlsCASecretName(cluster),
			Namespace: cluster.Namespace,
			// Cluster-scoped, not per-node-group, so this uses only the labels every
			// generated object shares (see commonLabels) rather than a node-group-specific
			// label set - there's no single node group this Secret belongs to.
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:          certPEM,
			corev1.TLSPrivateKeyKey:    keyPEM,
			TLSKeystorePasswordDataKey: []byte(keystorePassword),
		},
	}, nil
}
