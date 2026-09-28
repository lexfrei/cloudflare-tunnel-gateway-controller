# Upgrading from v3 to v4

v4 tightens defaults that were wrong in v3 and turns on TLS for the controller-to-proxy config API. There is no CR migration. Each section headed "Breaking" describes something that works on v3 and can stop after the upgrade, names who it reaches, and gives the change that keeps it working. Read them all before running `helm upgrade`, then follow the [upgrade procedure](#upgrade-procedure) at the end.

## Breaking: Kubernetes 1.31 or later

The chart's `kubeVersion` constraint is now `>=1.31.0-0`, the floor of the Gateway API v1.6 bundle the controller is built against. Helm refuses to install or upgrade the chart on an older cluster, so upgrade Kubernetes first. [Prerequisites](../getting-started/prerequisites.md#compatibility) explains where the floor comes from.

## Breaking: config API TLS is on by default

The config API is where the controller pushes the routing table to each data plane. It carries the bearer token and, for routes whose Gateway configures backend mTLS, the client certificate's private key. In v3 it was plain HTTP. In v4 it is TLS by default, for the shared plane and for every per-Gateway plane (`proxy.configAPITLS.enabled: true`).

What the chart and the controller now do:

- The controller creates its own CA at runtime in the Secret `<fullname>-config-ca` in the release namespace, and issues each data plane a serving certificate from it.
- The shared proxy mounts `<fullname>-proxy-config-tls`, which the controller creates. A new namespaced Role grants the controller `update` on that one Secret.
- The config API port serves the proxy's probes and `/metrics` as well, so all three now speak HTTPS.

[Config API TLS](../reference/security.md#config-api-tls) describes the certificates, renewal and CA rotation.

### Who is affected

- **A scrape config or monitor you wrote yourself** for the proxy's `/metrics`. A Prometheus `scrape_configs` job needs `scheme: https` and `tls_config.insecure_skip_verify: true`, as in the [scrape config example](../operations/metrics.md#scrape-config-prometheus). A PodMonitor or ServiceMonitor, such as one for per-Gateway planes, needs `scheme: https` and `tlsConfig.insecureSkipVerify: true` on the endpoint. The chart's own proxy ServiceMonitor already sets both.
- **A proxy image pinned to v3.** A pre-v4 proxy ignores the mounted certificate and serves plain HTTP, so its HTTPS probes fail and the plane never becomes ready. `proxy.image.tag` or `proxy.image.digest` in your values sets the shared proxy's image and also `--proxy-image`, the default image for every per-Gateway plane, so a pinned v3 value keeps both on v3. `GatewayConfig.spec.image` pins one per-Gateway plane. Move them to v4, or keep the plaintext config API with the value below.
- **A controller image pinned to v3** with `image.tag` or `image.digest`. The v4 chart passes the controller the config API TLS flags, which a v3 binary does not know, so it exits at startup. Move it to v4.
- **Anything that talks to a proxy's config API directly** over `http://`.

The controller's own ports are not part of this change: its `/metrics` on 8080 and its health endpoints on 8081 stay plain HTTP.

### To keep the plaintext config API

```yaml
proxy:
  configAPITLS:
    enabled: false
```

### Secrets left behind on uninstall

`<fullname>-config-ca`, which holds the CA private key, and `<fullname>-proxy-config-tls` stay after `helm uninstall` on purpose, so a reinstall under the same release name reuses the CA. [Uninstalling](../getting-started/installation.md#uninstalling) has the delete command for a full cleanup.

### Raw manifests

The `deploy/` manifests leave config API TLS off. [Manual installation](../operations/manual-installation.md) lists what to add to match the chart.

## Breaking: the controller NetworkPolicy admits no in-cluster source by default

This reaches only installs with `networkPolicy.enabled: true`. In v3 an empty `networkPolicy.ingress.from` rendered an ingress rule with no `from`, which admits every source to the controller's metrics and health ports. In v4 an empty list renders `ingress: []`, and nothing inside the cluster reaches those ports unless it is listed.

Add Prometheus:

```yaml
networkPolicy:
  enabled: true
  ingress:
    from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
```

Name the namespace your scraper actually runs in.

Kubelet probes come from the node, which most CNIs admit regardless of policy. On a CNI that applies policy to node-to-pod traffic, list the node source too, for example an `ipBlock` for the node CIDR. Without it the probes fail and the controller goes into CrashLoopBackOff.

## Breaking: the controller PodDisruptionBudget takes exactly one bound

This reaches only installs with `podDisruptionBudget.enabled: true`. The controller's budget now takes exactly one of its two bounds, and a bound written as `0` counts as set:

- Setting both `minAvailable` and `maxUnavailable` fails the render. v3 dropped a bound written as `0` and v4 counts it as set, so a zero on either side next to the other bound worked on v3 and fails now. That includes `maxUnavailable: 0` next to the chart's default `minAvailable: 1`. Any values file that sets `maxUnavailable` must set `minAvailable` to `null`:

    ```yaml
    podDisruptionBudget:
      enabled: true
      minAvailable: null
      maxUnavailable: 1
    ```

- A lone `minAvailable: 0` now renders. In v3 the zero was dropped from the budget.
- A lone `maxUnavailable: 0`, next to `minAvailable: null`, now renders as written. v3 dropped it and rendered a budget with neither bound. `maxUnavailable: 0` lets no voluntary eviction of healthy controller pods through, so `kubectl drain` on their nodes waits until the budget changes. If that is not what you want, set the bound you mean.

## Breaking: the TunnelShared reason within one namespace

The `cf.k8s.lex.la/TunnelShared` route condition marks a per-Gateway data plane that shares a Cloudflare Tunnel with another dedicated Gateway. When every Gateway on that tunnel is in the route's Gateway's namespace, the reason is now `TunnelSharedWithinNamespace`. In v3 it was `TunnelSharedAcrossNamespaces` in both cases; that reason now means some Gateway on the tunnel is in another namespace.

An alert that matches `TunnelSharedAcrossNamespaces` stops firing for sharing within one namespace. Match the new reason as well, or match on the condition type `cf.k8s.lex.la/TunnelShared` alone.

## Breaking: GatewayClasses in use must agree on parametersRef

One controller serves one GatewayClassConfig. When several GatewayClasses name this controller and the ones that Gateways use carry different `parametersRef` (or one of them has none):

- every Gateway on those classes reports `Accepted=False` with reason `InvalidParameters` and a message naming the classes;
- no new per-Gateway data plane is rendered;
- running planes are frozen: a `GatewayConfig` edit, an image change or a token-rotation rollout does not reach them until the classes agree;
- Gateways on the shared plane keep their address, so DNS records published from it stay in place.

Route sync already programmed nothing in that setup in v3. New in v4 are the Gateway status, that no new per-Gateway plane is rendered, that running planes are frozen, and that a class no Gateway uses is ignored while another class is in use. [GatewayClassConfig](../configuration/gatewayclassconfig.md) has the full rule.

Before upgrading, list the classes that name this controller:

```bash
kubectl get gatewayclasses \
  --output custom-columns='NAME:.metadata.name,CONTROLLER:.spec.controllerName,KIND:.spec.parametersRef.kind,PARAMS:.spec.parametersRef.name'
```

If more than one row carries your `controllerName` (the chart default is `cf.k8s.lex.la/tunnel-controller`) and they differ in `parametersRef`, delete the classes you do not use, or point them at the same GatewayClassConfig.

## Breaking: standalone proxy mode binds loopback

This reaches only a proxy run without `TUNNEL_TOKEN`, the development mode. Standalone mode now listens on `127.0.0.1` only, for both the config API and the proxy port. To restore the old bind:

```bash
export PROXY_CONFIG_ADDR=:8081 PROXY_ADDR=:8080
```

Standalone mode runs the config API unauthenticated by default, so the network boundary in front of a widened bind is yours to provide. Tunnel mode and chart installs are unaffected.

## Breaking: PROXY_TUNNEL_PROTOCOL_WAIT is gone

This reaches only a proxy Deployment you wrote by hand: the chart never set this variable. In v3, with `PROXY_TUNNEL_PROTOCOL` at `auto`, it bounded how long the proxy waited for its first config before dialing the edge. v4 does not read it, because every tunnel-mode proxy now waits for its first config, as the next section describes. Remove it from your manifests. A value left behind has no effect.

## Breaking: a WebSocket timeout written as zero fails the upgrade

The chart schema now rejects `proxy.websocket.dialTimeout`, `proxy.websocket.handshakeTimeout` and `proxy.websocket.idleTimeout` when the value is a duration written as zero, such as `0s`. A values file that sets one makes `helm upgrade` fail validation before anything renders, and so do values carried over with `--reuse-values`.

`idleTimeout` is new in v4. For the other two the proxy never honoured a zero: it replaced it with the 30-second default. Clear the value (`""`) to keep the behaviour you had, or set the duration you actually want.

## Breaking: an idle WebSocket session is closed after an hour

An established WebSocket session is now closed once it has carried no bytes in either direction for `proxy.websocket.idleTimeout`, one hour by default. Any bytes either way reset the window, so an application that sends its own WebSocket pings keeps its sessions open. Keepalives below the WebSocket layer, at the edge or in the transport, do not count.

A session that sat silent for longer than an hour stayed open on v3 and is cut on v4. If your application holds quiet sessions longer than that, raise the value:

```yaml
proxy:
  websocket:
    idleTimeout: 6h
```

Per-Gateway data planes take the same value.

## Breaking: the WebSocket upgrade sends the client Host

The upgrade request to a WebSocket backend (`appProtocol: kubernetes.io/ws` or `kubernetes.io/wss`) now carries the same `Host` as a plain HTTP request to that backend: the client's `Host`, or the hostname of a `URLRewrite` filter on the rule. A WebSocket backend that expected the Service address in `Host` now sees the client's hostname.

Make the backend accept the client hostname, or add a `URLRewrite` filter with the `hostname` the backend expects.

## Breaking: Cloudflare must confirm a per-Gateway tunnel claim

This reaches only per-Gateway data planes (`Gateway.spec.infrastructure.parametersRef` pointing at a `GatewayConfig`). Before a dedicated Gateway is accepted, the controller now asks the Cloudflare API for the tunnel's own connector token and checks that the Gateway's token carries the same account, tunnel and secret. It asks with the credential that writes the tunnel's configuration: the `GatewayConfig`'s `cloudflareCredentialsSecretRef` when set, the GatewayClass credentials otherwise. That needs no permission beyond the one the controller already requires.

A claim Cloudflare does not confirm is refused: `Accepted=False` with reason `InvalidParameters`, a `TunnelClaimRejected` Warning Event, no routes programmed and no data plane. `allowSharedTunnels: true` on the GatewayClassConfig no longer skips this check; it still waives only the contest between namespaces.

Affected: a dedicated Gateway whose token names a tunnel that its credential cannot confirm, for example a tunnel in a different Cloudflare account from that credential, or a token whose secret no longer matches the tunnel. Give it the tunnel's current token, or a `cloudflareCredentialsSecretRef` for the account that holds the tunnel. [Proving a tunnel claim](../guides/per-gateway-isolation.md#proving-a-tunnel-claim) covers outages and caching.

## Behaviour change: a proxy takes traffic only after its first config

In tunnel mode the proxy now registers with the Cloudflare edge only after the controller has pushed it its first config, whatever `proxy.tunnel.protocol` is set to. On v3 a new pod could register while it held no routes, and answer 404 (`Unimplemented` for gRPC) for the few seconds until its config arrived. During a rolling upgrade that showed up as a short outage of every route.

When a proxy pod joins, the controller replays to it the latest config that at least one replica accepted, even while an old replica is still refusing pushes. A proxy that receives no config within two minutes exits instead of registering, and kubelet restarts it. So while the controller is down, or cannot reach a new proxy pod, that pod exits every two minutes and ends up in `CrashLoopBackOff` instead of sitting `NotReady`. Old pods keep serving meanwhile. The pod recovers on its own once the controller pushes to it. Kubelet's restart back-off grows with each restart, so a pod that crash-looped while the controller was down can take up to about five more minutes to recover after the controller is back. [Proxy Pod Restarting Without a Config](../operations/troubleshooting.md#proxy-pod-restarting-without-a-config) covers finding what stops the push.

## Behaviour change: a route with an unevaluable parent is narrowed or left out

When a route's parent Gateway or ListenerSet exists but cannot be evaluated (for example it cannot be read, or binding validation against it returns an error), that parent now lends the route no hostname. The route serves only the hostnames its other parents lend, and a route that no other parent lends a hostname to is left out of the proxy config. In v3 it was served with the hostnames it declares, or under every Host when it declares none.

A route that loses hostnames this way gets `cf.k8s.lex.la/ProxyConfigPushed=False` with reason `ParentNotEvaluated`, and the controller retries the sync every 15 seconds until the parent can be evaluated. When the route's other parents already serve every hostname it declares, the controller only logs the unevaluable parent. [Limitations](../gateway-api/limitations.md#routes-attached-to-another-implementations-gateway) lists the causes.

## Behaviour change: same-named HTTPRoute and GRPCRoute keep separate status

An HTTPRoute and a GRPCRoute with the same name in the same namespace no longer share status conditions or Events. In v3 a problem found on one of them was also reported on the other.

## Behaviour change: the controller reports CRD fields the cluster lacks

At startup the controller logs an error for each field that the installed `GatewayClassConfig`, `GatewayConfig` or `ExternalBackend` CRD does not declare. The API server drops such a field on write, so a setting there is not in force. If you see one, re-apply the chart CRDs as described in [CRD upgrades](index.md#crd-upgrades).

## Behaviour change: no proxy roll right after a fresh install

A fresh install no longer rolls the shared proxy once after the controller starts.

## Behaviour change: an egress rule for the tracing collector

With `networkPolicy.enabled` and controller tracing on, a `controller.tracing.endpoint` that names an in-cluster Service with an explicit port gets an egress rule for that Service's namespace on that port. Other endpoints get no rule: use a policy of your own, or the Kubernetes API rule on 443 while it is unrestricted. [Tracing](../operations/tracing.md#network-policies) has the details.

## Behaviour change: a trailing slash on a PathPrefix is ignored

A trailing slash on an HTTPRoute `PathPrefix` value is now ignored, as the Gateway API specifies: `/abc` and `/abc/` are the same prefix, and either one matches `/abc`, `/abc/` and `/abc/def`. Two rules whose prefixes differ only by that slash now tie on path length, and the remaining precedence rules decide between them. `ReplacePrefixMatch` in a redirect on an `Exact` or `RegularExpression` match now leaves the path unchanged; it used to replace the whole path.

## Behaviour change: ReplacePrefixMatch keeps a trailing slash

A `ReplacePrefixMatch` rewrite or redirect now keeps a trailing slash on the request path.

## Behaviour change: an upgrade request to a non-WebSocket backend is plain HTTP

A request that asks for a protocol upgrade, to a backend without `appProtocol: kubernetes.io/ws` or `kubernetes.io/wss`, is now forwarded without its `Upgrade` header, so the backend answers it as a plain HTTP request. A backend that answers `101` anyway gets the client a 502. Set the `appProtocol` on the Service port of any backend that serves WebSocket.

## Behaviour change: another implementation's Gateway lends a route nothing

A route attached both to this controller's Gateway and to a Gateway of another Gateway API implementation no longer takes that other Gateway's listener hostnames, and a scheme-less redirect no longer takes `https` from its HTTPS listener. [Routes attached to another implementation's Gateway](../gateway-api/limitations.md#routes-attached-to-another-implementations-gateway) has the details.

## Behaviour change: an abandoned per-Gateway tunnel is emptied

When no Gateway serves a per-Gateway tunnel any more, because its Gateway was deleted, opted out, was refused or switched to another token, the next route sync writes that tunnel an ingress document holding only the catch-all rule. On v3 the old document stayed. The GatewayClass tunnel is never emptied.

## Behaviour change: a non-default configAPIPort reaches per-Gateway planes

`proxy.configAPIPort` now applies to per-Gateway data planes too: their listen address, container and Service ports, NetworkPolicy and push URL follow it. On v3 they kept 8081. With a value other than 8081, every per-Gateway plane rolls onto the new port once the v4 controller renders it.

## Behaviour change: the h2c response timeout starts after the upload

For an h2c backend, a rule's `timeouts.request` or `timeouts.backendRequest` now starts counting once the request body has been sent, as it already did for HTTP/1.1 and TLS backends. A slow upload or a gRPC client stream to an h2c backend is no longer cut off before it finishes.

## Upgrade procedure

1. **Upgrade Kubernetes to 1.31 or later.** The chart refuses an older cluster. A client-side render, such as `helm template` or a GitOps tool that renders the chart itself, checks the constraint against the version passed with `--kube-version` or against its own built-in default, not against the cluster, so pass the target cluster's version there.
2. **Move to a Gateway API v1.6.x bundle first, on the channel you already run.** The v4 controller reports `SupportedVersion=False` with reason `UnsupportedVersion` on the GatewayClass when the installed bundle has any other minor version, which is where an install that started on v3.0 or v3.1 usually is. Check the installed bundle and its channel:

    ```bash
    kubectl get crd gateways.gateway.networking.k8s.io \
      --output jsonpath='{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version} {.metadata.annotations.gateway\.networking\.k8s\.io/channel}{"\n"}'
    ```

    Apply the v1.6.x `standard-install.yaml` for the standard channel, or `experimental-install.yaml` for the experimental one. [Prerequisites](../getting-started/prerequisites.md#gateway-api-crds) has the standard command. Do not switch channels on the way. The v1.6 standard bundle installs the `safe-upgrades.gateway.networking.k8s.io` ValidatingAdmissionPolicy, which refuses experimental CRDs over standard ones, and applying standard CRDs over experimental ones drops the experimental fields from the schema.

3. **Edit your values** for each breaking section that applies: `networkPolicy.ingress.from`, the controller `podDisruptionBudget`, `image` and `proxy.image` if they pin a v3 tag or digest, `proxy.websocket` timeouts written as zero, `proxy.websocket.idleTimeout` if sessions stay silent for more than an hour, and `proxy.configAPITLS.enabled` if you keep the plaintext config API.
4. **Fix what lives outside the chart:** your own proxy scrape configs and PodMonitors (HTTPS without certificate verification), WebSocket backends that expect the Service address in `Host`, any `GatewayConfig.spec.image` pinned to a pre-v4 proxy image, GatewayClasses that disagree on `parametersRef`, and alerts on the `TunnelShared` reason.
5. **Re-apply the chart CRDs.** `helm upgrade` never updates them. Since v3.5 the CRDs changed only in field descriptions, but a cluster that last applied them before v3.5 is missing fields that v3.5 added. The command is in [CRD upgrades](index.md#crd-upgrades). Helm created these CRDs, so `kubectl apply` warns that each one is missing the `kubectl.kubernetes.io/last-applied-configuration` annotation. The warning is harmless: kubectl adds the annotation and applies the change.
6. **Run the upgrade** with `--reset-then-reuse-values`, or with your full values file. Either one renders the v4 chart defaults under your overrides:

    ```bash
    helm upgrade cloudflare-tunnel-gateway-controller \
      oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
      --namespace cloudflare-tunnel-system \
      --reset-then-reuse-values \
      --values values.yaml
    ```

    `--reuse-values` also renders from any v3 chart, and turns config API TLS on as the default does. It renders against the values of the chart you last installed, though, so every default the v4 chart added or changed is skipped.

7. **Avoid route changes until the rollout finishes.** While it runs, old and new controller and proxy pods cannot push to each other: an old controller pushes plain HTTP and a new proxy serves TLS, and the reverse. Traffic keeps flowing on the last applied config: old pods keep serving it, and new pods get it within seconds of starting and only then take traffic. A route edit made during the rollout reaches some data plane pods within seconds and the rest only as they finish rolling, so until every plane has rolled, requests for that route are served by a mix of the old and the new config.

### What to watch during the rollout

- New shared proxy pods wait in `ContainerCreating` until the new controller has created `<fullname>-proxy-config-tls`, then stay `NotReady` until it pushes to them. They register with the edge only after that push, so they take no traffic before they hold routes. That is expected and clears on its own.
- Per-Gateway data planes roll onto the TLS pod template once the new controller renders them.
- The controller log: an error about a CRD field the installed schema lacks means step 5 did not reach that CRD.
- `ProxyConfigPushFailed` Warning Events on routes while old and new pods run side by side. A burst of them is expected in that window. Their text says matching requests are served 502, which overstates it: the data plane they name keeps serving the config it last received.
- Route status: a sustained `cf.k8s.lex.la/ProxyConfigPushed=False` after the rollout finishes points at a push the TLS change broke, for example a plane on a pre-v4 image. The condition names the handshake failure.
- Gateway status: `Accepted=False` with `InvalidParameters` names conflicting GatewayClasses, or comes with a `TunnelClaimRejected` Event for a tunnel claim Cloudflare did not confirm.
- Prometheus targets: a controller or proxy target that went down points at `networkPolicy.ingress.from` or at a scrape config still on plain HTTP.

## What does not change

- No CR migration. Existing `GatewayClassConfig`, `GatewayConfig` and `ExternalBackend` objects keep working.
- The tunnel document format, and route status semantics outside the sections above.
