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

// Package certmetrics exports the expiry time of the webhook serving certificate as a Prometheus
// gauge, so an alert fires before an expired certificate makes the API server reject every
// NiFiCluster change (the webhook's failurePolicy is Fail).
package certmetrics

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ExpiryGauge is nifi_operator_webhook_certificate_expiry_timestamp_seconds: the NotAfter of the
// serving certificate the webhook server currently uses, as a Unix timestamp.
var ExpiryGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "nifi_operator_webhook_certificate_expiry_timestamp_seconds",
	Help: "Expiry (NotAfter) of the admission webhook's serving certificate, in Unix seconds.",
})

// CAExpiryGauge is nifi_operator_cluster_ca_expiry_timestamp_seconds: the NotAfter of each
// NiFiCluster's own TLS CA, as a Unix timestamp, labeled by namespace/cluster since every cluster
// has its own CA and none of them rotate on their own (NIFI-248 B3) - an alert on this is the
// only way to learn a CA needs manually replacing before it actually expires and takes every
// node certificate it signed down with it.
var CAExpiryGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "nifi_operator_cluster_ca_expiry_timestamp_seconds",
	Help: "Expiry (NotAfter) of a NiFiCluster's TLS CA certificate, in Unix seconds.",
}, []string{"namespace", "cluster"})

// SetCAExpiry records a NiFiCluster's CA expiry in CAExpiryGauge.
func SetCAExpiry(namespace, cluster string, notAfter time.Time) {
	CAExpiryGauge.WithLabelValues(namespace, cluster).Set(float64(notAfter.Unix()))
}

// DeleteCAExpiry removes a deleted NiFiCluster's series from CAExpiryGauge, so it does not report
// a frozen, increasingly stale expiry time for a CA that no longer exists.
func DeleteCAExpiry(namespace, cluster string) {
	CAExpiryGauge.DeleteLabelValues(namespace, cluster)
}

// Watcher refreshes ExpiryGauge from a PEM certificate file. It is a manager Runnable; it runs on
// every replica, since every replica serves the webhook.
type Watcher struct {
	Path     string
	Interval time.Duration
}

// NeedLeaderElection reports false: every replica serves the webhook with its own mounted copy.
func (w *Watcher) NeedLeaderElection() bool { return false }

// Start refreshes the gauge every Interval until ctx ends. A read failure is logged and retried;
// the gauge keeps its last value.
func (w *Watcher) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("certmetrics")
	interval := w.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if notAfter, err := NotAfter(w.Path); err != nil {
			logger.Error(err, "reading the webhook serving certificate", "path", w.Path)
		} else {
			ExpiryGauge.Set(float64(notAfter.Unix()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NotAfter returns the expiry of the first certificate in the PEM file at path.
func NotAfter(path string) (time.Time, error) {
	// path is the operator's own --webhook-cert-path flag, not user input.
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return time.Time{}, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return time.Time{}, errors.New("no certificate in " + path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing %s: %w", path, err)
		}
		return cert.NotAfter, nil
	}
}
