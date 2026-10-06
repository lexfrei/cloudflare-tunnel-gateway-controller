# Upgrading from v4 to v5

v5 brings routing, status and backend TLS in line with the Gateway API v1.6 spec, and stops tying route status to the Cloudflare tunnel document. The chart values, the CRDs, RBAC, the Kubernetes floor (1.31) and the Gateway API bundle (v1.6.x) are the same as in v4.0.0, so the upgrade itself is a plain `helm upgrade`. What changes is what the controller serves and reports.

Each section headed "Breaking" describes something that works on v4 and can serve differently, or stop, after the upgrade, names who it reaches, how to check, and what to change. Sections headed "Behaviour change" describe status, Events and metrics that change without changing what is served. Read the Breaking sections before running `helm upgrade`, then follow the [upgrade procedure](#upgrade-procedure) at the end.

The commands below read cluster objects with `kubectl` and filter them with `jq`. None of them changes anything.

## Breaking: listener isolation

A request now belongs to the most specific listener of a Gateway that matches its host, and only routes attached to that listener answer it. With listeners `*.example.com` and `foo.example.com` on one Gateway, a route attached only to `*.example.com` stops answering `foo.example.com`, even when it lists that hostname, and those requests get a 404 unless a route on `foo.example.com` matches them. A listener without a hostname gets only the hosts no other listener of the Gateway matches. ListenerSet entries count as listeners of their parent Gateway. Ports are not compared. On v4.0.0 every listener the route was attached through lent it its hostnames.

Affected: a route that answers a hostname only through a wildcard or hostname-less listener, while a more specific listener of the same Gateway owns that hostname and does not admit the route. That covers three cases:

- the route is attached through `sectionName` to the less specific listener only;
- the more specific listener's `allowedRoutes` does not admit the route;
- the more specific listener is a ListenerSet entry. A route whose parentRef names the Gateway binds only to the Gateway's own listeners, never to ListenerSet entries, so it stops answering every hostname a ListenerSet entry of that Gateway owns.

A route without `sectionName` binds to every listener of the Gateway that admits it, so in the other cases it keeps answering. List the Gateways with a wildcard or hostname-less listener, and the ListenerSet entries attached to each Gateway:

```bash
kubectl get gateways --all-namespaces --output json | jq --raw-output '
  .items[]
  | select(any((.spec.listeners // [])[]; (.hostname // "") == "" or (.hostname | startswith("*."))))
  | "\(.metadata.namespace)/\(.metadata.name): \([(.spec.listeners // [])[] | .hostname // "<none>"] | join(", "))"'
kubectl get listenersets --all-namespaces --output json | jq --raw-output '
  .items[]
  | "\(.spec.parentRef.namespace // .metadata.namespace)/\(.spec.parentRef.name) <- \(.metadata.namespace)/\(.metadata.name): \([(.spec.listeners // [])[] | .hostname // "<none>"] | join(", "))"'
```

A Gateway from the first list that has a more specific listener of its own, or a ListenerSet entry in the second list, is the one to look at.

Attach the route to the more specific listener as well, or remove that listener. [Listener isolation](../gateway-api/limitations.md#listener-isolation) has the full rule.

## Breaking: a Gateway that sets spec.tls.frontend is refused

Clients complete TLS with the Cloudflare edge, so no client certificate ever reaches the proxy. A Gateway that sets `spec.tls.frontend` (client certificate validation), with any content, is now refused: `Accepted=False` and `Programmed=False` with reason `Invalid`, no routes served through it, its ListenerSets `Accepted=False` with `ParentNotAccepted`, and its dedicated data plane removed.

Find such Gateways:

```bash
kubectl get gateways --all-namespaces --output json | jq --raw-output '
  .items[] | select(.spec.tls.frontend != null) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Remove `spec.tls.frontend`, and require client certificates at the Cloudflare edge instead, for example with Cloudflare's mTLS or Access policies. [Client certificate validation is refused](../gateway-api/limitations.md#client-certificate-validation-spectlsfrontend-is-refused) has the details.

## Breaking: spec.addresses other than the tunnel hostname

A tunnel is reachable only at its `<tunnel-id>.cfargotunnel.com` hostname, and `spec.addresses` is now checked against that:

- An entry of type `IPAddress` or `NamedAddress`, a custom type, or an entry without a type (the API default is `IPAddress`) refuses the whole Gateway: `Accepted=False` with reason `UnsupportedAddress`, no routes served, its ListenerSets `ParentNotAccepted`, and its dedicated data plane removed.
- A `Hostname` entry with a value other than the tunnel CNAME keeps the Gateway accepted and serving at the CNAME, with `Programmed=False` and reason `AddressNotUsable`.

On v4.0.0 `spec.addresses` was ignored. Find Gateways that set it:

```bash
kubectl get gateways --all-namespaces --output json | jq --raw-output '
  .items[] | select((.spec.addresses // []) | length > 0)
  | "\(.metadata.namespace)/\(.metadata.name): \(.spec.addresses)"'
```

Remove `spec.addresses`, or keep a single `Hostname` entry without a value. [spec.addresses accepts only the tunnel hostname](../gateway-api/limitations.md#specaddresses-accepts-only-the-tunnel-hostname) has the rule.

## Breaking: group "" in allowedRoutes.kinds names the core group

In a listener's `allowedRoutes.kinds`, only an omitted `group` defaults to `gateway.networking.k8s.io`. An explicit `group: ""` is the Kubernetes core group, which has no route kinds, so the listener no longer admits that kind: it reports `ResolvedRefs=False` with reason `InvalidRouteKinds` and leaves the kind out of `supportedKinds`. On v4.0.0 `""` was read as the Gateway API group. This is the listener-side twin of the v4 change to route [parentRefs](v3-to-v4.md#breaking-a-parentref-binds-only-in-the-gateway-api-group).

Find listeners that spell it so, on Gateways and ListenerSets:

```bash
kubectl get gateways,listenersets --all-namespaces --output json | jq --raw-output '
  .items[]
  | select(any(.spec.listeners[]?.allowedRoutes.kinds[]?; .group == ""))
  | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
```

Omit `group`, or set it to `gateway.networking.k8s.io`.

## Breaking: route matches are ranked one match at a time

The proxy now orders matches the way the Gateway API lists the precedence rules. On v4.0.0 it scored whole rules with a weighted sum, and three things change:

- Each match is ranked on its own. A rule's other matches no longer lift the one that fired: `[Exact /admin, PathPrefix /]` used to outrank `[PathPrefix /bar]` for `/bar`, and now `/bar` goes to the `/bar` rule.
- A criterion decides only when every criterion before it ties. Eleven header matches no longer beat a method match, and eleven query parameter matches no longer beat a header match.
- GRPCRoute matches are ranked by the length of the service, then of the method. A method-only match no longer beats a service-only match.

Ties go to the oldest route, then `{namespace}/{name}`, then rule order. A rule without matches ranks as `PathPrefix /`.

Affected: routes on the same hostname whose rules mix broad and specific matches, and GRPCRoutes that match on method alone next to service matches. After the upgrade a request can be served by a different rule, and the `cf.k8s.lex.la/RouteShadowed` condition can move to the other route of a colliding pair. Check that condition after the upgrade:

```bash
kubectl get httproutes,grpcroutes --all-namespaces --output json | jq --raw-output '
  .items[] | select(any(.status.parents[]?.conditions[]?; .type == "cf.k8s.lex.la/RouteShadowed" and .status == "True"))
  | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
```

[Route Conflict Resolution](../gateway-api/limitations.md#route-conflict-resolution) lists the order.

## Breaking: an empty GRPCRoute match matches everything

An empty match (`{}`) in a GRPCRoute rule now matches every request, as the spec requires, even next to specific matches in the same rule. On v4.0.0 the empty match was dropped when the rule had other matches, so `matches: [{}, {method: {service: foo.Bar}}]` served only `foo.Bar`. Now such a rule catches every request on its hostnames that a more specific match does not take. It ranks below any specific match, and against an equally broad one, such as another route's rule without matches, the oldest route wins.

Find such rules:

```bash
kubectl get grpcroutes --all-namespaces --output json | jq --raw-output '
  .items[]
  | select(any((.spec.rules // [])[]; (.matches // []) | (length > 1 and any(.[]; . == {}))))
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

Remove the `{}` entry if the rule should serve only its specific matches.

## Breaking: redirect ports

Two changes to the port in a `RequestRedirect` `Location`:

- **A scheme-less redirect takes the listener port.** When the filter sets neither `scheme` nor `port`, the redirect now carries the port of the listener the route is attached to, when the Cloudflare edge serves that port for the scheme (for `http`: 8080, 8880, 2052, 2082, 2086, 2095; for `https`: 2053, 2083, 2087, 2096, 8443). A route on an `HTTP` listener on port 8080 now redirects to `http://host:8080/...`. Listeners on 80 or 443, or on a port the edge does not serve, behave as before. With several accepting listeners the well-known port wins, then the lowest edge-served one.
- **The scheme's default port is left out.** `Location` no longer carries `:80` for `http` or `:443` for `https`, also when the filter sets `port` explicitly.

Affected by the first: redirect filters without `scheme` and `port` on routes attached to a listener on one of the edge ports above. List the Gateway and ListenerSet listeners on those ports:

```bash
kubectl get gateways,listenersets --all-namespaces --output json | jq --raw-output '
  [8080, 8880, 2052, 2082, 2086, 2095, 2053, 2083, 2087, 2096, 8443] as $edge
  | .items[] | .kind as $kind | .metadata as $m
  | .spec.listeners[]? | select(.port as $p | $edge | index($p))
  | "\($kind) \($m.namespace)/\($m.name) listener \(.name): \(.protocol) \(.port)"'
```

To keep such a redirect off the listener port, set `scheme` or `port` on the filter. [Redirect port](../gateway-api/limitations.md#redirect-port) has the rule.

## Breaking: RequestRedirect and URLRewrite in different filter lists

A rule that has a `RequestRedirect` and a `URLRewrite` anywhere among its own filters and its `backendRefs[].filters` is now refused, as the spec requires. The CRD rejects the pair inside one list but not across lists, and v4.0.0 served such a rule. Now the rule answers HTTP 500. If every rule of the route is refused this way, the route reports `Accepted=False` with reason `IncompatibleFilters`; otherwise it reports `PartiallyInvalid=True`.

Find such rules:

```bash
kubectl get httproutes --all-namespaces --output json | jq --raw-output '
  .items[]
  | select(any((.spec.rules // [])[];
      [(.filters // [])[].type, (.backendRefs // [])[].filters[]?.type] as $types
      | ($types | index("RequestRedirect")) and ($types | index("URLRewrite"))))
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

Keep the redirect and the rewrite in separate rules.

## Breaking: header modifier entries that differ only in case

In `RequestHeaderModifier` and `ResponseHeaderModifier`, on HTTPRoute and GRPCRoute, several `set` or `add` entries whose names match case-insensitively now apply only the first one. On v4.0.0 `set` let the last one win and `add` sent every value. A request or response can therefore carry a different header value than before. Keep one entry per header name.

## Breaking: BackendTLSPolicy and appProtocol apply to core Services only

A `BackendTLSPolicy` and a Service port's `appProtocol` now apply only to backends and `RequestMirror` destinations of kind `Service` in the core group:

- A `ServiceImport` backend named `X` no longer takes the policy, or the `appProtocol`, of a local Service named `X`. It is dialed plaintext over HTTP/1.1 (h2c for gRPC). On v4.0.0 it inherited both and was dialed with TLS.
- A policy `targetRef` attaches only when its `group` is `""` and its `kind` is `Service`. Any other target, such as a `ServiceImport`, attaches to nothing, and the policy reports `Accepted=False` with reason `TargetNotFound` under each Gateway whose routes use that object. On v4.0.0 a `targetRef` with another group and kind `Service` attached to the core Service.

Affected: Multi-Cluster Services users whose `ServiceImport` backends relied on a same-named Service's policy, and policies whose `targetRef` names another group or kind. Find routes with a `ServiceImport` backend or mirror destination, then such policies:

```bash
kubectl get httproutes,grpcroutes --all-namespaces --output json | jq --raw-output '
  .items[]
  | select(any((.spec.rules // [])[]
      | (.backendRefs // [])[], (.filters // [])[].requestMirror.backendRef?, (.backendRefs // [])[].filters[]?.requestMirror.backendRef?;
      .kind? == "ServiceImport"))
  | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
kubectl get backendtlspolicies --all-namespaces --output json | jq --raw-output '
  .items[] | select(any((.spec.targetRefs // [])[]; .group != "" or .kind != "Service"))
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

There is no way to keep TLS on a `ServiceImport` backend: point the route at a core Service with a `BackendTLSPolicy` instead. [Backend mTLS](../gateway-api/limitations.md#backend-mtls-backendtlspolicy) lists the target kinds.

## Breaking: HTTPRoute retries take effect

`spec.rules[].retry` is an Experimental-channel field, and the proxy now honours it. On v4.0.0 it was ignored. A route that already carries a `retry` stanza starts retrying after the upgrade: on the listed status codes and on every transport error, for every method, `POST` included, with request bodies up to 64 KiB resent. Two limits apply whatever the route says: at most 10 retries per request, and at least 10ms between attempts. A route asking for more than 10 is served with 10 and gets a Warning Event with reason `ConfigOverridden`; its conditions do not change.

Only clusters running the Experimental Gateway API CRDs are affected: the Standard CRD drops the field. Find routes that set it:

```bash
kubectl get httproutes --all-namespaces --output json | jq --raw-output '
  .items[] | select(any((.spec.rules // [])[]; .retry != null)) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Remove `retry` from rules whose backends cannot take a repeated request. Retries start once both the controller and the proxy run v5. [Retries](../gateway-api/limitations.md#retries) has the full behaviour, and `cftunnel_proxy_backend_retries_total` counts each retry.

## Breaking: error statuses for backends that cannot be served

- **HTTP 500 instead of 502.** A backendRef whose `BackendTLSPolicy` cannot be enforced (every CA ref invalid, `wellKnownCACertificates` alone, or an unsupported SAN type), and a backend whose Service port declares a TLS `appProtocol` (`https`, `kubernetes.io/wss`) without a policy, now answer HTTP 500 without a dial. On v4.0.0 they answered 502: from the failed handshake in the policy case, and without a dial in the `appProtocol` case. The first case now also sets `ResolvedRefs=False` with reason `InvalidBackendTLSPolicy` on the route, and a `RequestMirror` to such a target loses only the mirrored copies.
- **gRPC `UNAVAILABLE`.** A gRPC request (`Content-Type: application/grpc`) that the proxy cannot send to a backend, because the backend is invalid, has no ready endpoints, cannot be reached or times out, or the rule fails closed, now gets gRPC status `UNAVAILABLE` (14) in a trailers-only response. On v4.0.0 it got HTTP 500, 502, 503 or 504, which gRPC clients report as `Unknown` or `Internal`. The choice follows the request's content type, so a gRPC request routed by an HTTPRoute gets it too. These responses carry HTTP status 200 and show 200 in the access log, but still count as `5xx` in `cftunnel_proxy_requests_total`. Like every gRPC response they need the `http2` tunnel transport.

Affected: alerts and dashboards that match 502 for TLS backends, or HTTP 5xx in gRPC client metrics. Move them to 500 and the route's `ResolvedRefs` condition, and to gRPC status `UNAVAILABLE`. [Unavailable backends return a status](../gateway-api/limitations.md#unavailable-backends-return-a-status-not-a-dial-error) and [Fail-closed enforcement](../gateway-api/limitations.md#fail-closed-enforcement) have the details.

## Breaking: a failed tunnel document write no longer changes route status

The edge sends a request to a tunnel by its hostname's DNS record, not by the tunnel's ingress document, so the document only feeds the Cloudflare dashboard. A failed write of it (Cloudflare API down, a revoked or wrong API token, a deleted tunnel) no longer sets `Accepted=False` with reason `Pending` on routes, and routes keep serving. The failure shows up only as a `TunnelDocumentWriteFailed` Warning Event on every Gateway served from that tunnel, in `cftunnel_sync_errors_total`, and in the controller log, once while it keeps failing the same way. The write is retried every 15 seconds. A sync whose every tunnel write fails records `cftunnel_sync_duration_seconds` with `status="partial"`.

Affected: alerts that rely on route `Accepted=False` or `Pending` to catch a broken Cloudflare credential. Alert on the Events or on `cftunnel_sync_errors_total` instead. A revoked token can also stay unnoticed for up to 5 minutes while routes do not change, see [below](#behaviour-change-the-tunnel-document-is-cached).

The route `Accepted=True` message is now `Route accepted and programmed into the proxy`. Data-plane refusals, such as a refused tunnel claim or the namespace's plane cap, still set `Pending` per parent. [The tunnel ingress document only feeds the dashboard](../gateway-api/limitations.md#the-tunnel-ingress-document-only-feeds-the-dashboard) has the details.

## Behaviour change: one ingress rule per hostname

The tunnel ingress document now holds one rule per distinct hostname, without a path, and has no size cap. The first route sync after the upgrade rewrites every tunnel document, and the Cloudflare dashboard stops listing per-path entries. The rule names the lexicographically smallest backend URL serving the hostname, which affects only what the dashboard shows. The 1000-rule cap of v4.0.0, and the `Accepted=False` / `Pending` freeze that came with it, are gone. Routing does not change: the proxy does all matching.

## Behaviour change: the tunnel document is cached

The controller keeps the last document it read or wrote for each tunnel, and a sync whose document matches it makes no Cloudflare API call. The copy is trusted for 5 minutes, checked against the API token and the account ID, and dropped on any failed read or write. An edit made in the dashboard is therefore reverted by the next sync that changes the document, or, while routes do not change, by the first sync at least 5 minutes after the controller last read or wrote it. The saved calls are counted in the new `cftunnel_cloudflare_api_calls_skipped_total`; `cftunnel_cloudflare_api_calls_total` still counts real calls only, so the Cloudflare API rate drops.

## Behaviour change: read errors are retried, and pushes are held

A failed API read (anything other than not-found or a denial) is no longer written into status as a verdict. The controller keeps the previous status and retries:

- A route whose namespace cannot be read for an `allowedRoutes` selector is `Accepted=False` with reason `Pending`, where v4.0.0 reported `NotAllowedByListeners` and never retried. A ListenerSet whose namespace cannot be read for `allowedListeners` is `Pending` too.
- A dedicated Gateway whose class chain cannot be read keeps its status and address, where v4.0.0 reported `InvalidParameters`. `attachedRoutes` and `attachedListenerSets` keep their value while they cannot be counted.
- A listener whose certificate refs or ReferenceGrants cannot be read keeps its status instead of reporting `RefNotPermitted` or `InvalidCertificateRef`; a ListenerSet entry goes `Pending`.

When a read that decides how a backend is dialed fails, the controller now pushes no new config to that data plane until the read works. These reads are the namespace's BackendTLSPolicies, the Service a `sectionName`-scoped policy targets, a policy's CA ConfigMap, the backend Service's `appProtocol`, the inputs of the backend client certificate (parent Gateway, GatewayClass, ListenerSet parent, Secret, ReferenceGrant), and the ReferenceGrants of a cross-namespace backendRef. The proxy keeps serving its last config, a deleted route included, and the sync is retried every 15 seconds. Other data planes are unaffected. Each held push logs `proxy config not pushed` and counts in `cftunnel_sync_errors_total{error_type="proxy_push"}`. After a controller restart, proxy replicas that start while such a read keeps failing get no config and stay unready. [Fail-closed enforcement](../gateway-api/limitations.md#fail-closed-enforcement) lists the reads.

A `sectionName`-scoped policy on a Service the controller has not seen yet applies to every port of that Service until the Service shows up. A `RequestMirror` to a just-created Service therefore dials TLS with the policy's settings, and the handshake fails if that port does not serve TLS.

## Behaviour change: Gateway and GatewayClass status

- **Programmed follows Accepted.** A Gateway whose own listeners are all invalid (`Accepted=False`, `ListenersNotValid`) now reports `Programmed=False` with reason `Invalid` on both planes. A Gateway with an attached ListenerSet stays `Programmed=True`, since it serves the ListenerSet's listeners. On v4.0.0 a shared-plane Gateway reported `Programmed=True` regardless.
- **GatewayClass conflicts show on the class.** A GatewayClass reports `Accepted=False` with reason `InvalidParameters` when a Gateway on it would join a `parametersRef` conflict between classes, which v4 already refused on the Gateways.
- **SupportedVersion checks every CRD.** The GatewayClass `SupportedVersion` condition now reads the bundle version of every Gateway API CRD the controller serves: GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant, ListenerSet and BackendTLSPolicy. A partly upgraded bundle, or a missing one of them, reports `SupportedVersion=False`. The condition does not gate serving.
- **ListenerSets of a refused Gateway.** A ListenerSet whose parent Gateway is `Accepted=False` for any reason other than `ListenersNotValid` reports `Accepted=False` with reason `ParentNotAccepted`, and the refused Gateway reports `attachedListenerSets: 0`.
- **ListenerSet entries.** An entry with a protocol the controller does not serve, such as `TCP`, no longer claims its port, so a later `HTTP` entry on that port is no longer `ProtocolConflict`. The Gateway's `ListenersNotValid` message now names the accepted listeners as well.
- **Certificate refs of another kind.** A ReferenceGrant counts for a certificate ref only when it names the ref's own group and kind, so a grant to Secrets does not cover a ref to another kind. A cross-namespace listener `certificateRefs` entry or `clientCertificateRef` of a kind other than `Secret` now reports `RefNotPermitted` without such a grant, and `InvalidCertificateRef` (`InvalidClientCertificateRef`) with one. On v4.0.0 it reported `InvalidCertificateRef` either way. Such a ref is refused in both versions.
- **`From: Selector` without a selector** is handled like a selector that does not parse: the listener or ListenerSet entry reports `Accepted=False` with reason `UnsupportedValue`, and on `allowedListeners` the Gateway gets a Warning Event.
- **Generated Secrets** for a per-Gateway data plane (auth token, config TLS) get the `spec.infrastructure` labels and annotations and the `gateway-name` and `gateway-class-name` labels when they are created. Existing Secrets keep their metadata until they are recreated.

## Behaviour change: route and BackendTLSPolicy status

- **Other controllers' conditions are kept.** A condition type this controller does not write, such as `special.io/SomeField`, stays in its `status.parents` entry exactly as stored. On v4.0.0 it was deleted on the next sync, and a foreign condition with a higher `observedGeneration` made the controller skip its own write.
- **BackendTLSPolicy ancestors** list every Gateway that accepted a route sending traffic to the target: through a ListenerSet, from another namespace under a ReferenceGrant, and through a `RequestMirror`. A Gateway that rejected the route is no longer listed. When no managed Gateway is affected any more, the controller removes its own entries. Past the 16-entry cap, each Gateway left out gets a `PolicyAncestorsFull` Warning Event.
- **One invalid CA ref** among valid ones no longer rejects a BackendTLSPolicy. It stays `Accepted`, `ResolvedRefs=False` names the invalid refs, and the proxy trusts the valid ones.
- **A `sectionName`-scoped BackendTLSPolicy** now governs its named port over a policy for the whole Service, whatever their creation order. On v4.0.0 the older of the two won.

## Behaviour change: per-plane hostnames and cleanup

- A route attached to Gateways on different data planes now takes, on each plane, only the hostnames that plane's own Gateways give it.
- When no Gateway a plane serves gives a route a listener, for example because the Gateway was deleted between binding and the config build, the route is left out of that plane. On v4.0.0 a hostname-less route went through as written and answered every Host until the next sync.
- Deleting a GatewayClass now removes the `status.parents` entries this controller wrote on its routes.
- A per-Gateway plane that needs a new config TLS slot after the last one (4095) gets a `RenderFailed` Warning Event instead of a slot the controller cannot read back. Deleting that slot's Secret lets the controller issue again.
- Revoking a ReferenceGrant that covers only a `RequestMirror` target now stops the mirror on the next reconcile.
- A backendRef refused for a missing ReferenceGrant answers 500 only on the route that made the reference. On v4.0.0 every route on the plane that used the same Service got 500.

## Behaviour change: proxy replay across EndpointSlices

When a proxy pod joins, the replay check now looks at every EndpointSlice of the proxy Service, not only the one that changed. The replay of every plane, triggered by a deleted slice or one whose Gateway is gone, runs once and is not retried: a failure is a Warn line, not a `proxy-endpoint-reconciler` reconcile error, and the failed plane gets its config from its own next slice change or route sync.

## During the rolling upgrade

The controller and the proxies roll separately, and the config they exchange is compatible both ways: a proxy ignores fields it does not know. While old and new pods run side by side:

- A v4 proxy pushed by a v5 controller gets the changes the controller makes while building the config at once: the listener port of a scheme-less redirect, the header modifier dedup, the 500 for refused filter pairs, for unenforceable backend TLS and for a TLS `appProtocol` without a policy, the empty GRPCRoute match, and per-plane hostnames. It does not do what v5 does at request time: it ignores `retry` and listener isolation, keeps the v4 match ranking, still writes `:80` or `:443` into a redirect with an explicit default port, and answers gRPC requests to backends that cannot be served with HTTP 500, 502, 503 or 504 instead of `UNAVAILABLE`.
- A v5 proxy pushed by a v4 controller gets none of those controller-side changes, no `retry` and no isolation data. It does not retry or isolate listeners, and ranks GRPCRoute matches by path until the controller is upgraded.

Each data plane converges once both sides run v5. Avoid route changes until the rollout finishes.

## Upgrade procedure

1. **Check the Breaking sections above** with the commands they give, and fix what they find, before upgrading.
2. **Keep the Gateway API bundle on v1.6.x.** v5 needs nothing newer. If the `SupportedVersion` condition on your GatewayClass reports `False` after the upgrade, its message names the CRD whose bundle version differs; re-apply the v1.6.x bundle on the channel you run, as [Prerequisites](../getting-started/prerequisites.md#gateway-api-crds) shows.
3. **Run the upgrade.** The chart values and CRDs did not change since v4.0.0, so no values edit and no CRD re-apply is needed:

    ```bash
    helm upgrade cloudflare-tunnel-gateway-controller \
      oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
      --namespace cloudflare-tunnel-system \
      --reset-then-reuse-values \
      --values values.yaml
    ```

    `--reuse-values` works as well. CI renders the chart with the released `values.yaml` of v3.0.0, v3.2.0, v3.5.0 and v4.0.0 the way `--reuse-values` does, so values carried over from any of those render.

4. **Move alerts and dashboards**: route `Accepted=False` for Cloudflare failures to `TunnelDocumentWriteFailed` Events and `cftunnel_sync_errors_total`; 502 for TLS backends to 500 and `ResolvedRefs`; HTTP 5xx for gRPC to `UNAVAILABLE`; and anything matching the Gateway `Programmed` condition.

### What to watch after the rollout

- One document write per tunnel on the first sync, as each document is rewritten to one rule per hostname.
- Routes reporting `PartiallyInvalid=True` or `Accepted=False` with `IncompatibleFilters`, and listeners reporting `InvalidRouteKinds`.
- Gateways reporting `Accepted=False` with `Invalid` or `UnsupportedAddress`, or `Programmed=False` with `AddressNotUsable` or `Invalid`.
- `RouteShadowed` conditions that moved, and 404s on hostnames that a more specific listener now owns.
- `proxy config not pushed` log lines, which mean a backend TLS or grant read keeps failing.

## What does not change

- No CR migration, no chart value, CRD or RBAC change.
- The Kubernetes floor (1.31) and the Gateway API bundle (v1.6.x).
- The config API, its TLS and its auth token.
