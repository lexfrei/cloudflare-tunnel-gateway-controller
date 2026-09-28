{{/*
Expand the name of the chart.
*/}}
{{- define "cf-tunnel-gw-ctrl.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cf-tunnel-gw-ctrl.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "cf-tunnel-gw-ctrl.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "cf-tunnel-gw-ctrl.labels" -}}
helm.sh/chart: {{ include "cf-tunnel-gw-ctrl.chart" . }}
{{ include "cf-tunnel-gw-ctrl.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "cf-tunnel-gw-ctrl.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cf-tunnel-gw-ctrl.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "cf-tunnel-gw-ctrl.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "cf-tunnel-gw-ctrl.fullname" . }}
{{- end }}
{{- end }}

{{/*
Image reference for a `{repository, tag, digest}` values block, called with a
dict of `image` and `appVersion`. A digest, when set, REPLACES the tag rather
than joining it, so the rendered reference carries the digest alone -- the
chart never emits the combined `repository:tag@digest` form, though that form
remains valid input through `tag`. Shared by the controller Deployment, the
proxy Deployment and the controller's --proxy-image flag.

Why a digest is worth setting, and what it does and does not pin, lives in
docs/reference/security.md under Container Image Verification -- one home, so
the reasoning cannot drift between copies.
*/}}
{{- define "cf-tunnel-gw-ctrl.imageRef" -}}
{{- if .image.digest -}}
{{- printf "%s@%s" .image.repository .image.digest -}}
{{- else -}}
{{- printf "%s:%s" .image.repository (.image.tag | default .appVersion) -}}
{{- end -}}
{{- end }}

{{/*
Proxy fullname
*/}}
{{- define "cf-tunnel-gw-ctrl.proxyFullname" -}}
{{- printf "%s-proxy" (include "cf-tunnel-gw-ctrl.fullname" . | trunc 57 | trimSuffix "-") }}
{{- end }}

{{/*
Proxy ServiceAccount name. Empty when the chart neither creates one nor is
given one, so the pod falls back to the namespace's default ServiceAccount.
*/}}
{{- define "cf-tunnel-gw-ctrl.proxyServiceAccountName" -}}
{{- $sa := .Values.proxy.serviceAccount | default dict }}
{{- if $sa.create }}
{{- $sa.name | default (include "cf-tunnel-gw-ctrl.proxyFullname" .) }}
{{- else }}
{{- $sa.name | default "" }}
{{- end }}
{{- end }}

{{/*
Proxy headless service name. The base is truncated BEFORE the suffixes are
appended so the full name stays within the 63-character DNS label limit and
the "-proxy-headless" suffix is never cut off (which would collide with the
proxy Service name). The controller's --proxy-endpoints flag uses this same
helper, so the DNS name it resolves always matches the rendered Service.
*/}}
{{- define "cf-tunnel-gw-ctrl.proxyHeadlessName" -}}
{{- printf "%s-proxy-headless" (include "cf-tunnel-gw-ctrl.fullname" . | trunc 48 | trimSuffix "-") }}
{{- end }}

{{/*
Shared-proxy config-API auth Secret name: the operator's authTokenSecretRef.name
when set (bring-your-own), otherwise the chart's deterministic naming
convention for the Secret the CONTROLLER generates and manages itself via a
live API call (internal/controller/proxy_auth_secret.go), not a chart
template -- a template-time `lookup` cannot see prior state under GitOps
controllers that render client-side (e.g. ArgoCD's default `helm template`).
deployment.yaml (via --proxy-auth-secret-ref) and deployment-proxy.yaml (via
a pod-level secretKeyRef) both resolve the name through this one helper so
they can never end up pointed at different Secrets.
*/}}
{{- define "cf-tunnel-gw-ctrl.proxyAuthTokenSecretName" -}}
{{- if .Values.proxy.authTokenSecretRef.name -}}
{{- .Values.proxy.authTokenSecretRef.name -}}
{{- else -}}
{{- printf "%s-auth-token" (include "cf-tunnel-gw-ctrl.proxyFullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Config API TLS Secrets, both created and managed by the controller at
runtime. The CA stays in the release namespace; the shared proxy mounts the
leaf. deployment.yaml, deployment-proxy.yaml and role-config-tls.yaml resolve
the names through these helpers so they cannot disagree.
*/}}
{{- define "cf-tunnel-gw-ctrl.configCASecretName" -}}
{{- printf "%s-config-ca" (include "cf-tunnel-gw-ctrl.fullname" .) -}}
{{- end }}

{{- define "cf-tunnel-gw-ctrl.proxyConfigTLSSecretName" -}}
{{- printf "%s-config-tls" (include "cf-tunnel-gw-ctrl.proxyFullname" .) -}}
{{- end }}

{{/*
"true" when config API TLS is on. Read through dig so a values set without
the key, as `helm upgrade --reuse-values` carries from an older release,
gets the default instead of a nil-pointer failure.
*/}}
{{- define "cf-tunnel-gw-ctrl.configAPITLSEnabled" -}}
{{- if dig "configAPITLS" "enabled" true (.Values.proxy | default dict) -}}true{{- end -}}
{{- end }}

{{/*
Proxy labels
*/}}
{{- define "cf-tunnel-gw-ctrl.proxyLabels" -}}
helm.sh/chart: {{ include "cf-tunnel-gw-ctrl.chart" . }}
{{ include "cf-tunnel-gw-ctrl.proxySelectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Proxy selector labels
*/}}
{{- define "cf-tunnel-gw-ctrl.proxySelectorLabels" -}}
app.kubernetes.io/name: {{ include "cf-tunnel-gw-ctrl.name" . }}-proxy
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: proxy
{{- end }}

{{/*
labelSelectorString renders a Kubernetes LabelSelector object in kubectl
label-selector syntax (matchLabels + In/NotIn/Exists/DoesNotExist
expressions). Every controller flag carrying a selector goes through it, so a
value and the flag derived from it cannot drift apart: the
--hostname-ownership-namespace-selector flag shares its value with the
ValidatingAdmissionPolicyBinding, and --controller-pod-selector is derived from
the controller's own pod labels. Empty selector renders "".

matchLabels keys are emitted in explicit sortAlpha order so the rendered flag
string is stable regardless of map iteration order — the value feeds a
container arg compared across reconciles, and a reordered string would churn
the Deployment. (Don't rely on text/template's implicit map-key sort here: a
refactor to sprig `keys` would silently drop it.)
*/}}
{{- define "cf-tunnel-gw-ctrl.labelSelectorString" -}}
{{- $selector := . | default dict -}}
{{- $terms := list -}}
{{- $matchLabels := $selector.matchLabels | default dict -}}
{{- range $key := (keys $matchLabels | sortAlpha) -}}
{{- $terms = append $terms (printf "%s=%v" $key (index $matchLabels $key)) -}}
{{- end -}}
{{- range ($selector.matchExpressions | default list) -}}
{{- if eq .operator "In" -}}
{{- $terms = append $terms (printf "%s in (%s)" .key (join "," .values)) -}}
{{- else if eq .operator "NotIn" -}}
{{- $terms = append $terms (printf "%s notin (%s)" .key (join "," .values)) -}}
{{- else if eq .operator "Exists" -}}
{{- $terms = append $terms .key -}}
{{- else if eq .operator "DoesNotExist" -}}
{{- $terms = append $terms (printf "!%s" .key) -}}
{{- else -}}
{{- fail (printf "unsupported matchExpressions operator %q in a label selector value (supported: In, NotIn, Exists, DoesNotExist)" .operator) -}}
{{- end -}}
{{- end -}}
{{- join "," $terms -}}
{{- end -}}

