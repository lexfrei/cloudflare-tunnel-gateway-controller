# Security Policy

## Supported Versions

| Version            | Supported          |
|--------------------|--------------------|
| Latest 3.x minor   | :white_check_mark: |
| Earlier 3.x minors | :x:                |
| < 3.0              | :x:                |

Fixes land on the latest 3.x minor; older minors are not backported.

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report vulnerabilities via email:

- **Email**: <f@lex.la>
- **GPG Key**: `F57F 85FC 7975 F22B BC3F 2504 9C17 3EB1 B531 AA1F`

### What to Include

- Type of vulnerability
- Full paths of affected source files
- Location of affected source code (tag/branch/commit)
- Step-by-step reproduction instructions
- Proof-of-concept or exploit code (if possible)
- Impact assessment

### Response Timeline

- **Initial Response**: Within 48 hours
- **Status Update**: Within 7 days
- **Fix Timeline**: Depends on severity

## Hardening and Verification

Deployment hardening lives in the [security reference](https://cf.k8s.lex.la/latest/reference/security/) on the documentation site: the controller's Kubernetes permissions and what they imply, API token scope, network policies, and how to verify the signed container images and Helm chart before deploying them.
