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
	"encoding/pem"
	"math/big"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestGenerateCA(t *testing.T) {
	certPEM, keyPEM, err := GenerateCA(5)
	if err != nil {
		t.Fatalf("GenerateCA() error = %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("GenerateCA() cert is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing generated CA certificate: %v", err)
	}

	if !cert.IsCA {
		t.Error("GenerateCA() cert IsCA = false, want true")
	}
	// the CA may sign leaf certificates only, never another CA.
	if cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		t.Errorf("GenerateCA() MaxPathLen = %d, MaxPathLenZero = %v, want 0/true", cert.MaxPathLen, cert.MaxPathLenZero)
	}
	if !cert.BasicConstraintsValid {
		t.Error("GenerateCA() cert BasicConstraintsValid = false, want true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("GenerateCA() cert missing KeyUsageCertSign")
	}

	// A CA must be able to verify (sign) itself, since GenerateLeafCert reparses caCertPEM/
	// caKeyPEM exactly as read back from a Secret.
	if _, _, err := decodeCAPEM(certPEM, keyPEM); err != nil {
		t.Errorf("decodeCAPEM() on GenerateCA()'s own output failed: %v", err)
	}
}

func TestGenerateLeafCert(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA(5)
	if err != nil {
		t.Fatalf("GenerateCA() error = %v", err)
	}

	sans := []string{"regression-test-default-0.regression-test-default-headless.default.svc.cluster.local", "extra-san.example.com"}
	subject := pkix.Name{CommonName: "regression-test-default-0"}
	leafCertPEM, _, err := GenerateLeafCert(caCertPEM, caKeyPEM, subject, sans, 30)
	if err != nil {
		t.Fatalf("GenerateLeafCert() error = %v", err)
	}

	leafBlock, _ := pem.Decode(leafCertPEM)
	if leafBlock == nil {
		t.Fatal("GenerateLeafCert() cert is not valid PEM")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing generated leaf certificate: %v", err)
	}

	caBlock, _ := pem.Decode(caCertPEM)
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing CA certificate: %v", err)
	}

	// Acceptance: x509.Certificate.Verify against the CA passes.
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Errorf("leaf.Verify() against its CA failed: %v", err)
	}

	// Acceptance: SAN list on the leaf matches input exactly.
	gotSANs := append([]string{}, leaf.DNSNames...)
	wantSANs := append([]string{}, sans...)
	sort.Strings(gotSANs)
	sort.Strings(wantSANs)
	if !reflect.DeepEqual(gotSANs, wantSANs) {
		t.Errorf("leaf DNSNames = %v, want %v", gotSANs, wantSANs)
	}

	// Acceptance: EKU set correctly - both server and client auth (required for mTLS).
	hasServerAuth, hasClientAuth := false, false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasServerAuth || !hasClientAuth {
		t.Errorf("leaf ExtKeyUsage = %v, want both ServerAuth and ClientAuth", leaf.ExtKeyUsage)
	}

	if leaf.Subject.CommonName != "regression-test-default-0" {
		t.Errorf("leaf Subject.CommonName = %q, want %q", leaf.Subject.CommonName, "regression-test-default-0")
	}
}

// A leaf must never outlive its own CA (NIFI-248 B3): past the CA's NotAfter, no client accepts
// it regardless of its own dates.
func TestGenerateLeafCertCapsValidityAtCAExpiry(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, tlsRSAKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	caNotAfter := time.Now().Add(10 * 24 * time.Hour) // the CA itself expires in 10 days.
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              caNotAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCertPEM := encodeCertPEM(caDER)
	caKeyPEM := encodeKeyPEM(key)
	// X.509 time fields have no sub-second precision, so what actually comes back out of the
	// certificate differs slightly from caTemplate.NotAfter above - compare against this, the
	// same value GenerateLeafCert itself reads.
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caNotAfter = caCert.NotAfter

	// validityDays asks for a year, far past the CA's own 10-day expiry.
	leafCertPEM, _, err := GenerateLeafCert(caCertPEM, caKeyPEM, pkix.Name{CommonName: "pod-0"}, nil, 365)
	if err != nil {
		t.Fatalf("GenerateLeafCert() error = %v", err)
	}
	leafBlock, _ := pem.Decode(leafCertPEM)
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	if leaf.NotAfter.After(caNotAfter) {
		t.Errorf("leaf.NotAfter = %v, which is after the CA's own NotAfter %v", leaf.NotAfter, caNotAfter)
	}
	if !leaf.NotAfter.Equal(caNotAfter) {
		t.Errorf("leaf.NotAfter = %v, want it capped to exactly the CA's NotAfter %v", leaf.NotAfter, caNotAfter)
	}

	// Verification against the CA fails once the CA itself has expired - confirming the capped
	// leaf is genuinely unusable past that point, not just conservatively dated.
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	afterExpiry := caNotAfter.Add(24 * time.Hour)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: afterExpiry}); err == nil {
		t.Error("leaf.Verify() after the CA expired succeeded, want an error")
	}
}

func TestCertExpiringSoon(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA(5)
	if err != nil {
		t.Fatalf("GenerateCA() error = %v", err)
	}

	tests := []struct {
		name         string
		validityDays int
		threshold    float64
		want         bool
	}{
		{"freshly issued, well under threshold", 365, 0.5, false},
		{"1-day cert, 50% threshold - not yet due (just issued)", 1, 0.5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			certPEM, _, err := GenerateLeafCert(caCertPEM, caKeyPEM, pkix.Name{CommonName: "test"}, nil, tt.validityDays)
			if err != nil {
				t.Fatalf("GenerateLeafCert() error = %v", err)
			}
			got, err := CertExpiringSoon(certPEM, tt.threshold)
			if err != nil {
				t.Fatalf("CertExpiringSoon() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("CertExpiringSoon() = %v, want %v", got, tt.want)
			}
		})
	}

	// A cert already past its threshold (built with a NotBefore far enough in the past that
	// half its lifetime has already elapsed) must report true. GenerateLeafCert always backdates
	// NotBefore by 5 minutes for clock skew, so construct this case directly rather than through
	// GenerateLeafCert to control elapsed time precisely.
	ca, key, err := decodeCAPEM(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("decodeCAPEM() error = %v", err)
	}
	past := time.Now().Add(-20 * time.Hour)
	template := &x509.Certificate{
		SerialNumber: ca.SerialNumber,
		Subject:      pkix.Name{CommonName: "expiring-soon"},
		NotBefore:    past,
		NotAfter:     past.Add(24 * time.Hour), // 1-day validity, issued 20h ago -> 83% elapsed
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating backdated test certificate: %v", err)
	}
	backdatedPEM := encodeCertPEM(der)
	got, err := CertExpiringSoon(backdatedPEM, 0.5)
	if err != nil {
		t.Fatalf("CertExpiringSoon() error = %v", err)
	}
	if !got {
		t.Error("CertExpiringSoon() on a cert 83% through its lifetime = false, want true")
	}
}
