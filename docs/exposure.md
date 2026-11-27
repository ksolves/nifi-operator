# Exposing a NiFi cluster

The operator creates **one** Service for the whole cluster. Every other exposure - an Ingress, a
Gateway API HTTPRoute, an OpenShift Route - is yours to create, pointed at that Service, so all
node groups are reachable through a single entry point.

| Object | Name | Created when |
|---|---|---|
| Service (all node groups) | `<name>` | always; `spec.serviceType` (ClusterIP or LoadBalancer) applies to it |
| Service (one node group) | `<name>-<group>` | always; ClusterIP, for direct access to that group |

The cluster Service only routes to nodes that are ready. It uses `ClientIP` session affinity, which
applies to clients that reach it directly (in-cluster, or through a LoadBalancer that keeps client
IPs). Its port follows the TLS state:
`8443` named `https` with TLS, `8080` named `http` without.

## Service type and load balancers

```yaml
spec:
  serviceType: LoadBalancer          # ClusterIP (default) | LoadBalancer (needs TLS)
  service:
    annotations:                     # on the cluster Service only (cloud LB settings)
      service.beta.kubernetes.io/aws-load-balancer-scheme: internal
    loadBalancerClass: metallb       # LoadBalancer only
    loadBalancerSourceRanges:        # LoadBalancer only - who may connect
      - 10.0.0.0/8
    externalTrafficPolicy: Local     # LoadBalancer: keep client IPs (needed for ClientIP affinity)
```

- `NodePort` is not a `serviceType`. For node-port access, or for the ports of listening
  processors such as ListenHTTP, create a Service of your own selecting the NiFi pods
  (`app.kubernetes.io/name: nifi`, `app.kubernetes.io/instance: <name>`) with a numeric
  `targetPort`; the NiFi container does not need to declare the port.
- `LoadBalancer` needs TLS on the node groups (the default); the webhook rejects it on a cluster
  without TLS. A cluster stored that way keeps working, but changing `serviceType` or TLS is
  checked.
- The webhook warns when a LoadBalancer has no `loadBalancerSourceRanges` (open to any address)
  and when a setting does not apply to the chosen `serviceType`.
- Node ports Kubernetes assigned are kept across reconciles.
- Site-to-site through a load balancer is not handled: NiFi advertises each node's own address to
  site-to-site clients, so they must reach the nodes directly.

## Session affinity is required

With more than one node, NiFi requires every request from one browser session to reach **the
same node**: each node signs its own login tokens and keeps its own change-request cache
([NiFi Administration Guide, "Session Affinity"](https://nifi.apache.org/nifi-docs/administration-guide.html)).
Without it, logins fail intermittently with HTTP 401. Routers and gateways send traffic straight
to pod IPs, so configure stickiness on whatever you put in front of the Service (a cookie on an
OpenShift Route or Ingress controller; your Gateway implementation's session persistence).

## Your own Route, Ingress or HTTPRoute

Point it at the cluster Service `<name>` and add its hostname to `spec.proxyHosts` (see below).

OpenShift Route, with TLS on the nodes (the router verifies the node certificate against the
cluster CA; the Service DNS name is on every node certificate):

```sh
kubectl get secret <name>-ca-certificate -n <ns> -o jsonpath='{.data.tls\.crt}' | base64 -d > ca.crt
oc create route reencrypt <name> -n <ns> --service=<name> --port=https \
  --hostname=nifi.apps.example.com --dest-ca-cert=ca.crt
oc annotate route <name> -n <ns> haproxy.router.openshift.io/timeout=5m
```

Edge and reencrypt Routes pin each browser to one node with a cookie by default. The timeout
annotation keeps long NiFi requests (large uploads, provenance queries) from being cut off.

Gateway API HTTPRoute (with TLS on the nodes your Gateway also needs a `BackendTLSPolicy`
pointing at the cluster CA, Gateway API v1.4+):

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: nifi
spec:
  parentRefs:
    - name: shared-gateway
      namespace: gateway-system
  hostnames: [nifi.example.com]
  rules:
    - backendRefs:
        - name: <name>          # the cluster Service
          port: 8443            # 8080 without TLS
```

## Hostnames and certificates

- `spec.proxyHosts` is NiFi's list of allowed proxy hosts (`nifi.web.proxy.host`): list every
  hostname clients use. NiFi rejects other Host headers.
- With TLS, each node certificate carries the pod's own FQDN (`<pod>.<name>-<group>-headless.<ns>.svc.cluster.local`), the cluster Service names
  (`<name>.<ns>.svc`, which reencrypting routers and gateways verify), every proxy host and
  `tls.extraSANs`. NiFi 2 rejects TLS requests whose SNI is not on its certificate.

Certificates are issued when a pod starts, so changing any of these hostnames rolls the TLS pods.

## Upgrading

- Clusters that had `spec.route` or `spec.gateway` keep the Route, HTTPRoute, BackendTLSPolicy
  and `<name>-ca-bundle` ConfigMap the operator created; it no longer updates them, and they are
  removed with the NiFiCluster. Remove the fields from the spec, and add the hostname to
  `spec.proxyHosts` so NiFi keeps accepting it. To keep managing them, take them over: remove
  their owner reference.
- `spec.serviceType` applies to the `<name>` Service; the per-group `<name>-<group>` Services
  are ClusterIP.
- A cluster stored with `serviceType: NodePort` keeps validating (CRD validation ratcheting,
  Kubernetes 1.30+), but its Service becomes ClusterIP and the node ports are released; set
  `serviceType` to `ClusterIP` or `LoadBalancer`, or create your own NodePort Service.
  `spec.service.extraPorts` is dropped: the ports leave the Service and the NiFi container.
- The operator needs the Service name `<name>` (the NiFiCluster's name) in the namespace. If a
  Service of that name already exists and is not the operator's, it is left alone and the
  NiFiCluster reports `ServiceReady=False` with reason `ServiceNameConflict` until it is renamed or
  removed.
