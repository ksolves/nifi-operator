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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestIdentityServerGetPluginInfo(t *testing.T) {
	resp, err := (&IdentityServer{}).GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo() error = %v", err)
	}
	if resp.GetName() != driverName {
		t.Errorf("GetPluginInfo().Name = %q, want %q", resp.GetName(), driverName)
	}
	if resp.GetVendorVersion() != driverVersion {
		t.Errorf("GetPluginInfo().VendorVersion = %q, want %q", resp.GetVendorVersion(), driverVersion)
	}
}

func TestIdentityServerGetPluginCapabilities(t *testing.T) {
	resp, err := (&IdentityServer{}).GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities() error = %v", err)
	}
	// This driver only ever runs as an ephemeral-inline Node plugin - it must never advertise a
	// Controller-service capability (CreateVolume/attach-detach) it doesn't implement, since
	// kubelet/CSI sidecars would then start calling RPCs this driver has no handler for.
	if len(resp.GetCapabilities()) != 0 {
		t.Errorf("GetPluginCapabilities() = %v, want no capabilities advertised", resp.GetCapabilities())
	}
}

func TestIdentityServerProbe(t *testing.T) {
	resp, err := (&IdentityServer{}).Probe(context.Background(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if !resp.GetReady().GetValue() {
		t.Error("Probe().Ready = false, want true")
	}
}
