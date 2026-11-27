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
	"testing"

	corev1 "k8s.io/api/core/v1"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

func TestBuildClientServiceIsAlwaysClusterIP(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.ServiceType = corev1.ServiceTypeLoadBalancer
	svc := buildClientService(cluster, &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1})
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("per-node-group Service Type = %q, want ClusterIP (spec.serviceType applies to the cluster Service)", svc.Spec.Type)
	}
}

func TestBuildClusterService(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{
		{Name: "a", Replicas: 1, TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
		{Name: "b", Replicas: 1, TLS: &nifiv1alpha1.TLSSpec{Enabled: true}},
	}

	svc := buildClusterService(cluster)
	if svc.Name != testClusterName {
		t.Errorf("Name = %q, want the cluster name", svc.Name)
	}
	if _, ok := svc.Spec.Selector[labelNodeGroup]; ok {
		t.Errorf("selector %v must not pin one node group", svc.Spec.Selector)
	}
	if svc.Spec.Selector[labelAppInstance] != testClusterName {
		t.Errorf("selector %v must select the cluster's pods", svc.Spec.Selector)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Type = %q, want ClusterIP by default", svc.Spec.Type)
	}
	if svc.Spec.SessionAffinity != corev1.ServiceAffinityClientIP {
		t.Errorf("SessionAffinity = %q, want ClientIP (NiFi login tokens are per node)", svc.Spec.SessionAffinity)
	}
	if svc.Spec.PublishNotReadyAddresses {
		t.Error("cluster Service must not route to nodes that are not ready")
	}
	if p := svc.Spec.Ports[0]; p.Port != nifiHTTPSPort || p.Name != webPortNameHTTPS || p.AppProtocol == nil || *p.AppProtocol != "https" {
		t.Errorf("port = %+v, want https/%d with appProtocol https", p, nifiHTTPSPort)
	}

	cluster.Spec.ServiceType = corev1.ServiceTypeLoadBalancer
	if got := buildClusterService(cluster).Spec.Type; got != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %q, want spec.serviceType (LoadBalancer) honored", got)
	}

	// A NodePort stored before the type was removed is served as ClusterIP.
	cluster.Spec.ServiceType = corev1.ServiceTypeNodePort
	if got := buildClusterService(cluster); got.Spec.Type != corev1.ServiceTypeClusterIP || got.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("Type = %q (traffic policy %q), want a stored NodePort served as ClusterIP", got.Spec.Type, got.Spec.ExternalTrafficPolicy)
	}
}

// TestBuildClientServicePortMatchesTLSState: NiFi stops listening on nifiHTTPPort entirely once
// TLS is enabled (tlsProperties blanks nifi.web.http.port), so a client Service that always
// points at nifiHTTPPort would route traffic to a port nothing is listening on.
func TestBuildClientServicePortMatchesTLSState(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.ServiceType = corev1.ServiceTypeLoadBalancer

	t.Run("plain HTTP port when TLS is disabled", func(t *testing.T) {
		nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}
		svc := buildClientService(cluster, nodeGroup)
		if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != nifiHTTPPort || svc.Spec.Ports[0].Name != webPortNameHTTP {
			t.Errorf("Ports = %+v, want a single %q port %d", svc.Spec.Ports, webPortNameHTTP, nifiHTTPPort)
		}
	})

	t.Run("HTTPS port when TLS is enabled", func(t *testing.T) {
		nodeGroup := &nifiv1alpha1.NodeGroup{
			Name: "ng1", Replicas: 1,
			TLS: &nifiv1alpha1.TLSSpec{Enabled: true},
		}
		svc := buildClientService(cluster, nodeGroup)
		if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != nifiHTTPSPort || svc.Spec.Ports[0].Name != webPortNameHTTPS {
			t.Errorf("Ports = %+v, want a single %q port %d", svc.Spec.Ports, webPortNameHTTPS, nifiHTTPSPort)
		}
	})
}

// TestBuildHeadlessServicePortMatchesTLSState mirrors TestBuildClientServicePortMatchesTLSState
// for the headless Service, for the same reason: keeping its port list accurate to what NiFi
// actually listens on, even though nothing today routes traffic through it by port number.
func TestBuildHeadlessServicePortMatchesTLSState(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace

	t.Run("plain HTTP port when TLS is disabled", func(t *testing.T) {
		nodeGroup := &nifiv1alpha1.NodeGroup{Name: "ng1", Replicas: 1}
		svc := buildHeadlessService(cluster, nodeGroup)
		if svc.Spec.Ports[0].Port != nifiHTTPPort || svc.Spec.Ports[0].Name != webPortNameHTTP {
			t.Errorf("Ports[0] = %+v, want %q port %d", svc.Spec.Ports[0], webPortNameHTTP, nifiHTTPPort)
		}
	})

	t.Run("HTTPS port when TLS is enabled", func(t *testing.T) {
		nodeGroup := &nifiv1alpha1.NodeGroup{
			Name: "ng1", Replicas: 1,
			TLS: &nifiv1alpha1.TLSSpec{Enabled: true},
		}
		svc := buildHeadlessService(cluster, nodeGroup)
		if svc.Spec.Ports[0].Port != nifiHTTPSPort || svc.Spec.Ports[0].Name != webPortNameHTTPS {
			t.Errorf("Ports[0] = %+v, want %q port %d", svc.Spec.Ports[0], webPortNameHTTPS, nifiHTTPSPort)
		}
	})
}

func TestBuildClusterServiceOptions(t *testing.T) {
	class := "metallb"
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Namespace = testClusterNamespace
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "a", Replicas: 1}}
	cluster.Spec.Annotations = map[string]string{"team": "data"}
	cluster.Spec.Service = &nifiv1alpha1.ClusterServiceSpec{
		Annotations:              map[string]string{"lb.example.com/internal": "true"},
		LoadBalancerClass:        &class,
		LoadBalancerSourceRanges: []string{"10.0.0.0/8"},
		ExternalTrafficPolicy:    corev1.ServiceExternalTrafficPolicyLocal,
	}

	// ClusterIP: load balancer fields and the traffic policy must stay unset (the API rejects them).
	svc := buildClusterService(cluster)
	if svc.Spec.LoadBalancerClass != nil || svc.Spec.LoadBalancerSourceRanges != nil || svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("ClusterIP Service got LoadBalancer-only fields: %+v", svc.Spec)
	}
	if svc.Annotations["team"] != "data" || svc.Annotations["lb.example.com/internal"] != "true" {
		t.Errorf("annotations = %v, want cluster and service annotations merged", svc.Annotations)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Errorf("ports = %+v, want only the web port", svc.Spec.Ports)
	}

	cluster.Spec.ServiceType = corev1.ServiceTypeLoadBalancer
	svc = buildClusterService(cluster)
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != class ||
		len(svc.Spec.LoadBalancerSourceRanges) != 1 || svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("LoadBalancer Service spec = %+v", svc.Spec)
	}
}

func TestKeepAllocatedNodePorts(t *testing.T) {
	desired := []corev1.ServicePort{{Name: "http", Port: 8080}, {Name: "new", Port: 9090}}
	existing := []corev1.ServicePort{{Name: "http", Port: 8080, NodePort: 31234}}
	got := keepAllocatedNodePorts(desired, existing)
	if got[0].NodePort != 31234 || got[1].NodePort != 0 {
		t.Errorf("ports = %+v, want http to keep 31234 and the new port left for allocation", got)
	}
	if desired[0].NodePort != 0 {
		t.Error("desired ports were modified in place")
	}
}

func TestBuildClusterServiceTrafficPolicyResetsToClusterDefault(t *testing.T) {
	cluster := &nifiv1alpha1.NiFiCluster{}
	cluster.Name = testClusterName
	cluster.Spec.NodeGroups = []nifiv1alpha1.NodeGroup{{Name: "a", Replicas: 1}}
	cluster.Spec.ServiceType = corev1.ServiceTypeLoadBalancer
	if got := buildClusterService(cluster).Spec.ExternalTrafficPolicy; got != corev1.ServiceExternalTrafficPolicyCluster {
		t.Errorf("unset traffic policy on LoadBalancer = %q, want the explicit Cluster default", got)
	}
	cluster.Spec.ServiceType = corev1.ServiceTypeClusterIP
	if got := buildClusterService(cluster).Spec.ExternalTrafficPolicy; got != "" {
		t.Errorf("ClusterIP traffic policy = %q, want unset", got)
	}
}
