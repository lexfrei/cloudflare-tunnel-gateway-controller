---
name: Bug Report
about: Report a bug or unexpected behavior
title: '[BUG] '
labels: bug
assignees: ''
---

## Description

A clear and concise description of what the bug is.

## Steps to Reproduce

1. Deploy controller with '...'
2. Create Gateway resource '...'
3. Create HTTPRoute '...'
4. See error

## Expected Behavior

What you expected to happen.

## Actual Behavior

What actually happened.

## Environment

- **Controller Version**: [e.g., v0.1.0]
- **Kubernetes Version**: [e.g., v1.31.0]
- **Gateway API Version**: [e.g., v1.4.0]
- **cloudflared Version**: [e.g., 2024.11.0]
- **Deployment Method**: [Helm/Manual/Other]

## Logs

<details>
<summary>Controller logs</summary>

```text
Paste controller logs here (kubectl logs ...)
```

</details>

<details>
<summary>Gateway/HTTPRoute status (if relevant)</summary>

```yaml
Paste kubectl get gateway -o yaml or kubectl get httproute -o yaml
```

</details>

## Additional Context

Add any other context about the problem here (screenshots, configuration files, etc.).
