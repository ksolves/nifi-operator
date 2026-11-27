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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// buildHeadlessService renders the headless Service used for StatefulSet pod DNS and
// cluster-internal node discovery.
func buildHeadlessService(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *corev1.Service {
	selector := map[string]string{
		labelAppName:     appNameValue,
		labelAppInstance: cluster.Name,
		labelNodeGroup:   nodeGroup.Name,
	}

	// Matches buildClientService: NiFi stops listening on the plain HTTP port entirely once TLS
	// is enabled (tlsProperties blanks nifi.web.http.port), so this Service's port list should
	// reflect the port NiFi actually listens on, even though nothing today routes cluster-internal
	// traffic through this Service's own port numbers (the readiness probe and preStop hook both
	// dial the pod's hostname directly) - kept accurate for anything that inspects the Service.
	webPortName, webPort := webPortNameHTTP, int32(nifiHTTPPort)
	if nodeGroup.TLS != nil && nodeGroup.TLS.Enabled {
		webPortName, webPort = webPortNameHTTPS, nifiHTTPSPort
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        headlessServiceName(cluster, nodeGroup),
			Namespace:   cluster.Namespace,
			Labels:      commonLabels(cluster, nodeGroup),
			Annotations: mergedAnnotations(cluster, nodeGroup),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                "None",
			PublishNotReadyAddresses: true,
			Selector:                 selector,
			Ports: []corev1.ServicePort{
				{Name: webPortName, Port: webPort, TargetPort: intstr.FromInt32(webPort), Protocol: corev1.ProtocolTCP},
				{Name: "cluster-proto", Port: nifiClusterProtocolPort, TargetPort: intstr.FromInt32(nifiClusterProtocolPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// buildClientService renders the ClusterIP Service selecting one node group's pods. It is kept
// for direct per-group access; external exposure goes
// through buildClusterService, which is what spec.serviceType applies to.
func buildClientService(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) *corev1.Service {
	selector := map[string]string{
		labelAppName:     appNameValue,
		labelAppInstance: cluster.Name,
		labelNodeGroup:   nodeGroup.Name,
	}
	portName, port := webPort(tlsEnabled(nodeGroup))

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        clientServiceName(cluster, nodeGroup),
			Namespace:   cluster.Namespace,
			Labels:      commonLabels(cluster, nodeGroup),
			Annotations: mergedAnnotations(cluster, nodeGroup),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{
				{Name: portName, Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP, AppProtocol: &portName},
			},
		},
	}
}

// clientSessionAffinityTimeoutSeconds is the longest ClientIP affinity Kubernetes allows. NiFi's
// docs ask for an affinity timeout longer than its login session (12 hours by default).
const clientSessionAffinityTimeoutSeconds int32 = 86400

// buildClusterService renders the Service selecting every NiFi pod of the cluster, across all
// node groups (selector without the node-group label). Not-ready nodes are left out of it
// (publishNotReadyAddresses stays false, unlike the headless Services used for peer discovery),
// so no UI traffic reaches a node that has not joined the cluster yet.
//
// NiFi requires session affinity for its UI/API: each node signs login tokens with its own key.
// ClientIP affinity covers clients reaching this Service directly (ClusterIP from inside the
// cluster, or a LoadBalancer that keeps client IPs); routers and gateways in front of it send traffic straight to pod IPs and
// need their own stickiness, such as a cookie.
func buildClusterService(cluster *nifiv1alpha1.NiFiCluster) *corev1.Service {
	// Anything else is ClusterIP, including a NodePort stored before that type was removed.
	serviceType := corev1.ServiceTypeClusterIP
	if cluster.Spec.ServiceType == corev1.ServiceTypeLoadBalancer {
		serviceType = corev1.ServiceTypeLoadBalancer
	}
	portName, port := webPort(clusterTLSEnabled(cluster))
	timeout := clientSessionAffinityTimeoutSeconds

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        clusterServiceName(cluster),
			Namespace:   cluster.Namespace,
			Labels:      clusterLabels(cluster),
			Annotations: cluster.Spec.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type: serviceType,
			Selector: map[string]string{
				labelAppName:     appNameValue,
				labelAppInstance: cluster.Name,
			},
			SessionAffinity: corev1.ServiceAffinityClientIP,
			SessionAffinityConfig: &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: &timeout},
			},
			Ports: []corev1.ServicePort{
				{Name: portName, Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP, AppProtocol: &portName},
			},
		},
	}

	// Explicit Kubernetes default on the types that have a traffic policy, so removing the setting
	// resets it and an unset value never differs from what the API server stored.
	if serviceType != corev1.ServiceTypeClusterIP {
		svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	}

	opts := cluster.Spec.Service
	if opts == nil {
		return svc
	}
	svc.Annotations = mergeStringMaps(cluster.Spec.Annotations, opts.Annotations)
	// The API server rejects these on a type that does not use them, so they are only set where
	// they apply.
	if serviceType == corev1.ServiceTypeLoadBalancer {
		svc.Spec.LoadBalancerClass = opts.LoadBalancerClass
		svc.Spec.LoadBalancerSourceRanges = opts.LoadBalancerSourceRanges
	}
	if serviceType != corev1.ServiceTypeClusterIP && opts.ExternalTrafficPolicy != "" {
		svc.Spec.ExternalTrafficPolicy = opts.ExternalTrafficPolicy
	}
	return svc
}

// keepAllocatedNodePorts copies the node ports the API server already assigned onto desired ports
// of the same name. Desired ports never carry them, and without this every reconcile of a
// LoadBalancer Service would be an (otherwise no-op) write.
func keepAllocatedNodePorts(desired, existing []corev1.ServicePort) []corev1.ServicePort {
	allocated := map[string]int32{}
	for _, p := range existing {
		allocated[p.Name] = p.NodePort
	}
	out := make([]corev1.ServicePort, len(desired))
	for i, p := range desired {
		if p.NodePort == 0 {
			p.NodePort = allocated[p.Name]
		}
		out[i] = p
	}
	return out
}

// webPort returns the name and number of the NiFi web port for the given TLS state: NiFi
// listens on exactly one of them.
func webPort(tls bool) (string, int32) {
	if tls {
		return webPortNameHTTPS, nifiHTTPSPort
	}
	return webPortNameHTTP, nifiHTTPPort
}

// tlsEnabled reports whether a node group has TLS turned on.
func tlsEnabled(nodeGroup *nifiv1alpha1.NodeGroup) bool {
	return nodeGroup.TLS != nil && nodeGroup.TLS.Enabled
}

// clusterTLSEnabled reports whether the cluster runs with TLS. The validating webhook keeps every
// node group on the same setting, so the first group decides.
func clusterTLSEnabled(cluster *nifiv1alpha1.NiFiCluster) bool {
	return len(cluster.Spec.NodeGroups) > 0 && tlsEnabled(&cluster.Spec.NodeGroups[0])
}
