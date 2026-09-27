package controller

import (
	"context"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

const gatewayClassConfigCRDName = "gatewayclassconfigs.cf.k8s.lex.la"

// servedCRDs are the CRDs this controller reads, each with the version it
// reads and the Go type that version must describe.
var servedCRDs = []struct {
	name    string
	version string
	object  any
}{
	{gatewayClassConfigCRDName, v1alpha1.GroupVersion.Version, v1alpha1.GatewayClassConfig{}},
	{"gatewayconfigs.cf.k8s.lex.la", v1alpha1.GroupVersion.Version, v1alpha1.GatewayConfig{}},
	{"externalbackends.cf.k8s.lex.la", v1alpha1.GroupVersion.Version, v1alpha1.ExternalBackend{}},
}

func schemaGaps(_ any, _ *apiextensionsv1.JSONSchemaProps) []string {
	return nil
}

func logInstalledSchemaGaps(_ context.Context, _ client.Reader, _ logr.Logger) {}
