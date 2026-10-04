//go:build conformance

package conformance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// writeTwoClusterKubeconfig writes a kubeconfig with contexts "one" and "two"
// and the given current-context, and points KUBECONFIG at it.
func writeTwoClusterKubeconfig(t *testing.T, current string) {
	t.Helper()

	kubeconfig := clientcmdapi.NewConfig()
	for name, server := range map[string]string{"one": "https://one.test:6443", "two": "https://two.test:6443"} {
		kubeconfig.Clusters[name] = &clientcmdapi.Cluster{Server: server}
		kubeconfig.AuthInfos[name] = &clientcmdapi.AuthInfo{Token: name}
		kubeconfig.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	}

	kubeconfig.CurrentContext = current

	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(*kubeconfig, path))
	t.Setenv(clientcmd.RecommendedConfigPathEnvVar, path)
}

// The upstream suite builds its client from config.GetConfig(), which reads
// only the current context, so the selection has to reach it through there.
func TestSelectKubeContext(t *testing.T) {
	t.Run("named context wins over the current one", func(t *testing.T) {
		writeTwoClusterKubeconfig(t, "one")

		require.NoError(t, selectKubeContext(t, "two"))

		cfg, err := config.GetConfig()
		require.NoError(t, err)
		assert.Equal(t, "https://two.test:6443", cfg.Host)
	})

	t.Run("works with no current context", func(t *testing.T) {
		writeTwoClusterKubeconfig(t, "")

		require.NoError(t, selectKubeContext(t, "one"))

		cfg, err := config.GetConfig()
		require.NoError(t, err)
		assert.Equal(t, "https://one.test:6443", cfg.Host)
	})

	t.Run("unknown context is an error", func(t *testing.T) {
		writeTwoClusterKubeconfig(t, "one")

		require.ErrorContains(t, selectKubeContext(t, "kind-gone"), "kind-gone")
	})

	t.Run("empty name keeps the current context", func(t *testing.T) {
		writeTwoClusterKubeconfig(t, "one")
		before := os.Getenv(clientcmd.RecommendedConfigPathEnvVar)

		require.NoError(t, selectKubeContext(t, ""))

		assert.Equal(t, before, os.Getenv(clientcmd.RecommendedConfigPathEnvVar))
	})
}
