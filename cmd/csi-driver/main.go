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

// Command csi-driver is the entrypoint for the TLS CSI node plugin - a
// separate binary and container image from the operator's own manager (cmd/main.go), deployed
// as its own DaemonSet (config/csi/daemonset.yaml), not as part of the operator Deployment.
// See internal/csidriver for what it actually does.
package main

import (
	"flag"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
	"github.com/ksolves/nifi-operator/internal/controller"
	"github.com/ksolves/nifi-operator/internal/csidriver"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nifiv1alpha1.AddToScheme(scheme))
}

func main() {
	var csiEndpoint string
	// CSI_ENDPOINT is the conventional env var kubelet/the node-driver-registrar sidecar
	// expect a CSI driver to honor; the flag exists for manual/local runs (e.g. the `csc`
	// sanity-check tool) where setting an env var is more awkward than a flag.
	flag.StringVar(&csiEndpoint, "csi-endpoint", os.Getenv("CSI_ENDPOINT"),
		"The Unix socket endpoint this driver listens on, as unix:///path/to/csi.sock. "+
			"Defaults to the CSI_ENDPOINT environment variable if set.")
	flag.StringVar(&controller.ClusterDomain, "cluster-domain", envOr("CLUSTER_DOMAIN", "cluster.local"),
		"The Kubernetes cluster's DNS domain, used in NiFi pod FQDNs and certificate SANs. "+
			"The operator and the CSI driver must agree.")
	// Production logging by default; --zap-devel switches to development output.
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	if csiEndpoint == "" {
		setupLog.Error(nil, "no CSI endpoint given: set --csi-endpoint or the CSI_ENDPOINT environment variable")
		os.Exit(1)
	}
	nodeID := os.Getenv("NODE_NAME")
	if nodeID == "" {
		setupLog.Error(nil, "NODE_NAME environment variable is not set - "+
			"the DaemonSet must set it from spec.nodeName via the downward API")
		os.Exit(1)
	}
	socketPath, ok := strings.CutPrefix(csiEndpoint, "unix://")
	if !ok {
		setupLog.Error(nil, "unsupported CSI endpoint scheme: only unix:// is supported", "endpoint", csiEndpoint)
		os.Exit(1)
	}

	k8sClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	setupLog.Info("starting CSI driver", "endpoint", csiEndpoint, "nodeID", nodeID)
	// SIGTERM/SIGINT cancel this context, which drains in-flight CSI calls before exiting.
	if err := csidriver.Serve(ctrl.SetupSignalHandler(), socketPath, k8sClient, nodeID); err != nil {
		setupLog.Error(err, "CSI driver server exited with an error")
		os.Exit(1)
	}
}

// envOr returns the environment variable name, or def when it is unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
