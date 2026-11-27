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
	"fmt"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

// statefulSetName returns the name of the StatefulSet for a node group.
func statefulSetName(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return StatefulSetName(cluster.Name, nodeGroup.Name)
}

// StatefulSetName is statefulSetName's string-only form, for the CSI driver
// (internal/csidriver), which only has clusterName/nodeGroupName as strings from volume
// attributes and needs the exact StatefulSet name to verify a mounting pod's ownership.
func StatefulSetName(clusterName, nodeGroupName string) string {
	return fmt.Sprintf("%s-%s", clusterName, nodeGroupName)
}

// headlessServiceName returns the name of the headless Service used for cluster-internal DNS
// and node discovery for a node group.
func headlessServiceName(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return fmt.Sprintf("%s-%s-headless", cluster.Name, nodeGroup.Name)
}

// clientServiceName returns the name of the ClusterIP Service selecting only one node group's
// pods (direct access to that group).
func clientServiceName(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return fmt.Sprintf("%s-%s", cluster.Name, nodeGroup.Name)
}

// clusterServiceName returns the name of the Service selecting every NiFi pod of the cluster
// across all node groups - the one entry point for clients and user-managed routes. It is the
// NiFiCluster's own name, which can never collide with a per-node-group "<cluster>-<group>" name.
func clusterServiceName(cluster *nifiv1alpha1.NiFiCluster) string {
	return cluster.Name
}

// clusterServiceDNSNames returns the in-cluster DNS names of clusterServiceName. They are added
// to every node certificate, because a reencrypting router or gateway verifies the NiFi node's
// certificate against the Service's DNS name.
func clusterServiceDNSNames(cluster *nifiv1alpha1.NiFiCluster) []string {
	svc := clusterServiceName(cluster)
	return []string{
		fmt.Sprintf("%s.%s.svc", svc, cluster.Namespace),
		fmt.Sprintf("%s.%s.svc.%s", svc, cluster.Namespace, ClusterDomain),
	}
}

// configMapName returns the name of the ConfigMap holding a node group's NiFi configuration.
func configMapName(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return fmt.Sprintf("%s-%s-config", cluster.Name, nodeGroup.Name)
}

// configSecretName returns the name of the operator-owned Secret holding a node group's
// Secret-sourced nifi.properties overrides (the ConfigMap's counterpart for values that must not
// be stored in plain text).
func configSecretName(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return configMapName(cluster, nodeGroup) + "-secret"
}

// entrypointSecretName returns the name of the operator-owned Secret holding a node group's
// entrypoint script (NIFI-248 B2): a NiFi pod's RBAC Role grants it broad access to ConfigMaps in
// its namespace (NiFi's own KubernetesConfigMapStateProvider creates one per component, with
// names the operator does not control, so that grant can't be narrowed to one cluster's own
// ConfigMaps). The same Role grants nothing on Secrets at all, so a Secret is what actually keeps
// a compromised pod in one NiFiCluster from rewriting another's startup script.
func entrypointSecretName(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return configMapName(cluster, nodeGroup) + "-entrypoint"
}

// sensitivePropsSecretName returns the name of the Secret holding the cluster's
// nifi.sensitive.props.key. One per cluster: every node of a NiFi cluster must share the same key
// to decrypt the one flow they all run.
func sensitivePropsSecretName(cluster *nifiv1alpha1.NiFiCluster) string {
	return fmt.Sprintf("%s-sensitive-props", cluster.Name)
}

// legacySensitivePropsSecretName returns the name of the per-node-group sensitive-props Secret
// created by operator versions that ran each node group as its own NiFi cluster. Read only to
// adopt its key during migration (see reconcileSensitivePropsSecret).
func legacySensitivePropsSecretName(cluster *nifiv1alpha1.NiFiCluster, nodeGroup *nifiv1alpha1.NodeGroup) string {
	return fmt.Sprintf("%s-%s-sensitive-props", cluster.Name, nodeGroup.Name)
}

// nifiServiceAccountName returns the name of the ServiceAccount the operator creates for the
// cluster's NiFi pods when spec.serviceAccountName is unset.
func nifiServiceAccountName(cluster *nifiv1alpha1.NiFiCluster) string {
	return fmt.Sprintf("%s-nifi", cluster.Name)
}

// nifiRoleName returns the name of the Role (and its RoleBinding) granting the cluster's NiFi
// pods the Lease and ConfigMap access Kubernetes coordination needs.
func nifiRoleName(cluster *nifiv1alpha1.NiFiCluster) string {
	return fmt.Sprintf("%s-nifi", cluster.Name)
}

// nifiSCCRoleBindingName returns the name of the RoleBinding that lets the cluster's NiFi pods
// use the configured OpenShift SecurityContextConstraints.
func nifiSCCRoleBindingName(cluster *nifiv1alpha1.NiFiCluster) string {
	return fmt.Sprintf("%s-nifi-scc", cluster.Name)
}

// ClusterDomain is the Kubernetes cluster's DNS domain, used in pod FQDNs, NiFi node addresses
// and certificate SANs. Set once at startup from --cluster-domain by both the operator and the CSI
// driver, which must agree.
var ClusterDomain = "cluster.local"

// nifiClusterKind is the Kind of the operator's custom resource, as it appears in owner references.
const nifiClusterKind = "NiFiCluster"

// csiCAReaderName returns the name of the Role (and its RoleBinding) that lets the TLS CSI
// driver read this cluster's CA Secret, and nothing else in the namespace.
func csiCAReaderName(cluster *nifiv1alpha1.NiFiCluster) string {
	return fmt.Sprintf("%s-csi-ca-reader", cluster.Name)
}

// tlsCASecretName returns the name of the Secret holding a NiFiCluster's self-signed TLS CA -
// one per cluster (not per node group, unlike sensitivePropsSecretName), matching NiFiKop's own
// NodeCACertTemplate naming convention (<cluster>-ca-certificate) rather than inventing a new
// scheme. Referenced by name alone (not owner-reference lookup) by the CSI driver, which reads
// it directly via the clusterName/namespace volume attributes it receives - see
// internal/csidriver.
func tlsCASecretName(cluster *nifiv1alpha1.NiFiCluster) string {
	return TLSCASecretName(cluster.Name)
}

// TLSCASecretName is tlsCASecretName's string-only form, for callers that only have a cluster
// name rather than the full NiFiCluster object - namely the CSI driver (internal/csidriver),
// which resolves the CA Secret from the clusterName volume attribute set on the pod template.
func TLSCASecretName(clusterName string) string {
	return fmt.Sprintf("%s-ca-certificate", clusterName)
}

// authSecretName returns the name of the Secret the operator synthesizes to back the cluster's
// NiFi login: from spec.authentication.singleUser's literal Username/Password fields when set,
// or an operator-generated default login when spec.authentication is left unset entirely - so
// pod env vars always source from a Secret either way. One per cluster (like tlsCASecretName),
// since NiFi's login identity is shared cluster-wide, not per node group.
func authSecretName(cluster *nifiv1alpha1.NiFiCluster) string {
	return AuthSecretName(cluster.Name)
}

// AuthSecretName is authSecretName's string-only form, for the webhook: a user-supplied
// singleUser.secretName must never name the operator's own Secret.
func AuthSecretName(clusterName string) string {
	return fmt.Sprintf("%s-auth-credentials", clusterName)
}

// PodFQDN returns the fully-qualified DNS name a specific pod is reachable at through its node
// group's headless Service, built from plain string identifiers - mirrors
// headlessServiceName/statefulSetName's naming pattern without requiring a NiFiCluster/NodeGroup
// object, since the CSI driver's NodePublishVolume only has clusterName/nodeGroupName/namespace
// (from static volume attributes) and podName (from kubelet-injected pod info) as strings.
func PodFQDN(podName, clusterName, nodeGroupName, namespace string) string {
	return fmt.Sprintf("%s.%s-%s-headless.%s.svc.%s", podName, clusterName, nodeGroupName, namespace, ClusterDomain)
}
