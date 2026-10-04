//go:build conformance

package conformance

import (
	"path/filepath"
	"testing"

	"github.com/cockroachdb/errors"
	"k8s.io/client-go/tools/clientcmd"
)

var errUnknownKubeContext = errors.New("kubeconfig has no such context")

// selectKubeContext makes kubeContext the current context for the rest of the
// test. conformance.DefaultOptions loads its client through config.GetConfig,
// which takes no context argument, so the merged kubeconfig is rewritten with
// that context current into a temp file and KUBECONFIG is pointed at it. An
// empty name leaves the kubeconfig's own current context in effect.
func selectKubeContext(t *testing.T, kubeContext string) error {
	t.Helper()

	if kubeContext == "" {
		return nil
	}

	merged, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return errors.Wrap(err, "loading kubeconfig")
	}

	if _, ok := merged.Contexts[kubeContext]; !ok {
		return errors.Wrapf(errUnknownKubeContext, "%q", kubeContext)
	}

	merged.CurrentContext = kubeContext

	path := filepath.Join(t.TempDir(), "kubeconfig")

	err = clientcmd.WriteToFile(*merged, path)
	if err != nil {
		return errors.Wrap(err, "writing kubeconfig")
	}

	t.Setenv(clientcmd.RecommendedConfigPathEnvVar, path)

	return nil
}
