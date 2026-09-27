package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

const shippedCRDDir = "../../charts/cloudflare-tunnel-gateway-controller/crds"

// shippedCRD loads the chart's copy of the named CRD.
func shippedCRD(t *testing.T, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	plural, group, _ := strings.Cut(name, ".")
	raw, err := os.ReadFile(filepath.Join(shippedCRDDir, group+"_"+plural+".yaml"))
	require.NoError(t, err)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	require.Equal(t, name, crd.Name)

	return &crd
}

func versionSchema(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, version string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()

	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == version {
			return crd.Spec.Versions[i].Schema.OpenAPIV3Schema
		}
	}

	t.Fatalf("CRD %s has no version %s", crd.Name, version)

	return nil
}

// TestSchemaGaps_ShippedCRDsDescribeEveryField pins the walk against the CRDs
// the chart ships: on a fresh install it must report nothing, or every
// controller start would log a false alarm. It also pins that the list of
// checked CRDs covers every CRD the chart ships.
func TestSchemaGaps_ShippedCRDsDescribeEveryField(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(shippedCRDDir)
	require.NoError(t, err)

	checked := make(map[string]bool, len(servedCRDs))
	for _, served := range servedCRDs {
		plural, group, _ := strings.Cut(served.name, ".")
		checked[group+"_"+plural+".yaml"] = true

		schema := versionSchema(t, shippedCRD(t, served.name), served.version)
		assert.Empty(t, schemaGaps(served.object, schema), "shipped %s", served.name)
	}

	for _, entry := range entries {
		assert.True(t, checked[entry.Name()], "chart ships %s but the startup check does not verify it", entry.Name())
	}
}

// TestSchemaGaps_PrunedFieldReported covers the upgrade that keeps an older
// CRD: a field the binary reads is absent from the stored schema, so the
// apiserver drops it on write.
func TestSchemaGaps_PrunedFieldReported(t *testing.T) {
	t.Parallel()

	for _, served := range servedCRDs {
		if served.name != gatewayClassConfigCRDName {
			continue
		}

		schema := versionSchema(t, shippedCRD(t, served.name), served.version)
		spec := schema.Properties["spec"]
		delete(spec.Properties, "maxDataPlanesPerNamespace")

		secretRef := spec.Properties["cloudflareCredentialsSecretRef"]
		delete(secretRef.Properties, "key")
		spec.Properties["cloudflareCredentialsSecretRef"] = secretRef
		schema.Properties["spec"] = spec

		assert.ElementsMatch(t,
			[]string{"spec.maxDataPlanesPerNamespace", "spec.cloudflareCredentialsSecretRef.key"},
			schemaGaps(served.object, schema))

		return
	}

	t.Fatalf("%s is not in servedCRDs", gatewayClassConfigCRDName)
}

// capturingLogger records every rendered log line.
func capturingLogger() (func() string, logr.Logger) {
	var (
		mu    sync.Mutex
		lines []string
	)

	logger := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()

		lines = append(lines, prefix+" "+args)
	}, funcr.Options{})

	return func() string {
		mu.Lock()
		defer mu.Unlock()

		return strings.Join(lines, "\n")
	}, logger
}

func crdClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// TestLogInstalledSchemaGaps_NamesEachMissingField drives the startup check
// against an apiserver holding an older GatewayClassConfig CRD.
func TestLogInstalledSchemaGaps_NamesEachMissingField(t *testing.T) {
	t.Parallel()

	objs := make([]client.Object, 0, len(servedCRDs))

	for _, served := range servedCRDs {
		crd := shippedCRD(t, served.name)
		if served.name == gatewayClassConfigCRDName {
			delete(versionSchema(t, crd, served.version).Properties["spec"].Properties, "maxDataPlanesPerNamespace")
		}

		objs = append(objs, crd)
	}

	output, logger := capturingLogger()
	logInstalledSchemaGaps(context.Background(), crdClient(t, objs...), logger)

	logged := output()
	assert.Contains(t, logged, "spec.maxDataPlanesPerNamespace")
	assert.Contains(t, logged, gatewayClassConfigCRDName)
	assert.Equal(t, 1, strings.Count(logged, "\n")+1, "only the pruned field is reported:\n%s", logged)
}

// TestLogInstalledSchemaGaps_ReportsUnreadableCRD pins that a CRD the check
// cannot read is reported rather than passed over as verified.
func TestLogInstalledSchemaGaps_ReportsUnreadableCRD(t *testing.T) {
	t.Parallel()

	output, logger := capturingLogger()
	logInstalledSchemaGaps(context.Background(), crdClient(t), logger)

	logged := output()
	for _, served := range servedCRDs {
		assert.Contains(t, logged, served.name)
	}
}
