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
Proxy fullname
*/}}
{{- define "cf-tunnel-gw-ctrl.proxyFullname" -}}
{{ include "cf-tunnel-gw-ctrl.fullname" . }}-proxy
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
Validate PodDisruptionBudget configuration
*/}}
{{- define "cf-tunnel-gw-ctrl.validatePDB" -}}
{{- if .Values.podDisruptionBudget.enabled }}
{{- if and .Values.podDisruptionBudget.minAvailable .Values.podDisruptionBudget.maxUnavailable }}
{{- fail "ERROR: Cannot set both podDisruptionBudget.minAvailable and podDisruptionBudget.maxUnavailable. Use only one." }}
{{- end }}
{{- if and (eq (.Values.replicaCount | int) 1) .Values.podDisruptionBudget.minAvailable }}
{{- if or (eq (.Values.podDisruptionBudget.minAvailable | toString) "1") (eq (.Values.podDisruptionBudget.minAvailable | toString) "100%") }}
{{- fail "ERROR: PodDisruptionBudget with minAvailable=1 (or 100%) and replicaCount=1 will block all pod evictions. Set minAvailable=0, use maxUnavailable=1, or increase replicaCount to 2+" }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
