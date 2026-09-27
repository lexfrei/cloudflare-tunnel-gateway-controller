package controller

import (
	"context"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

const gatewayClassConfigCRDName = "gatewayclassconfigs.cf.k8s.lex.la"

// servedCRD is a CRD this controller reads, the version it reads, and the Go
// type that version must describe.
type servedCRD struct {
	name    string
	version string
	object  any
}

func servedCRDs() []servedCRD {
	return []servedCRD{
		{gatewayClassConfigCRDName, v1alpha1.GroupVersion.Version, v1alpha1.GatewayClassConfig{}},
		{"gatewayconfigs.cf.k8s.lex.la", v1alpha1.GroupVersion.Version, v1alpha1.GatewayConfig{}},
		{"externalbackends.cf.k8s.lex.la", v1alpha1.GroupVersion.Version, v1alpha1.ExternalBackend{}},
	}
}

// logInstalledSchemaGaps logs, for each CRD this controller reads, every field
// of its Go type that the installed schema does not declare. helm upgrade
// never updates a chart's CRDs, so after an upgrade the apiserver can prune a
// field the new binary reads, and the controller then sees it unset. Where
// unset means "no limit", that fails open without a trace; this turns it into
// an error line at startup. A CRD that cannot be read is reported too, rather
// than passed over as verified.
func logInstalledSchemaGaps(ctx context.Context, reader client.Reader, logger logr.Logger) {
	for _, served := range servedCRDs() {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := reader.Get(ctx, types.NamespacedName{Name: served.name}, &crd); err != nil {
			logger.Error(err, "cannot read installed CRD to check it declares every field this controller reads",
				"crd", served.name)

			continue
		}

		schema := servedVersionSchema(&crd, served.version)
		if schema == nil {
			logger.Error(nil, "installed CRD has no schema for the version this controller reads; re-apply the chart's CRDs",
				"crd", served.name, "version", served.version)

			continue
		}

		for _, field := range schemaGaps(served.object, schema) {
			logger.Error(nil, "installed CRD does not declare a field this controller reads: the apiserver drops it on write, "+
				"so the controller treats it as unset; re-apply the chart's CRDs",
				"crd", served.name, "field", field)
		}
	}
}

func servedVersionSchema(crd *apiextensionsv1.CustomResourceDefinition, version string) *apiextensionsv1.JSONSchemaProps {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == version && crd.Spec.Versions[i].Schema != nil {
			return crd.Spec.Versions[i].Schema.OpenAPIV3Schema
		}
	}

	return nil
}

// schemaGaps returns the JSON paths of the fields of object's type that schema
// does not declare. It descends into a struct only where the schema lists
// properties for it, so a type with its own serialisation (a timestamp, a
// quantity, ObjectMeta) is taken as declared once its field is.
func schemaGaps(object any, schema *apiextensionsv1.JSONSchemaProps) []string {
	return appendSchemaGaps(nil, reflect.TypeOf(object), schema, "")
}

func appendSchemaGaps(gaps []string, typ reflect.Type, schema *apiextensionsv1.JSONSchemaProps, path string) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		if schema.Items == nil || schema.Items.Schema == nil {
			return gaps
		}

		return appendSchemaGaps(gaps, typ.Elem(), schema.Items.Schema, path+"[]")
	}

	if typ.Kind() != reflect.Struct || len(schema.Properties) == 0 {
		return gaps
	}

	for field := range typ.Fields() {
		gaps = appendFieldGaps(gaps, &field, schema, path)
	}

	return gaps
}

func appendFieldGaps(gaps []string, field *reflect.StructField, schema *apiextensionsv1.JSONSchemaProps, path string) []string {
	if !field.IsExported() && !field.Anonymous {
		return gaps
	}

	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")

	switch {
	case name == "-":
		return gaps
	case name == "" && field.Anonymous:
		return appendSchemaGaps(gaps, field.Type, schema, path)
	case name == "":
		name = field.Name
	}

	fieldPath := name
	if path != "" {
		fieldPath = path + "." + name
	}

	property, declared := schema.Properties[name]
	if !declared {
		return append(gaps, fieldPath)
	}

	return appendSchemaGaps(gaps, field.Type, &property, fieldPath)
}
