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
	"fmt"
	"net"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// shutdownTimeout bounds how long Serve waits for in-flight CSI calls to finish once ctx is
// cancelled before cutting them off - kept under the DaemonSet's default 30s
// terminationGracePeriodSeconds so the kubelet's SIGKILL never lands mid-drain.
const shutdownTimeout = 20 * time.Second

// Serve starts the CSI gRPC server on a Unix domain socket at socketPath, wiring the Identity
// and Node services together. Blocks until the listener errors (e.g. the socket is removed out
// from under it) or ctx is cancelled - kubelet expects a CSI driver's gRPC server to run for
// the lifetime of the container, not to exit after handling one request. On cancellation
// (SIGTERM during a rollout) it stops accepting calls and lets in-flight ones - a NodePublish
// mid-way through writing a pod's keystore - finish, up to shutdownTimeout, then returns nil.
//
// socketPath is expected to be inside the shared hostPath volume the driver DaemonSet mounts
// (see config/csi/daemonset.yaml) - the standard CSI convention kubelet and the
// node-driver-registrar sidecar both rely on to find this driver's socket.
func Serve(ctx context.Context, socketPath string, k8sClient client.Client, nodeID string) error {
	// A CSI driver restarting (e.g. after a crash or upgrade) must not fail to bind because a
	// stale socket file from its previous run is still there - kubelet does not clean this up
	// on its own.
	if err := os.RemoveAll(socketPath); err != nil {
		return fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}

	grpcServer := grpc.NewServer()
	csi.RegisterIdentityServer(grpcServer, &IdentityServer{})
	csi.RegisterNodeServer(grpcServer, &NodeServer{Client: k8sClient, NodeID: nodeID})

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(listener) }()
	select {
	case err := <-serveErr:
		return fmt.Errorf("serving CSI gRPC on %s: %w", socketPath, err)
	case <-ctx.Done():
	}

	drained := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(shutdownTimeout):
		grpcServer.Stop()
	}
	// Serve returns nil once Stop/GracefulStop has been called.
	return <-serveErr
}
