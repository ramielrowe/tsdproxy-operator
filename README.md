# tsdproxy-operator

A Kubernetes operator for [tsdproxy](https://github.com/almeidapaulopt/tsdproxy).
It runs one tsdproxy instance per tailnet and exposes cluster workloads on
those tailnets from custom resources.

- **`TSDProxy`** is one tsdproxy instance joined to one tailnet.
- **`Proxy`** is one hostname on that tailnet, pointing at a Service or a URL.

The operator works with stock tsdproxy v3 images. tsdproxy has no Kubernetes
integration of its own, so the operator drives it through its file-based
configuration.

## How it works

For every `TSDProxy` the operator creates a StatefulSet with a single pod, a
Service, a Secret holding a generated API key, and a ConfigMap with two files:

| File | Contents | When it changes |
|---|---|---|
| `proxies.yaml` | One entry per attached `Proxy` | tsdproxy reloads it live and only restarts the proxies whose entry changed. The pod keeps running. |
| `tsdproxy.yaml` | Tailnet credentials, tags, mode, logging | tsdproxy reads it once at startup, so the operator rolls the pod. |

Creating, changing or deleting a `Proxy` therefore never restarts tsdproxy.
Changing the `TSDProxy` itself, or the Secret with its credentials, does.

After updating the proxy list the operator also touches an annotation on the
pod. That makes the kubelet refresh the mounted ConfigMap immediately instead
of on its periodic sync; in the end-to-end test tsdproxy loads a new proxy
within a few seconds of the `Proxy` being created.

Tailscale credentials never enter the ConfigMap. They are mounted from the
Secret you reference.

## Quick start

Install the CRDs and the operator:

```sh
make install
make deploy IMG=ghcr.io/ramielrowe/tsdproxy-operator:main
```

Images for amd64 and arm64 are published to
`ghcr.io/ramielrowe/tsdproxy-operator` by the `Publish Image` workflow, once
lint, the tests and the e2e suite pass for that commit:
`main` and `sha-<commit>` for every push to `main`, and `<version>`,
`<major>.<minor>` and `latest` for a `v<version>` tag. While the package is
private, the operator's namespace needs an image pull secret for ghcr.io.

Create an instance. With an OAuth client, tsdproxy mints a one-time auth key
for each device; the client needs the `auth_keys` scope and must own the tags.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: tailscale-oauth
  namespace: tailnet-home
stringData:
  client_id: "..."
  client_secret: "..."
---
apiVersion: tsdproxy-operator.apmelton.com/v1alpha1
kind: TSDProxy
metadata:
  name: home
  namespace: tailnet-home
spec:
  tailscale:
    oauth:
      secretName: tailscale-oauth
    tags: ["tag:k8s"]
  allowedNamespaces:
    from: Selector
    selector:
      matchLabels:
        tailnet: home
```

Expose a Service from a namespace labelled `tailnet=home`:

```yaml
apiVersion: tsdproxy-operator.apmelton.com/v1alpha1
kind: Proxy
metadata:
  name: jellyfin
  namespace: media
spec:
  instanceRef:
    name: home
    namespace: tailnet-home
  ports:
    - port: 443
      target:
        service:
          name: jellyfin
          port: 8096
```

```console
$ kubectl get proxy -n media
NAME       INSTANCE   HOSTNAME   ACCEPTED   STATUS    URL                                AGE
jellyfin   home       jellyfin   True       Running   https://jellyfin.example.ts.net    1m
```

More examples are in [`config/samples`](config/samples).

## TSDProxy

| Field | Purpose |
|---|---|
| `tailscale.authKey` | Secret key holding an auth key. Must be reusable in `PerProxy` mode. Exactly one of `authKey` and `oauth` is required. |
| `tailscale.oauth` | Secret holding an OAuth client (`client_id` and `client_secret` keys by default). Requires `tags`. |
| `tailscale.tags` | Tags for every device; a `Proxy` can override them. |
| `tailscale.mode` | `PerProxy` (default): one tailnet device per `Proxy`. `Services`: one device for the instance and a Tailscale VIP Service per `Proxy`; requires `oauth` and `hostname`, and has no UDP. |
| `tailscale.ephemeral` | Default for registering proxy devices as ephemeral nodes. |
| `tailscale.preventDuplicates` | Delete a stale device with the same hostname before registering (OAuth only). |
| `tailscale.controlURL` | Alternative control server, for example Headscale. |
| `allowedNamespaces` | Which namespaces may attach Proxies: `Same` (default), `All` or `Selector`. |
| `storage` | Data directory: a `PersistentVolumeClaim` (default, 1Gi) or an `EmptyDir`. Immutable. |
| `log`, `admins`, `image`, `resources`, `nodeSelector`, `tolerations` | Logging, dashboard admins and pod settings. |

The Secret with the credentials must be in the `TSDProxy`'s namespace.

## Proxy

| Field | Purpose |
|---|---|
| `instanceRef` | The `TSDProxy` to attach to. `namespace` defaults to the Proxy's own. |
| `hostname` | Tailnet hostname. Defaults to the Proxy's name. |
| `ports[]` | `port` and `protocol` (`https`, `http`, `tcp`, `udp`) on the tailnet, and a `target`: a `service` in the Proxy's namespace or a `url`. Optional `redirect`, `funnel` and `insecureSkipTLSVerify`. |
| `tailscale` | Per-proxy `tags`, `ephemeral`, `runWebClient`, `verbose`. |
| `dashboard` | `label`, `icon` and `category` in the tsdproxy dashboard. |

A Proxy reports two conditions:

- **Accepted**: the Proxy is rendered into its instance's proxy list. `False`
  reasons are `InstanceNotFound`, `NamespaceNotAllowed`, `InvalidHostname`,
  `HostnameConflict` and `Unsupported`.
- **Ready**: tsdproxy reports the proxy as running. Otherwise the reason is
  tsdproxy's own state (such as `Authenticating`), or `Pending`, `NotServing`,
  `InstanceNotReady` or `StatusUnavailable`.

Hostnames are unique per instance. When two Proxies ask for the same one, the
older Proxy keeps it and the other is rejected with `HostnameConflict`; it
takes over when the holder is deleted.

## Things to know

These follow from how stock tsdproxy behaves.

- **A proxy that fails to start disappears from tsdproxy.** When Tailscale
  rejects the auth key or a tag, tsdproxy logs the error and drops the proxy,
  and its API no longer lists it. The Proxy then reports `Ready=False` with
  reason `NotServing` and a pointer to the pod's logs. tsdproxy tries again
  when the Proxy changes or the pod restarts.
- **Deleted proxies leave devices behind.** tsdproxy does not remove a device
  from the tailnet when its proxy goes away. Use `tailscale.ephemeral: true`,
  or `preventDuplicates` with OAuth, if stale devices are a problem.
- **One pod per instance.** Device state lives in the data directory, so an
  instance cannot be scaled out.
- **The PersistentVolumeClaim outlives the instance.** Deleting a `TSDProxy`
  keeps the claim with the device identities; delete it yourself if you do
  not want them back.
- **The API key gives admin access to the instance's dashboard and API.** It
  is stored in the Secret `tsdproxy-<name>-api`. The dashboard's localhost
  bypass stays disabled, because tsdproxy would extend it to the whole pod
  network.
- **Not supported yet**: tsdproxy's shared mode and custom domains (DNS and
  ACME providers), port ranges, and creating proxies from Service annotations
  or Ingresses.

## Development

```sh
make test        # unit tests and controller tests against envtest
make lint
make test-e2e    # creates a kind cluster, runs the operator and tsdproxy in it
make run         # run the operator from your machine against the current cluster
```

Two of the tests check the operator against tsdproxy itself:

- `internal/render` contains a compatibility test that feeds the rendered
  files to tsdproxy's own configuration loader, validator and list provider.
  The tsdproxy version is pinned in `internal/render/testdata/tsdproxy/go.mod`
  and downloaded by the go command, so the test needs no checkout; `-short`
  skips it. Set `TSDPROXY_SRC` to a tsdproxy checkout to test against that
  instead.
- `make test-e2e` runs the stock tsdproxy image with a fake auth key. It
  verifies that tsdproxy accepts the configuration, reloads the proxy list
  without a restart, and restarts for a main configuration change. It does
  not join a tailnet.

When run with `make run`, the operator cannot reach the instances' Services,
so Proxies report `StatusUnavailable` instead of tsdproxy's state.

See [AGENTS.md](AGENTS.md) for the project layout and conventions.

## License

Apache License 2.0. See [LICENSE](LICENSE).
