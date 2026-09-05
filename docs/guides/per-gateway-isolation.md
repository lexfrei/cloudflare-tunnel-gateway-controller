# Per-Gateway Isolation

By default every Gateway of the class shares one chart-deployed proxy pool and one Cloudflare Tunnel. This guide covers the opt-in hard-isolation mode: a DEDICATED data plane — its own proxy Deployment and its own tunnel — rendered and reconciled by the controller for a single Gateway.

Use it when admission-level scoping (see [Multi-Tenancy](multi-tenancy.md)) is not enough: tenants that must not share a process, a tunnel identity, or a failure domain.

## What you get

- A dedicated proxy Deployment in the Gateway's namespace, running the same proxy image as the shared plane (chart-wired via `--proxy-image`, overridable per Gateway).
- A dedicated Cloudflare Tunnel: the Gateway's routes are written to ITS tunnel's ingress document and pushed to ITS proxy pods only. Routes of other Gateways never reach this data plane — and this Gateway's routes never reach theirs.
- Independent lifecycle: rendered resources are controller-owned (deleted with the Gateway, healed on drift), connector draining on shutdown, optional autoscaling on the proxy's in-flight gauge.
- A per-Gateway config-push credential: the controller authenticates config pushes to this plane with the Gateway's own token, never the shared plane's.

## Opting in

1. Create a Cloudflare Tunnel for the Gateway (one tunnel per isolated Gateway) and store its connector token in the Gateway's namespace:

    ```bash
    kubectl --context <ctx> --namespace tenant-a create secret generic edge-tunnel-token \
      --from-literal=tunnel-token=<connector-token>
    ```

2. Create a `GatewayConfig` next to the Gateway:

    ```yaml
    apiVersion: cf.k8s.lex.la/v1alpha1
    kind: GatewayConfig
    metadata:
      name: edge-config
      namespace: tenant-a
    spec:
      tunnelTokenSecretRef:
        name: edge-tunnel-token
      replicas: 2
    ```

3. Reference it from the Gateway:

    ```yaml
    apiVersion: gateway.networking.k8s.io/v1
    kind: Gateway
    metadata:
      name: edge
      namespace: tenant-a
    spec:
      gatewayClassName: cloudflare-tunnel
      infrastructure:
        parametersRef:
          group: cf.k8s.lex.la
          kind: GatewayConfig
          name: edge-config
      listeners:
        - name: https
          port: 443
          protocol: HTTPS
    ```

The controller renders `cf-proxy-edge` (Deployment) and `cf-proxy-edge-config` (headless Service) in `tenant-a`, parses the tunnel identity from the connector token (there is deliberately no separate `tunnelID` field — it cannot drift from the token), and starts syncing the Gateway's routes to that tunnel. The Gateway's status address becomes `<tunnel-id>.cfargotunnel.com`, and `Programmed` turns `True` only once the rendered Deployment has ready replicas — that is, registered tunnel connectors.

A Gateway without `infrastructure.parametersRef` keeps the shared data plane, unchanged. Removing the ref later deletes the rendered resources (only when actually owned by the Gateway) and returns the Gateway to the shared plane.

## GatewayConfig reference

| Field | Required | Meaning |
| --- | --- | --- |
| `tunnelTokenSecretRef` | yes | Connector-token Secret in the SAME namespace (key `tunnel-token` by default). Token rotation rolls the proxy pods automatically. |
| `cloudflareCredentialsSecretRef` | no | API-token override for this Gateway's tunnel-document writes, from a Secret in the SAME namespace (key `api-token` by default); defaults to the GatewayClass → GatewayClassConfig credentials. |
| `authTokenSecretRef` | no | Bearer token (key `auth-token`) protecting this plane's config API; the controller pushes with it. |
| `replicas` | no | Fixed replica count; default 2 (the HA floor — one connector pod is a tunnel availability hazard), max 100. Mutually exclusive with `autoscaling`. |
| `autoscaling` | no | Renders a HorizontalPodAutoscaler — see below. `minReplicas`/`maxReplicas` cap at 100. |
| `resources` | no | Proxy container resources; chart-parity defaults when unset. |
| `image` | no | Proxy image override; defaults to the release's proxy image. |

All Secret references are namespace-local by construction, so a Gateway cannot point at another tenant's credentials. That is narrower than it sounds: a tenant does not need another tenant's Secret to reach their tunnel, because the tunnel identity is parsed from the connector token and a token is just base64 JSON. Writing someone else's tunnel UUID into your own token is the same claim without touching their Secret, which is why the controller arbitrates tunnel ownership separately (see below).

`Gateway.spec.infrastructure.labels` and `.annotations` propagate to the rendered resources and the pod template; changing them rolls the pods.

## Autoscaling

```yaml
spec:
  autoscaling:
    minReplicas: 2
    maxReplicas: 10
    targetInflightPerPod: 50
```

The rendered `autoscaling/v2` HPA scales the proxy Deployment on `cftunnel_proxy_requests_in_flight` as a Pods-type custom metric — concurrency is the saturation signal for an I/O-bound L7 hop, not CPU. Serving Pods metrics to the HPA requires a metrics adapter (prometheus-adapter or KEDA) exposing the gauge through the custom-metrics API; without one the HPA reports `FailedGetPodsMetric` and holds `minReplicas` — visible degradation, never silent. See [Metrics & Alerting](../operations/metrics.md) for adapter examples.

Do not pair a VerticalPodAutoscaler in apply mode with these Deployments: applying VPA recommendations restarts pods, which drops tunnel connectors. Recommendation mode is fine.

## Sharing a tunnel

A Cloudflare Tunnel belongs to the Gateway already serving it, and the GatewayClass tunnel belongs to the operator. A Gateway whose connector token names a tunnel that another namespace's Gateway — or the GatewayClass itself — already serves is refused: it reports `Accepted=False` with reason `InvalidParameters`, emits a `TunnelClaimRejected` Warning Event, none of its routes are programmed anywhere, and **its data plane is not rendered at all**. Its routes say so too, so the tenant can see the cause without asking the operator.

The plane is removed rather than merely left unconfigured. That matters when both parties hold the SAME token — an operator handing one token to two Gateways — because both connectors then register and Cloudflare load-balances across them, so a configless survivor would answer a share of the incumbent's requests with 404s.

A forged claim is a different story, and the difference is worth knowing: connector registration authenticates on the account tag and the tunnel secret inside the token, not on the tunnel UUID. A tenant who writes a neighbour's UUID into a token they minted themselves cannot connect to that tunnel at all, and never serves a byte of its traffic. What the claim did buy them, before this rule, was the controller pushing that tunnel's merged configuration — hostnames, backends, and any backend-mTLS client keys — straight to their own proxy pods over the config API, which needs no working connector. That disclosure is what the refusal closes.

Possession, not age, decides who holds a contested tunnel: a Gateway already advertising that tunnel in its status addresses keeps it against any newcomer, however old the newcomer is. Otherwise a tenant whose Gateway happens to predate the victim's could retarget its own token and evict the legitimate holder. Age (then UID) decides only between claims with equal standing, such as two first-time claims.

The refusal exists because a connector token proves nothing. It is base64-encoded JSON, and any tenant who can create a `GatewayConfig` can write any tunnel UUID into it. Tunnel UUIDs are not secret either: the controller publishes them in Gateway status as `<id>.cfargotunnel.com` so external-dns can consume them. Without arbitration, naming a neighbour's tunnel is enough to join their partition — and because Cloudflare load-balances a tunnel's requests across every connector, the controller then merges both parties' routes into one ingress document and pushes the union to both parties' proxies. The claimant receives the incumbent's hostnames, backend services, filter bodies, and any backend-mTLS client certificates their routes carry.

Two Gateways in the SAME namespace may share a tunnel: that is a tenant sharing with itself, and no boundary is crossed.

If every party on a tunnel is trusted to see the others' routes — a single-tenant cluster, or a migration from the shared plane to dedicated ones — an operator can set `allowSharedTunnels: true` on the cluster-scoped `GatewayClassConfig` (chart value `gatewayClassConfig.allowSharedTunnels`). That restores the merge behaviour for the whole class. The field is deliberately not on `GatewayConfig`: a tenant must not be able to grant it to themselves.

When two DEDICATED Gateways end up on one tunnel, every affected route carries a `cf.k8s.lex.la/TunnelShared=True` condition and a mirrored `TunnelShared` Warning Event naming the other Gateways, so the collapsed isolation stays visible rather than silent. That covers both ways it can happen — the operator enabling `allowSharedTunnels`, and two Gateways in one namespace, which needs no opt-in. The condition's reason reads `TunnelSharedAcrossNamespaces` in the same-namespace case too, which is inaccurate; tracked in issue #680.

A dedicated Gateway sharing the CLASS tunnel is not flagged: the collision detector counts dedicated partitions only, and the shared plane is not one. Watch the controller log for that case.

### What this does not settle

Ownership here is first-claim-wins, and the claim is still unproven. The controller reads the tunnel UUID out of the connector token and never asks Cloudflare whether the claimant can actually use that tunnel. So a tenant who can create a Secret, a `GatewayConfig` and a Gateway in their own namespace can name any UUID — including one they have no access to — and become its holder, with a token that will never connect.

The practical consequence is a denial of service, not a disclosure: the rightful owner of that tunnel is then refused, permanently, and no amount of waiting fixes it. The rule protects a tunnel that is already being served; it cannot tell a legitimate first claim from a squatted one.

That denial of service is not confined to this cluster. Arbitration can only contest a tunnel some Gateway represents, so a UUID naming any OTHER tunnel in the same Cloudflare account — another cluster's, another product's, a bare `cloudflared` — is uncontested, and a `GatewayConfig` without its own `cloudflareCredentialsSecretRef` writes with the credentials resolved from the GatewayClass. The controller then overwrites that tunnel's ingress document with the claiming tenant's routes, using the operator's API token, and the outage lands on a service that has nothing to do with this cluster. The account is the blast radius, not the cluster; scope the API token to the tunnels this controller should manage, and give tenants who need isolation their own account rather than their own tunnel.

A claim that has not yet advertised anything is not defended while its configuration is unreadable. An established holder survives a token rotation because its address carries the claim, but a Gateway still bootstrapping — no address written yet — drops out of the arbitration entirely if its `GatewayConfig`, its token Secret, or the class credentials Secret it falls back to cannot be read at that moment, and a competing claim on the same tunnel is then accepted. The alternative, refusing to arbitrate at all on any unreadable claim, would let any tenant freeze the rule cluster-wide by deleting their own Secret.

Removing a refused Gateway's data plane relies on the controller ownerReference it stamped on the proxy Deployment: the cleanup deliberately leaves alone any object whose reference has been stripped, so it never deletes something another controller now owns. A tenant with write access in their own namespace can therefore strip that reference and keep the pod, and so the connector, running. It gets no route config, so it serves nothing of its own — but on a SHARED token it stays a registered connector on the contested tunnel.

Possession is read from `Gateway.status.addresses`, so anyone able to write `gateways/status` in their own namespace can forge it and evict a real holder immediately — possession beats age, so that is faster than the squat above. The Gateway API CRDs carry no RBAC aggregation labels, so the built-in `edit` and `admin` roles do not grant it; if you grant status write to tenants, this rule does not hold for them.

Watch for it in the `TunnelClaimRejected` Warning Events: a Gateway you believe owns a tunnel reporting `Accepted=False` against a claimant you do not recognise is this case. The remedy is operator-side: delete the squatting Gateway, or remove its `spec.infrastructure.parametersRef`. The rightful owner's status recovers on its next reconcile, within about half a minute. Its ROUTES are programmed on the next full route sync, which any route change in the cluster triggers — so if nothing else is moving, expect a gap between the Gateway reporting `Accepted=True` and its traffic actually flowing. Deleting only the `GatewayConfig` does NOT release the claim — a Gateway that already advertises a tunnel keeps holding it even when its configuration no longer resolves, which is what stops a token rotation from surrendering a tunnel. Verifying claims against the Cloudflare API would close it properly; that is tracked in issue #679.

## Securing a tenant data plane

A tenant data plane is locked down by default on two axes:

- **The config API is authenticated.** When a GatewayConfig declares no `authTokenSecretRef`, the controller generates a random bearer-token Secret (`cf-proxy-<gateway>-auth`, controller-owned, never rotated) and wires the proxy to it, so a tenant plane is never exposed unauthenticated. `authTokenSecretRef` is a bring-your-own-token OVERRIDE for operators who want to manage the token themselves (external secret stores, scheduled rotation). The proxy reads the token from a `SecretKeyRef` env var at start, so the pod template hashes the resolved token: rotating the bring-your-own Secret rolls the proxy pods automatically (same as `tunnelTokenSecretRef`), and the controller re-authenticates its config pushes with the new token.
- **The config API is network-restricted.** The controller renders a NetworkPolicy per data plane that admits the config API port (8081) only from the **controller pod** (its labels AND'd with its namespace, not the tenant's), so neither the config API nor the `/metrics` it co-serves is reachable from arbitrary pods — including other pods in the controller's own namespace, and including any in the Gateway's. The proxy's data port (8080) takes no in-cluster ingress at all — traffic arrives through the outbound tunnel. Because `/metrics` shares the locked port, a policy that admits only the controller silently breaks Prometheus scraping and therefore the rendered HPA (it reports `FailedGetPodsMetric` and holds `minReplicas`); set `proxy.networkPolicy.monitoringNamespaceSelector` in the chart to additionally admit your monitoring namespace. Where the CNI does not enforce NetworkPolicy this layer is a no-op (defense in depth, not a guarantee).

Both axes bound reach, not confidentiality: the controller's push is plain HTTP carrying the bearer token and, where the Gateway configures backend mTLS, the client certificate's private key, so on a CNI without pod-to-pod encryption an on-path party inside the cluster reads them — see [Config API Authentication](../reference/security.md#config-api-authentication).

Also note the RBAC equivalence: `create` on `GatewayConfig` (plus a Gateway referencing it) lets a user run an arbitrary image via `spec.image` under the namespace's default ServiceAccount — see the [security reference](../reference/security.md).

## Operational notes

- **Events:** the controller emits `ProxyProvisioned` (Normal) on the Gateway when the data plane is rendered, and `RenderFailed` (Warning) when rendering cannot proceed (apply failures) — `kubectl describe gateway` shows both.
- **No proxy image configured:** if neither `GatewayConfig.spec.image` nor the controller's `--proxy-image` default is set, the data plane cannot be rendered. The Gateway surfaces `Accepted=False` with reason `InvalidParameters` and a message naming the missing image (a persistent condition, not just a transient Event) — set one of the two and the Gateway recovers on the next reconcile.
- **Drain:** on pod shutdown the proxy unregisters its connectors from the edge and gives in-flight requests a grace period before exiting; the rendered `terminationGracePeriodSeconds` covers the window.
- **RBAC:** rendering requires cluster-wide write on Deployments/Services/HPAs (Gateways live in arbitrary namespaces); see the [security reference](../reference/security.md) for the exact rules and ownership guards.
- **Failure containment:** a tunnel-sync failure for one Gateway's tunnel marks only THAT Gateway's routes Pending; other tenants' route statuses are untouched.
- **Post-render breakage:** if the GatewayConfig stops resolving AFTER a healthy render (token Secret deleted, ref broken), new route changes fail closed — they are not programmed anywhere — but the already-running data plane keeps serving its LAST pushed config until the configuration resolves again or the Gateway is deleted. The Gateway surfaces `InvalidParameters`, and a route bound only to the broken Gateway reports `Accepted=False` on that parent (it is served nowhere) — a route accepted on a healthy parent too keeps `Accepted=True` there.
- **Not rendered:** per-Gateway PodDisruptionBudget and ServiceMonitor. The shared plane's ServiceMonitor does not select rendered pods; scrape them with a PodMonitor on the `cf.k8s.lex.la/gateway` label if needed (and admit the monitoring namespace via `proxy.networkPolicy.monitoringNamespaceSelector`, since the rendered NetworkPolicy locks the metrics port).
- **Cloudflare-side residue on teardown:** deleting the Gateway (or removing `infrastructure.parametersRef`) garbage-collects the in-cluster resources, but the LAST ingress document written to the dedicated Cloudflare Tunnel is left in place. It is inert — the connectors are gone, so nothing serves it — but it is not erased from the Cloudflare API. Delete or repurpose the tunnel on the Cloudflare side if you need the account state clean.
- **Sustained config-push failure is surfaced:** if the controller cannot push a route's config to its data plane for a sustained run of attempts (the proxy pods are unhealthy or the config-API NetworkPolicy blocks the push), the affected routes carry a `cf.k8s.lex.la/ProxyConfigPushed=False` condition and a `ProxyConfigPushFailed` Warning Event — the route spec is valid and the tunnel document was written, but the in-cluster proxy never received the config, so matching requests 502. `Accepted`/`Programmed` are unchanged (the failure does not flap them on a one-off blip), and the condition clears automatically on the first successful push.
- **API-credential rotation is watched:** rotating the `cloudflareCredentialsSecretRef` override (the controller-side API token, distinct from the connector token) re-syncs the Gateway's routes promptly — the controller watches that Secret. Connector-token (`tunnelTokenSecretRef`) rotation also re-renders the proxy Deployment.
- **Well-known labels:** every rendered resource (Deployment, headless Service, HPA, NetworkPolicy) carries the [GEP-1762](https://github.com/kubernetes-sigs/gateway-api/blob/main/geps/gep-1762/index.md) well-known labels `gateway.networking.k8s.io/gateway-name` and `gateway.networking.k8s.io/gateway-class-name` on its metadata (never the immutable selector), so ecosystem tooling that understands the Gateway API convention can discover a Gateway's dedicated data plane without a controller-specific label. Both values go through the same truncation as the `cf.k8s.lex.la/gateway` selector label: Gateway and GatewayClass names are DNS-1123 subdomains up to 253 characters, but Kubernetes label values cap at 63, so tooling matching on the exact Gateway or GatewayClass name will miss for names longer than that.
