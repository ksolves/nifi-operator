# First steps

With the operator [installed](installation.md), this page creates a secure NiFi cluster, opens its
UI, scales it and removes it again. It takes about 15 minutes.

## 1. Create a namespace

NiFi runs in its own namespace. The NiFi pods meet the Kubernetes `restricted` Pod Security
Standard, so the namespace can enforce it:

```sh
kubectl create namespace nifi
kubectl label namespace nifi pod-security.kubernetes.io/enforce=restricted
```

## 2. Create a NiFiCluster

```sh
kubectl apply -f - <<'EOF'
apiVersion: nifi.ksolves.com/v1alpha1
kind: NiFiCluster
metadata:
  name: nifi
  namespace: nifi
spec:
  nodeGroups:
    - name: default
      replicas: 1
      storage: 10Gi         # flowfile, content and provenance repositories
      logStorage: 1Gi
      tls:
        enabled: true       # the default: HTTPS and a login; certificates are issued and rotated by the operator
      resources:
        requests:
          cpu: 500m
          memory: 2Gi
        limits:
          memory: 2Gi
EOF
```

That is the whole configuration. Without `spec.image` the cluster runs the stock
`apache/nifi:2.12.0` image, and without `spec.authentication` the operator generates a login. Every
field is described in the [CRD reference](../crd-reference.md).

## 3. Wait until it is healthy

```sh
kubectl -n nifi get pods -w        # Ctrl-C when nifi-default-0 shows 1/1 Running (2-5 minutes)
kubectl -n nifi wait --for=condition=ClusterHealthy nificlusters.nifi.ksolves.com/nifi --timeout=15m
kubectl -n nifi get nificlusters.nifi.ksolves.com nifi -o wide
```

```
NAME   READY   REPLICAS   COORDINATION   AUTH SECRET             VERSION   HEALTHY   AGE
nifi   1       1          Kubernetes     nifi-auth-credentials   2.12.0    True      4m
```

`HEALTHY` comes from NiFi itself: every node is connected to the cluster. If it stays empty or
`False`, see [Troubleshooting](../troubleshooting.md).

## 4. Open the UI

Read the generated login:

```sh
kubectl -n nifi get secret nifi-auth-credentials -o jsonpath='{.data.username}' | base64 -d; echo
kubectl -n nifi get secret nifi-auth-credentials -o jsonpath='{.data.password}' | base64 -d; echo
```

The operator's cluster Service `nifi` is ClusterIP, reachable only inside the Kubernetes cluster.
For a first look from your machine, create a NodePort Service of your own next to it, selecting
the NiFi pods (on a real cluster, use a LoadBalancer or your own Ingress, HTTPRoute or OpenShift
Route instead; see [Exposure](../exposure.md)):

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: nifi-ui
  namespace: nifi
spec:
  type: NodePort
  selector:                     # the NiFi pods of the NiFiCluster named "nifi"
    app.kubernetes.io/instance: nifi
    app.kubernetes.io/name: nifi
  sessionAffinity: ClientIP     # keeps a browser on one NiFi node, or logins fail now and then
  ports:
    - name: https
      port: 8443
      targetPort: 8443
EOF
```

Find a node address and the port Kubernetes assigned:

```sh
kubectl get nodes -o wide                                   # INTERNAL-IP or EXTERNAL-IP of any node
kubectl -n nifi get svc nifi-ui -o jsonpath='{.spec.ports[0].nodePort}'; echo
```

NiFi only answers on the addresses it knows, and its certificate must name them. Add the address
you will use, as `host:port`, to `spec.proxyHosts` (here `192.0.2.10:31739`):

```sh
kubectl -n nifi patch nificlusters.nifi.ksolves.com nifi --type merge \
  -p '{"spec":{"proxyHosts":["192.0.2.10:31739"]}}'
kubectl -n nifi wait --for=condition=ClusterHealthy nificlusters.nifi.ksolves.com/nifi --timeout=15m
```

The pods restart once, with certificates that include the new address. Open
**https://192.0.2.10:31739/nifi/** and log in. The browser warns about the certificate because it
comes from the cluster's own certificate authority; accept it.

`kubectl port-forward` does not work: NiFi listens on its pod address, not on the loopback address
the port-forward connects to. In production, use a LoadBalancer, or your own OpenShift Route, Ingress or
HTTPRoute, with a DNS name in `proxyHosts` ([Exposure](../exposure.md)); with more than one node the
browser must stay on one node ([Session affinity](../exposure.md#session-affinity-is-required)).

## 5. Scale the cluster

```sh
kubectl -n nifi patch nificlusters.nifi.ksolves.com nifi --type json \
  -p '[{"op":"replace","path":"/spec/nodeGroups/0/replicas","value":2}]'
kubectl -n nifi get pods -w
```

The new node joins the NiFi cluster by itself; the UI lists it under Cluster. Scaling down offloads
a node's data to the others before its pod is removed ([Scaling](../scaling.md)).

## 6. Clean up

```sh
kubectl -n nifi delete nificlusters.nifi.ksolves.com nifi
kubectl -n nifi delete service nifi-ui
kubectl delete namespace nifi
```

The persistent volumes stay after the NiFiCluster is deleted, unless
`persistentVolumeClaimRetentionPolicy` says otherwise ([Storage](../storage.md)); deleting the
namespace removes them.

## What's next

- [Authentication](../authentication.md): your own login Secret.
- [Cluster coordination](../cluster-coordination.md): Kubernetes Leases, and the supported NiFi
  versions ([Compatibility](../compatibility.md)).
- [Storage](../storage.md) and [TLS](../tls.md).
- [Docs index](../README.md) for everything else.
