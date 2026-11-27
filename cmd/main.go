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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/certmetrics"
	"github.com/ksolves/nifi-operator/internal/controller"
	webhookv1alpha1 "github.com/ksolves/nifi-operator/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(nifiv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var leaderElectionID string
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var nifiSCC string
	var csiDriverServiceAccount string
	var leaseDuration, renewDeadline, retryPeriod time.Duration
	var nifiStatusInterval time.Duration
	var offloadThroughput int64
	var allowCustomSABinding bool
	var trustedImageRepositories string
	var defaultNiFiImage string
	var nifiFSGroup, nifiRunAsUser, nifiSeccompProfile string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "9ffb69f3.ksolves.com",
		"The lease name used for leader election. Must be unique per manager Deployment sharing a "+
			"namespace - a second, differently-named instance in the same namespace needs its own value "+
			"here, or its leader election will collide with this one's.")
	// Lease timings. The defaults are controller-runtime's own: a crashed leader is
	// replaced after at most leaseDuration; a leader that stops normally (rolling update, drain)
	// releases the lease at once (LeaderElectionReleaseOnCancel below).
	flag.DurationVar(&leaseDuration, "leader-election-lease-duration", 15*time.Second,
		"How long a non-leader waits before taking over a lease the leader stopped renewing.")
	flag.DurationVar(&renewDeadline, "leader-election-renew-deadline", 10*time.Second,
		"How long the leader keeps retrying to renew its lease before giving up leadership.")
	flag.DurationVar(&retryPeriod, "leader-election-retry-period", 2*time.Second,
		"How often leader-election clients retry acquiring or renewing the lease.")
	flag.DurationVar(&nifiStatusInterval, "nifi-status-interval", controller.DefaultNiFiStatusInterval,
		"How often each NiFiCluster's NiFi REST API is polled for status (nodes, coordinator, version).")
	flag.Int64Var(&offloadThroughput, "offload-throughput-bytes-per-second",
		controller.DefaultOffloadThroughputBytesPerSecond,
		"Offload rate assumed when warning that a scale-down step may not offload a node within its "+
			"terminationGracePeriodSeconds.")
	flag.BoolVar(&allowCustomSABinding, "allow-custom-service-account-binding", false,
		"Allow NiFiClusters that set coordination.bindPodServiceAccounts to have the Kubernetes coordination Role "+
			"bound to their node groups' own ServiceAccounts.")
	flag.StringVar(&trustedImageRepositories, "trusted-image-repositories", os.Getenv("TRUSTED_IMAGE_REPOSITORIES"),
		"Comma-separated image repositories a NiFiCluster may use without its editor being allowed to create pods "+
			"in the namespace; an entry ending in \"/\" trusts every repository under it. Empty means every new image "+
			"repository needs that permission.")
	flag.StringVar(&nifiFSGroup, "nifi-fs-group", envOr("NIFI_FS_GROUP", "0"),
		"fsGroup of the NiFi pods. Empty sets none, for platforms that assign one themselves "+
			"and reject a fixed value (e.g. OpenShift's restricted-v2 SCC).")
	flag.StringVar(&nifiRunAsUser, "nifi-run-as-user", envOr("NIFI_RUN_AS_USER", "1000"),
		"UID of the NiFi pods, with group 0. Empty sets neither, for platforms that assign the UID "+
			"themselves (e.g. OpenShift's restricted-v2 SCC). Stock apache/nifi images need one otherwise: "+
			"their user is a name, which runAsNonRoot cannot verify.")
	flag.StringVar(&nifiSeccompProfile, "nifi-seccomp-profile", envOr("NIFI_SECCOMP_PROFILE", "RuntimeDefault"),
		"Seccomp profile of the NiFi pods: RuntimeDefault, or empty for none, for policies that forbid "+
			"setting one (e.g. OpenShift's anyuid SCC). A NiFiCluster's own security contexts still apply.")
	flag.StringVar(&defaultNiFiImage, "default-nifi-image", os.Getenv("DEFAULT_NIFI_IMAGE"),
		"Node image (repository:tag) for a NiFiCluster without spec.image. Empty means "+controller.BuiltinNiFiImage+".")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&nifiSCC, "nifi-scc", os.Getenv("NIFI_SCC"),
		"OpenShift SecurityContextConstraints to bind the NiFi pods' ServiceAccount to (e.g. anyuid). "+
			"Ignored on clusters without the security.openshift.io API. The operator must hold \"use\" on it.")
	flag.StringVar(&csiDriverServiceAccount, "csi-driver-service-account", os.Getenv("CSI_DRIVER_SERVICE_ACCOUNT"),
		"The TLS CSI driver's ServiceAccount as <namespace>/<name>. For every TLS cluster the operator grants it "+
			"get on that cluster's CA Secret only. Empty disables the grant.")
	// Production logging by default (JSON, no stack traces on warnings); --zap-devel switches
	flag.StringVar(&controller.ClusterDomain, "cluster-domain", envOr("CLUSTER_DOMAIN", "cluster.local"),
		"The Kubernetes cluster's DNS domain, used in NiFi pod FQDNs and certificate SANs. "+
			"The operator and the CSI driver must agree.")
	// to development output.
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	fsGroup, runAsUser, err := nifiPodFlags(nifiFSGroup, nifiRunAsUser, defaultNiFiImage, nifiSeccompProfile)
	if err != nil {
		setupLog.Error(err, "invalid NiFi pod flag")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize metrics certificate watcher")
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		LeaseDuration:          &leaseDuration,
		RenewDeadline:          &renewDeadline,
		RetryPeriod:            &retryPeriod,
		// The leader releases the lease when the manager stops (rolling update, node drain), so
		// the other replica takes over within about retryPeriod instead of waiting out
		// leaseDuration. Safe because main returns as soon as mgr.Start does, with no
		// work after it that a new leader could race with.
		LeaderElectionReleaseOnCancel: true,
		Cache:                         cache.Options{ByObject: controller.CacheByObject()},
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if nifiSCC != "" {
		if _, err := mgr.GetRESTMapper().RESTMapping(
			schema.GroupKind{Group: "security.openshift.io", Kind: "SecurityContextConstraints"}); err != nil {
			setupLog.Info("--nifi-scc ignored: this cluster has no OpenShift SecurityContextConstraints API",
				"nifiSCC", nifiSCC, "reason", err.Error())
			nifiSCC = ""
		}
	}

	var csiSA types.NamespacedName
	if csiDriverServiceAccount != "" {
		ns, name, ok := strings.Cut(csiDriverServiceAccount, "/")
		if !ok || ns == "" || name == "" {
			setupLog.Error(nil, "--csi-driver-service-account must be <namespace>/<name>", "value", csiDriverServiceAccount)
			os.Exit(1)
		}
		csiSA = types.NamespacedName{Namespace: ns, Name: name}
	}

	if err := (&controller.NiFiClusterReconciler{
		NiFiStatusInterval:               nifiStatusInterval,
		OffloadThroughputBytesPerSecond:  offloadThroughput,
		AllowCustomServiceAccountBinding: allowCustomSABinding,
		Client:                           mgr.GetClient(),
		APIReader:                        mgr.GetAPIReader(),
		Scheme:                           mgr.GetScheme(),
		Recorder:                         mgr.GetEventRecorderFor("nifi-operator"),
		NiFiSCC:                          nifiSCC,
		NoFSGroup:                        fsGroup == nil,
		FSGroup:                          ptr.Deref(fsGroup, 0),
		RunAsUser:                        runAsUser,
		CSIDriverServiceAccount:          csiSA,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NiFiCluster")
		os.Exit(1)
	}
	// Exports every NiFiCluster's CA expiry, set/cleared by the reconciler itself - unlike the
	// webhook certificate's, there is no single file or Watcher to read it from.
	metrics.Registry.MustRegister(certmetrics.CAExpiryGauge)
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		webhookOpts := webhookv1alpha1.WebhookOptions{TrustedImageRepositories: splitList(trustedImageRepositories)}
		if err := webhookv1alpha1.SetupNiFiClusterWebhookWithManager(mgr, webhookOpts); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "NiFiCluster")
			os.Exit(1)
		}
		if err := webhookv1alpha1.SetupNiFiNodeGroupWebhookWithManager(mgr, webhookOpts); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "NiFiNodeGroup")
			os.Exit(1)
		}
		webhookv1alpha1.SetupNiFiNodeGroupScaleWebhookWithManager(mgr)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(everyReplica{metricsCertWatcher}); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(everyReplica{webhookCertWatcher}); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
		// Exports the serving certificate's expiry for the NiFiOperatorWebhookCertExpiring alert.
		metrics.Registry.MustRegister(certmetrics.ExpiryGauge)
		if err := mgr.Add(&certmetrics.Watcher{Path: filepath.Join(webhookCertPath, webhookCertName)}); err != nil {
			setupLog.Error(err, "unable to add webhook certificate expiry metric to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// A bare Ping only proves the HTTP server itself is alive, not that this instance can
	// actually do its job - in particular it says nothing about whether this replica has won
	// (or is even eligible to win) the leader-election lease, or whether it can reach the API
	// server at all. Listing the CRD this operator manages, via the API reader (a direct
	// client, not the cache, so it doesn't just report a locally cached answer) is a real,
	// end-to-end proof of both.
	if err := mgr.AddReadyzCheck("readyz", readyCheck(mgr.GetAPIReader())); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	// Every replica serves the admission webhook (only the leader reconciles), so a replica only
	// takes webhook traffic once its webhook server listens. Without the webhook the
	// server never starts, so the check is skipped then.
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			setupLog.Error(err, "unable to set up webhook ready check")
			os.Exit(1)
		}
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// everyReplica runs a manager Runnable on every replica, not just the elected leader. A Runnable
// that does not say otherwise only starts on the leader - for the certificate watchers that meant
// a standby replica, which serves the admission webhook too, never reloaded its renewed serving
// certificate and kept presenting the one it started with (found testing two operator
// replicas).
type everyReplica struct{ manager.Runnable }

// NeedLeaderElection reports false: see everyReplica.
func (everyReplica) NeedLeaderElection() bool { return false }

// readyCheck lists NiFiClusters, in every namespace, through reader.
func readyCheck(reader client.Reader) healthz.Checker {
	return func(_ *http.Request) error {
		readyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return reader.List(readyCtx, &nifiv1alpha1.NiFiClusterList{}, client.Limit(1))
	}
}

// envOr returns the environment variable name, or def when it is unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// splitList splits a comma-separated flag value, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// setDefaultNiFiImage sets the node image for NiFiClusters without spec.image from
// --default-nifi-image, keeping the built-in one when the flag is empty.
func setDefaultNiFiImage(ref string) error {
	if ref == "" {
		return nil
	}
	image, err := controller.ParseImage(ref)
	if err != nil {
		return err
	}
	controller.DefaultNiFiImage = image
	return nil
}

// nifiPodFlags applies --default-nifi-image and --nifi-seccomp-profile, and parses
// --nifi-fs-group and --nifi-run-as-user.
func nifiPodFlags(fsGroup, runAsUser, defaultImage, seccompProfile string) (*int64, *int64, error) {
	if err := setDefaultNiFiImage(defaultImage); err != nil {
		return nil, nil, fmt.Errorf("--default-nifi-image: %w", err)
	}
	profile, err := controller.ParseSeccompProfile(seccompProfile)
	if err != nil {
		return nil, nil, fmt.Errorf("--nifi-seccomp-profile: %w", err)
	}
	controller.NiFiSeccompProfile = profile
	if profile == "" {
		setupLog.Info("NiFi pods run without a seccomp profile (--nifi-seccomp-profile is empty); " +
			"the restricted Pod Security Standard requires one")
	}
	g, err := parseID(fsGroup, "group")
	if err != nil {
		return nil, nil, fmt.Errorf("--nifi-fs-group: %w", err)
	}
	u, err := parseID(runAsUser, "user")
	if err == nil && ptr.Deref(u, 1) == 0 {
		err = fmt.Errorf("0 is root, which the NiFi pods' runAsNonRoot rejects")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("--nifi-run-as-user: %w", err)
	}
	return g, u, nil
}

// parseID reads --nifi-fs-group and --nifi-run-as-user (kind "group" or "user"): empty means
// none (nil).
func parseID(value, kind string) (*int64, error) {
	if value == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 0 {
		return nil, fmt.Errorf("%q is not a %s ID", value, kind)
	}
	return &id, nil
}
