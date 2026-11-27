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
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// dialCSISocket connects to socketPath and returns an Identity client, failing the test if the
// server isn't actually listening within a short deadline - Serve runs in a background goroutine,
// so callers must wait for the socket file to exist before dialing.
func dialCSISocket(t *testing.T, socketPath string) csi.IdentityClient {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%s): %v", socketPath, err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Logf("closing connection: %v", err)
		}
	})
	return csi.NewIdentityClient(conn)
}

// TestServe_ListensAndServes proves Serve actually wires up a working CSI Identity service on
// the given socket - not just that it doesn't error immediately.
func TestServe_ListensAndServes(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "csi.sock")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(t.Context(), socketPath, fakeClient, "test-node") }()
	t.Cleanup(func() {
		select {
		case err := <-errCh:
			t.Logf("Serve() returned: %v", err)
		default:
		}
	})

	identityClient := dialCSISocket(t, socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := identityClient.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo() over the socket = error %v, want a live server", err)
	}
	if resp.GetName() != driverName {
		t.Errorf("GetPluginInfo().Name = %q, want %q", resp.GetName(), driverName)
	}
}

// TestServe_RemovesStaleSocket is the regression test for the documented restart case: a
// previous run's socket file left behind must not block a fresh Serve() from binding.
func TestServe_RemovesStaleSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "csi.sock")

	// Simulate a stale socket from a crashed previous run. A graceful Close() on a
	// *net.UnixListener unlinks its socket file, which isn't what a killed process leaves
	// behind - SetUnlinkOnClose(false) reproduces the real "process died, file stayed" case.
	stale, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("creating stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("closing stale listener: %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("expected stale socket file to still exist, stat err = %v", err)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(t.Context(), socketPath, fakeClient, "test-node") }()

	identityClient := dialCSISocket(t, socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// WaitForReady: the stale socket file exists from the start, so dialCSISocket returns before
	// Serve has removed and rebound it; without it the call fails fast with "connection refused".
	if _, err := identityClient.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("GetPluginInfo() after stale-socket removal = error %v, want Serve() to have rebound cleanly", err)
	}
}

// TestServe_StopsOnContextCancel proves a cancelled context (SIGTERM in the real binary) shuts
// the server down cleanly: Serve returns nil instead of blocking until the process is killed,
// and the socket is unlinked so the next run doesn't find a stale one.
func TestServe_StopsOnContextCancel(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "csi.sock")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, socketPath, fakeClient, "test-node") }()

	identityClient := dialCSISocket(t, socketPath)
	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()
	if _, err := identityClient.GetPluginInfo(callCtx, &csi.GetPluginInfoRequest{}); err != nil {
		t.Fatalf("GetPluginInfo() before shutdown = error %v, want a live server", err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve() after context cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return within 5s of context cancel")
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file after shutdown: stat err = %v, want it removed", err)
	}
}
