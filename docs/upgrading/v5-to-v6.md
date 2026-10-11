# Upgrading from v5 to v6

v6 matches each request to a listener by port and scheme as well as by hostname, as the Gateway API requires. The chart values, the CRDs, RBAC, the Kubernetes floor (1.31) and the Gateway API bundle (v1.6.x) are the same as in v5, so the upgrade itself is a plain `helm upgrade`. What changes is which listener, and so which routes, a request reaches. v6 also drops the `--gateway-class-name` controller flag, which has done nothing since v2.

Each section headed "Breaking" describes something that works on v5 and can serve differently, or stop, after the upgrade, names who it reaches, how to check, and what to change. Read them before running `helm upgrade`, then follow the [upgrade procedure](#upgrade-procedure) at the end.

The commands below read cluster objects with `kubectl` and filter them with `jq`. None of them changes anything.

## Breaking: a Gateway with only an HTTP listener stops serving HTTPS

A Gateway with only an HTTP listener stops serving HTTPS: add an HTTPS listener. An `HTTP` listener now takes only plain-HTTP requests and an `HTTPS` listener only HTTPS ones. On v5 the scheme was not compared, so an `HTTP` listener on 80 also served every HTTPS request, and most Gateways were written that way. Clients reach the Cloudflare edge mostly over HTTPS, gRPC always does, and a zone with Always Use HTTPS on turns every plain-HTTP request into an HTTPS one before it reaches the tunnel. On v6 such a Gateway answers those requests with a 404, and gRPC requests with `Unimplemented`.

Affected: every Gateway without an `HTTPS` listener on 443, counting the entries of its ListenerSets. List the Gateways and ListenerSets that have none:

```bash
kubectl get gateways,listenersets --all-namespaces --output json | jq --raw-output '
  .items[]
  | select(any((.spec.listeners // [])[]; .protocol == "HTTPS" and .port == 443) | not)
  | "\(.kind) \(.metadata.namespace)/\(.metadata.name): \([(.spec.listeners // [])[] | "\(.protocol) \(.port)"] | join(", "))"'
```

A Gateway in the list still serves HTTPS on 443 if one of its ListenerSets has such an entry, that is, a ListenerSet of it is missing from the list. Add an `HTTPS` listener on 443 next to the `HTTP` one. It needs no `tls` block: the edge serves its own certificate for the hostname, and `certificateRefs` would be validated but never served.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: cloudflare-tunnel
  namespace: cloudflare-tunnel-system
spec:
  gatewayClassName: cloudflare-tunnel
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: All
    - name: https
      protocol: HTTPS
      port: 443
      allowedRoutes:
        namespaces:
          from: All
```

Keep the `HTTP` listener only if plain-HTTP clients reach the zone; on a zone with Always Use HTTPS it gets no traffic. A listener with a hostname needs the same hostname on the new one, so the routes keep the hostnames they answer.

A route binds to the new listener only if nothing pins it elsewhere. A route without `sectionName` and `port` in its parentRef binds to every listener of the Gateway that admits it, so it picks up the `HTTPS` listener by itself. A route whose parentRef names a listener by `sectionName` or `port` binds only to that one. List those parentRefs:

```bash
kubectl get httproutes,grpcroutes --all-namespaces --output json | jq --raw-output '
  .items[]
  | .kind as $kind | .metadata as $m
  | .spec.parentRefs[]? | select(.sectionName != null or .port != null)
  | "\($kind) \($m.namespace)/\($m.name) -> \(.name) sectionName=\(.sectionName // "-") port=\(.port // "-")"'
```

Add a second parentRef with the `HTTPS` listener's `sectionName`, or drop the pin. [Listener ports and schemes](../gateway-api/limitations.md#listener-ports-and-schemes) has the full rule.

## Breaking: listeners are matched by port

A request now reaches only the listeners on the port it arrived on: the port the edge keeps in `Host` when the client used a non-default one (`app.example.com:8443`), else 80 for HTTP and 443 for HTTPS. On v5 ports were not compared, so listeners with the same hostname on different ports shared their routes. A route attached only to a listener on 8443 no longer answers on 443, and the other way round. A request on a port no listener of the Gateway has gets a 404. A listener on a port the Cloudflare edge does not proxy, or an `HTTP` listener on one of the edge's HTTPS ports such as 8443, gets traffic only from a client that names its port in `Host` by hand.

Affected: Gateways and ListenerSets with a listener on a port other than 80 and 443. List them:

```bash
kubectl get gateways,listenersets --all-namespaces --output json | jq --raw-output '
  .items[] | .kind as $kind | .metadata as $m
  | .spec.listeners[]? | select(.port != 80 and .port != 443)
  | "\($kind) \($m.namespace)/\($m.name) listener \(.name): \(.protocol) \(.port)"'
```

Requests reach such a listener only on its own port. Move the route to a listener on the port its clients use, or attach it to both.

The port in `Host` is the client's to choose: the edge forwards a `Host` naming any port and carries the port the client connected to nowhere else. A listener port separates traffic but is not an access boundary.

## Breaking: a scheme-less redirect takes the request's scheme and port

A `RequestRedirect` that sets neither `scheme` nor `port` now keeps the scheme and port of the request, as the Gateway API requires. On v5 the controller picked them from the listeners the route was attached to, preferring `https` when the route sat on both an `HTTP` and an `HTTPS` listener. A route on both listeners now redirects a plain-HTTP request to `http://`, so a redirect meant to move clients to HTTPS must set `scheme: https`. The scheme's default port is left out of `Location` as before.

Affected: redirect filters without `scheme` on routes attached to an `HTTP` listener. To force a scheme, set it on the filter. [Redirect port](../gateway-api/limitations.md#redirect-port) has the rule.

## Breaking: the `--gateway-class-name` flag is removed

The controller no longer accepts `--gateway-class-name`. The flag has done nothing since v2, because GatewayClasses are matched by `spec.controllerName`. A controller started with it now exits at once with `unknown flag: --gateway-class-name`.

Affected: controllers deployed from raw manifests or a patched Deployment whose container `args` or `command` still carry the flag. The chart never passes it. To check, run `kubectl get deployment --all-namespaces --output json | jq --raw-output '.items[] | select(any(.spec.template.spec.containers[] | (.command // []) + (.args // []) | .[]; startswith("--gateway-class-name"))) | "\(.metadata.namespace)/\(.metadata.name)"'`, then remove the flag before upgrading.

## Behaviour change: shadowed routes are reported per port and scheme

Shadowing is computed per port and scheme. Two routes with the same match on listeners of different ports, or on an `HTTP` and an `HTTPS` listener, no longer shadow each other. A route that loses its match on one port it is reached on is reported `RouteShadowed` even while it still serves another, and the message names that port.

## During the rolling upgrade

The controller and the proxies roll separately. The listener data the v6 controller sends uses new field names, and a proxy ignores fields it does not know, so a mixed pair keeps exchanging config. While old and new pods run side by side:

- A v5 proxy pushed by a v6 controller finds no listener data it can read. It serves every route without listener isolation, and a scheme-less redirect from it carries no port and can get the wrong scheme, since the v6 controller no longer stamps them.
- A v6 proxy pushed by a v5 controller gets the v5 listener data under the old names and ignores it. It serves without listener isolation until the controller is upgraded, and takes the scheme and port of a scheme-less redirect from the v5 controller's stamp.

Each data plane converges once both sides run v6. Avoid route and Gateway changes until the rollout finishes.

## Upgrade procedure

1. **Add the `HTTPS` listeners** the first Breaking section finds, and the parentRefs for pinned routes, before upgrading. On v5 they change nothing that is served, except that a scheme-less redirect on a route that now sits on both listeners prefers `https`.
2. **Check the other Breaking sections** with the commands they give.
3. **Run the upgrade.** The chart values and CRDs did not change since v5, so no values edit and no CRD re-apply is needed:

    ```bash
    helm upgrade cloudflare-tunnel-gateway-controller \
      oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
      --namespace cloudflare-tunnel-system \
      --reset-then-reuse-values \
      --values values.yaml
    ```

### What to watch after the rollout

- 404s on HTTPS traffic for hostnames served before: a Gateway or a pinned route without an `HTTPS` listener.
- 404s on a non-default port: a route attached to a listener on another port.
- `RouteShadowed` conditions that cleared or appeared, and their messages naming a port.

## What does not change

- No CR migration, no chart value, CRD or RBAC change.
- The Kubernetes floor (1.31) and the Gateway API bundle (v1.6.x).
- Hostname matching and listener isolation by hostname, which v5 introduced.
