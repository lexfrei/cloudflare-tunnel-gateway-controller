//go:build envtest

package controller

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

var (
	testEnv      *envtest.Environment
	envK8sClient client.Client
	envCfg       *rest.Config
	envScheme    *runtime.Scheme
)

func TestMain(m *testing.M) {
	// Find the CRD path relative to test file location.
	crdPath := filepath.Join("..", "..", "charts", "cloudflare-tunnel-gateway-controller", "crds")

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{crdPath},
		ErrorIfCRDPathMissing: false,
	}

	var err error

	envCfg, err = testEnv.Start()
	if err != nil {
		panic(err)
	}

	// Build scheme with all required types.
	envScheme = runtime.NewScheme()

	if err := gatewayv1.Install(envScheme); err != nil {
		panic(err)
	}

	if err := v1alpha1.AddToScheme(envScheme); err != nil {
		panic(err)
	}

	if err := corev1.AddToScheme(envScheme); err != nil {
		panic(err)
	}

	envK8sClient, err = client.New(envCfg, client.Options{Scheme: envScheme})
	if err != nil {
		panic(err)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		panic(err)
	}

	os.Exit(code)
}
