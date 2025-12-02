# Operations

This section covers operational aspects of running the Cloudflare Tunnel Gateway
Controller in production.

## Overview

Operating the controller effectively requires understanding:

- Common issues and how to troubleshoot them
- Metrics and alerting for proactive monitoring
- Alternative installation methods for special environments

## Sections

<div class="grid cards" markdown>

-   :material-wrench:{ .lg .middle } **Troubleshooting**

    ---

    Common issues, debugging techniques, and solutions.

    [:octicons-arrow-right-24: Troubleshooting](troubleshooting.md)

-   :material-chart-areaspline:{ .lg .middle } **Metrics & Alerting**

    ---

    Prometheus metrics reference and alerting rules.

    [:octicons-arrow-right-24: Metrics & Alerting](metrics.md)

-   :material-file-document:{ .lg .middle } **Manual Installation**

    ---

    Installation without Helm for special requirements.

    [:octicons-arrow-right-24: Manual Installation](manual-installation.md)

</div>

## Quick Diagnostics

Check controller health:

```bash
# Controller logs
kubectl logs --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system

# Gateway status
kubectl get gateway cloudflare-tunnel --namespace cloudflare-tunnel-system \
  --output jsonpath='{.status.conditions}'

# HTTPRoute status
kubectl get httproute --all-namespaces \
  --output custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,ACCEPTED:.status.parents[*].conditions[?(@.type=="Accepted")].status'
```

## Production Checklist

- [ ] Leader election enabled for HA deployments
- [ ] Resource limits configured
- [ ] Prometheus ServiceMonitor deployed
- [ ] Alerting rules configured
- [ ] Log aggregation set up
- [ ] Backup strategy for GatewayClassConfig secrets
