{{- define "gvList" -}}
{{- $groupVersions := . -}}
# API Reference

<!-- Generated from api/v1alpha1 by `make generate`. Do not edit by hand. -->

Field-level reference for the project's own custom resources, generated from the Go types the CRDs are built from. For examples, status conditions and the Gateway API resources the controller watches, see the [CRD Reference](crd-reference.md).
{{ range $groupVersions }}
{{ template "gvDetails" . }}
{{- end }}
{{ "" }}
{{- end -}}
