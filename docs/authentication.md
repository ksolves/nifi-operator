# NiFi login credentials

The operator always backs the NiFi UI/API login with a Kubernetes Secret, so every node in the
cluster shares one consistent login - never each node's own random, inconsistent credentials.
Which Secret is in use depends on `spec.authentication.singleUser`:

| `spec.authentication.singleUser` | Secret used | Who owns it |
|---|---|---|
| Not set at all | `<name>-auth-credentials`, auto-generated | Operator |
| `username`/`password` set | `<name>-auth-credentials`, synced from those fields | Operator |
| `secretName` set | the named Secret | You |

Check which Secret a cluster is actually using with:

```
kubectl get nificlusters.nifi.ksolves.com <name> -o wide
```

The `AUTH SECRET` column (also available as `status.authSecretName`) always names the Secret
currently in effect, regardless of which mode above is active.

None of this works without TLS: NiFi only issues a login token over HTTPS, so with
`tls.enabled: false` every request reaches NiFi as the anonymous user, regardless of which mode
above is configured. See [TLS](tls.md).

## Default: no `spec.authentication` set

If you don't configure authentication at all, the operator generates a login the first time the
cluster is created - username `admin`, a random password - and stores it in
`<name>-auth-credentials`. This only happens once: later changes to the cluster never rotate it.

Retrieve the generated credentials:

```
kubectl get secret <name>-auth-credentials -o jsonpath='{.data.username}' | base64 -d; echo
kubectl get secret <name>-auth-credentials -o jsonpath='{.data.password}' | base64 -d; echo
```

A sample CR for this mode: [`config/samples/nifi-auth-default.yaml`](../config/samples/nifi-auth-default.yaml).

Without this, leaving authentication unset would instead fall back to NiFi's own built-in
behavior: each node generates its *own* random credentials independently, so a multi-node
cluster's UI login differs from node to node and is only ever visible in that one pod's logs.

## Literal username/password

Set credentials directly in the CR - simple, but stored in plain text in the CR itself (visible
via `kubectl get nificluster -o yaml`, and in etcd). The operator keeps its own
`<name>-auth-credentials` Secret in sync with these fields; editing the password here rolls the
pods onto the new login. The restart is triggered by that Secret's UID/resourceVersion (the
`nifi.ksolves.com/auth-hash` pod annotation), never by a hash of the password itself.

**Upgrade note:** older operator versions put an unsalted hash of the password on that
annotation. Upgrading changes its value, so every literal-mode cluster gets one rolling restart
when the new operator first reconciles it. Plan the upgrade accordingly.

```yaml
spec:
  authentication:
    singleUser:
      username: admin
      password: at-least-12-characters
```

A sample CR: [`config/samples/nifi-auth-literal.yaml`](../config/samples/nifi-auth-literal.yaml).

## Bring your own Secret

Point at a Secret you manage yourself - e.g. synced from Vault, AWS Secrets Manager, or any other
external secret manager via External Secrets Operator. The operator never creates or modifies
this Secret; it only reads it, and rolls the pods if its content changes.

```yaml
spec:
  authentication:
    singleUser:
      secretName: my-login-secret   # must have "username" and "password" keys
```

A sample CR: [`config/samples/nifi-auth-secret.yaml`](../config/samples/nifi-auth-secret.yaml).
