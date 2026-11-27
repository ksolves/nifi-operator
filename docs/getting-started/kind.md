# Try it on your laptop (kind)

`kind` runs a small Kubernetes cluster inside Docker on your machine. A script prepares it. Then
**you** install the operator and create a NiFi cluster, the same way you would on any Kubernetes.
About 45 minutes.

## What you need

- **Linux (x86_64)**, or macOS on Intel. The NiFi images are built for x86_64 only; on Apple
  Silicon Macs they run emulated and slowly.
- At least **4 GB of memory free** (6 GB for 2 NiFi nodes) and 15 GB of disk.

You do not need to install anything else first. `hack/try/kind.sh` installs what is missing:

| Tool | What it is | How the script installs it |
|---|---|---|
| Docker (with containerd) | Runs the containers; kind's "machines" are Docker containers | Linux: Docker Engine from get.docker.com, after asking (needs sudo). macOS: tells you how to install Docker Desktop. |
| kind | Creates the Kubernetes cluster inside Docker | Pinned version (v0.29; older ones are replaced), checksum verified, into `~/.local/bin` |
| kubectl | The Kubernetes command line | Linux: copied from the Kubernetes node image (no download). macOS: pinned version, checksum verified |
| helm | Installs packaged applications (here: the operator) | Same |

## 1. Prepare the cluster (10 min)

```sh
git clone https://github.com/ksolves/nifi-operator.git
cd nifi-operator
hack/try/kind.sh
```

The script:

1. checks your machine and installs the missing tools;
2. creates a one-node Kubernetes cluster called `nifi-try`, with port 8443 on your machine
   forwarded into it for the NiFi UI;
3. loads the operator images (public, `ksoperator/nifi-operator`) and NiFi (`apache/nifi`) into it.

When it is done, `kubectl get nodes` shows one `Ready` node. If the script installed tools into
`~/.local/bin`, add that to your PATH as it tells you.

## 2. Install the operator (5 min)

The operator gets its own namespace. Its certificate helper (a CSI driver) mounts host directories,
which Kubernetes only allows in a namespace marked `privileged`:

```sh
kubectl create namespace nifi-operator
kubectl label namespace nifi-operator pod-security.kubernetes.io/enforce=privileged
helm install nifi-operator charts/nifi-operator -n nifi-operator --wait
kubectl -n nifi-operator get pods
```

Expect 2 `nifi-operator-controller-manager` pods (`1/1`) and 1 `nifi-operator-csi-driver` pod
(`3/3`), all `Running`. That one Helm command is the whole install.

## 3. Create a NiFi cluster (5 min)

NiFi runs in its own namespace, with the strictest pod security setting. NiFi passes it.

```sh
kubectl create namespace nifi
kubectl label namespace nifi pod-security.kubernetes.io/enforce=restricted
cp docs/getting-started/nifi-kind.yaml my-nifi.yaml
cat my-nifi.yaml               # this short file is the whole NiFi configuration
kubectl apply -f my-nifi.yaml
kubectl -n nifi get pods -w    # Ctrl-C when nifi-default-0 shows 1/1 Running (2-4 min)
```

Then ask the operator whether NiFi is healthy:

```sh
kubectl -n nifi get nificlusters.nifi.ksolves.com nifi -o wide    # HEALTHY: True
```

## 4. Open the UI (2 min)

Make NiFi reachable from your browser. On kind this takes one extra Service; on a real cluster
it would be a load balancer or a Route (read the comment in the file):

```sh
kubectl apply -f docs/getting-started/nifi-ui-service.yaml
```

The operator generated a login for you:

```sh
kubectl -n nifi get secret nifi-auth-credentials -o jsonpath='{.data.password}' | base64 --decode; echo
```

Open **https://localhost:8443/nifi/**, exactly this address (`localhost`, not `127.0.0.1`), and
log in as `admin` with that password. The browser warns about the certificate, because it comes from
the NiFi cluster's own certificate authority. Accept it.

## 5. Things to try

Your NiFi configuration is one file, `my-nifi.yaml`. To change the cluster, edit it and run
`kubectl apply -f my-nifi.yaml`. On a laptop, use `replicas: 2` at most: each NiFi node needs about
2 GB.

1. **Build a flow.** In the UI, add GenerateFlowFile → LogAttribute, connect them and start them.
2. **Look at what the operator made.**

   ```sh
   kubectl -n nifi get nificlusters.nifi.ksolves.com -o wide
   kubectl -n nifi get pods,pvc,svc,secrets
   kubectl -n nifi describe nificlusters.nifi.ksolves.com nifi    # Conditions and Events at the end
   ```

3. **Change a NiFi setting.** Add this under `spec:` and apply:

   ```yaml
     configOverrides:
       nifiProperties:
         nifi.ui.banner.text: "Hello from kind"
   ```

   The nodes restart one at a time and the banner shows in the UI. The flow keeps its state.
4. **Grow to a cluster.** Change `replicas: 1` to `2` and apply. The new node joins by itself; the
   NiFi UI shows it under Cluster.
5. **Break a node.** `kubectl -n nifi delete pod nifi-default-0`. It comes back with the same disk
   and rejoins the cluster.
6. **Make a mistake on purpose.** Set `storage: 1Gi` (smaller than now), or misspell a field, and
   apply. The API server or the operator's webhook refuses it with a message, before anything
   breaks.
7. **Delete and recreate.** `kubectl -n nifi delete nificlusters.nifi.ksolves.com nifi` removes
   everything except the disks, which stay by default. Apply `my-nifi.yaml` again and the flow
   comes back. The login password is new: read it again from the Secret `nifi-auth-credentials`.

## 6. Remove everything

```sh
hack/try/kind.sh delete
```

This deletes the kind cluster and everything in it. The tools stay installed.

## If something goes wrong

| Symptom | Fix |
|---|---|
| `cannot pull ...` in the script | Check the internet connection (a proxy may block Docker Hub), then run the script again. |
| `permission denied ... docker.sock` | The script added you to the `docker` group; log out and back in, then run it again. |
| `kubectl: command not found` after the script | Add `~/.local/bin` to your PATH: `export PATH="$HOME/.local/bin:$PATH"`. |
| `helm install` fails with `cannot re-use a name` | It is already installed; check `kubectl -n nifi-operator get pods`. |
| Pod stays `Pending` | Too little memory. Check `kubectl -n nifi describe pod nifi-default-0`, then lower `replicas` or close other programs. |
| Browser: "connection refused" | Run step 4's `kubectl apply` again. If port 8443 was busy when the cluster was created: `hack/try/kind.sh delete`, then `UI_PORT=9443 hack/try/kind.sh`, and use `localhost:9443` in `my-nifi.yaml` (`proxyHosts`) and in the URL. |
| Browser: "Invalid host" or 421 | Use exactly `https://localhost:8443/nifi/`. |
| Login fails right after start | Wait a minute and reload: NiFi is still joining its cluster. |
| You want to start over | `hack/try/kind.sh delete`, then start again at step 1. |
