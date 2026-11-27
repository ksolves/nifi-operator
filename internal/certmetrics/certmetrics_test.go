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

package certmetrics

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func writeCert(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "webhook"},
		NotBefore: notAfter.Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	// A key block first, like a combined PEM, to check it is skipped.
	out := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")})
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherExportsExpiryAndFollowsRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tls.crt")
	first := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	writeCert(t, path, first)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error)
	go func() { done <- (&Watcher{Path: path, Interval: 20 * time.Millisecond}).Start(ctx) }()

	waitFor := func(want time.Time) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if testutil.ToFloat64(ExpiryGauge) == float64(want.Unix()) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("gauge = %v, want %v", testutil.ToFloat64(ExpiryGauge), want.Unix())
	}
	waitFor(first)

	// A renewed certificate (cert-manager or Helm re-issue) is picked up without a restart.
	second := first.Add(60 * 24 * time.Hour)
	writeCert(t, path, second)
	waitFor(second)

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNotAfterErrors(t *testing.T) {
	if _, err := NotAfter(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: no error")
	}
	empty := filepath.Join(t.TempDir(), "empty.crt")
	_ = os.WriteFile(empty, []byte("not pem"), 0o600)
	if _, err := NotAfter(empty); err == nil {
		t.Error("non-PEM file: no error")
	}
}

// SetCAExpiry/DeleteCAExpiry (NIFI-248 B3): one cluster's series doesn't leak into another's, and
// deleting a cluster removes its series rather than leaving it frozen at its last value.
func TestSetAndDeleteCAExpiry(t *testing.T) {
	want := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	SetCAExpiry("ns-a", "cluster-a", want)
	SetCAExpiry("ns-a", "cluster-b", want.Add(time.Hour))

	if got := testutil.ToFloat64(CAExpiryGauge.WithLabelValues("ns-a", "cluster-a")); got != float64(want.Unix()) {
		t.Errorf("cluster-a gauge = %v, want %v", got, want.Unix())
	}
	if got := testutil.ToFloat64(CAExpiryGauge.WithLabelValues("ns-a", "cluster-b")); got == float64(want.Unix()) {
		t.Error("cluster-b's series took on cluster-a's value; they must be independent")
	}

	DeleteCAExpiry("ns-a", "cluster-a")
	// Deleting the label combination makes it stop reporting rather than freezing its last value:
	// a fresh WithLabelValues call creates a new, zero-valued series, distinguishable from the one
	// just deleted only by it no longer being what SetCAExpiry last wrote.
	if got := testutil.ToFloat64(CAExpiryGauge.WithLabelValues("ns-a", "cluster-a")); got == float64(want.Unix()) {
		t.Error("cluster-a's series still reports its old value after DeleteCAExpiry")
	}
	DeleteCAExpiry("ns-a", "cluster-b")
}
