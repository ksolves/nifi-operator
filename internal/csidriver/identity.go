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

// Package csidriver implements a CSI (Container Storage Interface) node plugin that mints a
// fresh, per-pod TLS certificate at pod-mount time, using a CSI ephemeral inline volume, signed by
// internal/controller's operator-owned CA (docs/tls.md).
//
// Only the Identity and Node gRPC services are implemented (no Controller service): an ephemeral
// inline volume's lifecycle is tied 1:1 to its pod's, so kubelet never calls
// CreateVolume/ControllerPublishVolume/etc. for it - see the CSI spec's ephemeral volume mode.
package csidriver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// driverName is this CSI driver's name, as registered in the CSIDriver object (config/csi) and
// referenced by the StatefulSet's ephemeral volume (see statefulset.go).
const driverName = "tls.nifi.ksolves.com"

// driverVersion is reported via GetPluginInfo. Bump it when the driver's behavior changes in a
// way worth being able to distinguish in `kubectl describe csinode`/driver logs.
const driverVersion = "v1"

// IdentityServer implements the CSI Identity service - boilerplate every CSI driver must expose
// so kubelet/external-provisioners can discover its name, version and capabilities.
type IdentityServer struct {
	csi.UnimplementedIdentityServer
}

func (s *IdentityServer) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          driverName,
		VendorVersion: driverVersion,
	}, nil
}

func (s *IdentityServer) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	// Deliberately empty: this driver has no Controller service capabilities (no
	// CreateVolume/attach-detach - see the package doc) and no volume-expansion or other
	// optional capability. An ephemeral-inline-only Node plugin advertises nothing extra here.
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (s *IdentityServer) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
