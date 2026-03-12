package helm

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"helm.sh/helm/v4/pkg/action"
	v2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/release/common"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
)

const (
	testChartVersion = "1.0.0"
	testAppVersion   = "2.0.0"
)

// newTestActionConfig creates an action.Configuration with in-memory storage
// and a fake kube client for unit testing.
func newTestActionConfig() *action.Configuration {
	mem := driver.NewMemory()
	store := storage.Init(mem)

	return &action.Configuration{
		Releases:   store,
		KubeClient: &fake.PrintingKubeClient{Out: io.Discard},
	}
}

// newTestManager creates a Manager with noop metrics for testing.
func newTestManager() *Manager {
	return &Manager{
		metrics: cfmetrics.NewNoopCollector(),
		logger:  slog.Default(),
	}
}

// newTestRelease creates a minimal release for storage testing.
func newTestRelease(name, namespace string, version int) *releasev1.Release {
	return &releasev1.Release{
		Name:      name,
		Namespace: namespace,
		Version:   version,
		Info: &releasev1.Info{
			Status: common.StatusDeployed,
		},
		Chart: &v2.Chart{
			Metadata: &v2.Metadata{
				Name:       "test-chart",
				Version:    testChartVersion,
				AppVersion: testAppVersion,
				APIVersion: v2.APIVersionV2,
			},
		},
	}
}

// newTestChart creates a minimal chart for testing.
func newTestChart() *v2.Chart {
	return &v2.Chart{
		Metadata: &v2.Metadata{
			Name:       "test-chart",
			Version:    testChartVersion,
			AppVersion: testAppVersion,
			APIVersion: v2.APIVersionV2,
		},
	}
}

func TestManager_GetRelease_Found(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	rel := newTestRelease("my-release", "default", 1)
	require.NoError(t, cfg.Releases.Create(rel))

	mgr := newTestManager()

	result, err := mgr.GetRelease(cfg, "my-release")

	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestManager_GetRelease_NotFound(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()

	_, err := mgr.GetRelease(cfg, "nonexistent-release")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get release")
}

func TestManager_GetRelease_InvalidName(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()

	_, err := mgr.GetRelease(cfg, "INVALID_UPPERCASE")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get release")
}

func TestManager_ReleaseExists_True(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	rel := newTestRelease("existing-release", "default", 1)
	require.NoError(t, cfg.Releases.Create(rel))

	mgr := newTestManager()

	exists := mgr.ReleaseExists(cfg, "existing-release")

	assert.True(t, exists)
}

func TestManager_ReleaseExists_False(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()

	exists := mgr.ReleaseExists(cfg, "missing-release")

	assert.False(t, exists)
}

func TestManager_ReleaseExists_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		releaseName string
		setup       func(*action.Configuration)
		expected    bool
	}{
		{
			name:        "release exists",
			releaseName: "test-release",
			setup: func(cfg *action.Configuration) {
				rel := newTestRelease("test-release", "default", 1)
				_ = cfg.Releases.Create(rel)
			},
			expected: true,
		},
		{
			name:        "release does not exist",
			releaseName: "no-such-release",
			setup:       func(_ *action.Configuration) {},
			expected:    false,
		},
		{
			name:        "empty release name",
			releaseName: "",
			setup:       func(_ *action.Configuration) {},
			expected:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := newTestActionConfig()
			tt.setup(cfg)

			mgr := newTestManager()
			result := mgr.ReleaseExists(cfg, tt.releaseName)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestManager_Install_NilChart(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()

	_, err := mgr.Install(ctx, cfg, "test-release", "default", nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to install release")
}

func TestManager_Install_InvalidChartType(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()

	// A string is not a valid Charter implementation.
	_, err := mgr.Install(ctx, cfg, "test-release", "default", "not-a-chart", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to install release")
}

func TestManager_Install_UnreachableCluster(t *testing.T) {
	t.Parallel()

	mem := driver.NewMemory()
	store := storage.Init(mem)

	cfg := &action.Configuration{
		Releases: store,
		KubeClient: &fake.FailingKubeClient{
			ConnectionError: assert.AnError,
		},
	}

	mgr := newTestManager()
	ctx := context.Background()
	ch := newTestChart()

	_, err := mgr.Install(ctx, cfg, "test-release", "default", ch, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to install release")
}

func TestManager_Upgrade_NoExistingRelease(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()
	ch := newTestChart()

	_, err := mgr.Upgrade(ctx, cfg, "nonexistent-release", ch, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to upgrade release")
}

func TestManager_Upgrade_NilChart(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()

	_, err := mgr.Upgrade(ctx, cfg, "test-release", nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to upgrade release")
}

func TestManager_Upgrade_InvalidChartType(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()

	_, err := mgr.Upgrade(ctx, cfg, "test-release", "not-a-chart", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to upgrade release")
}

func TestManager_Uninstall_NoRelease(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()

	err := mgr.Uninstall(ctx, cfg, "nonexistent-release")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to uninstall release")
}

func TestManager_Uninstall_UnreachableCluster(t *testing.T) {
	t.Parallel()

	mem := driver.NewMemory()
	store := storage.Init(mem)

	cfg := &action.Configuration{
		Releases: store,
		KubeClient: &fake.FailingKubeClient{
			ConnectionError: assert.AnError,
		},
	}

	mgr := newTestManager()
	ctx := context.Background()

	err := mgr.Uninstall(ctx, cfg, "test-release")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to uninstall release")
}

func TestManager_Uninstall_Success(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	rel := newTestRelease("uninstall-me", "default", 1)
	require.NoError(t, cfg.Releases.Create(rel))

	mgr := newTestManager()
	ctx := context.Background()

	err := mgr.Uninstall(ctx, cfg, "uninstall-me")

	require.NoError(t, err)

	// Verify release no longer exists as deployed.
	exists := mgr.ReleaseExists(cfg, "uninstall-me")
	assert.False(t, exists)
}

func TestManager_GetActionConfig_ReturnsConfigWithRegistryClient(t *testing.T) {
	t.Parallel()

	mgr, err := NewManager(cfmetrics.NewNoopCollector(), nil)
	require.NoError(t, err)

	cfg, err := mgr.GetActionConfig("test-namespace")
	// If a kubeconfig is available (e.g., in CI or local env), this succeeds.
	// If not, it fails. Either way, we verify the behavior.
	if err != nil {
		assert.Contains(t, err.Error(), "failed to init action config")

		return
	}

	require.NotNil(t, cfg)
	assert.NotNil(t, cfg.RegistryClient, "registry client should be set on the config")
}

func TestManager_GetActionConfig_SetsRegistryClient(t *testing.T) {
	t.Parallel()

	mgr, err := NewManager(cfmetrics.NewNoopCollector(), nil)
	require.NoError(t, err)

	cfg, err := mgr.GetActionConfig("default")
	if err != nil {
		t.Skip("no kubeconfig available in test environment")
	}

	assert.Same(t, mgr.registryClient, cfg.RegistryClient,
		"action config should use the manager's registry client")
}

func TestManager_RecordChartInfo_ValidChart(t *testing.T) {
	t.Parallel()

	mgr := newTestManager()
	ctx := context.Background()
	ch := newTestChart()

	// Should not panic; metrics are recorded via noop collector.
	mgr.recordChartInfo(ctx, ch)
}

func TestManager_RecordChartInfo_NilChart(t *testing.T) {
	t.Parallel()

	mgr := newTestManager()
	ctx := context.Background()

	// nil chart should cause NewAccessor to fail; recordChartInfo handles
	// this gracefully by returning early.
	mgr.recordChartInfo(ctx, nil)
}

func TestManager_RecordChartInfo_InvalidCharterType(t *testing.T) {
	t.Parallel()

	mgr := newTestManager()
	ctx := context.Background()

	// A string is not a valid Charter; NewAccessor returns error.
	mgr.recordChartInfo(ctx, "not-a-chart")
}

func TestManager_RecordChartInfo_ChartWithoutMetadata(t *testing.T) {
	t.Parallel()

	mgr := newTestManager()
	ctx := context.Background()

	ch := &v2.Chart{
		Metadata: nil,
	}

	// Should not panic even with nil metadata.
	mgr.recordChartInfo(ctx, ch)
}

func TestManager_GetRelease_MultipleVersions(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()

	// Store two versions of the same release.
	rel1 := newTestRelease("multi-version", "default", 1)
	rel2 := newTestRelease("multi-version", "default", 2)

	require.NoError(t, cfg.Releases.Create(rel1))
	require.NoError(t, cfg.Releases.Create(rel2))

	mgr := newTestManager()

	// GetRelease (version=0) gets the latest revision.
	result, err := mgr.GetRelease(cfg, "multi-version")

	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestManager_LoadChart_CacheHitWithoutRegistry(t *testing.T) {
	t.Parallel()

	mgr := newTestManager()
	ctx := context.Background()

	// Pre-populate the cache to avoid hitting registry.
	ch := newTestChart()
	mgr.chartCache = ch
	mgr.chartVersion = testChartVersion

	result, err := mgr.LoadChart(ctx, "oci://example.com/charts/test", testChartVersion)

	require.NoError(t, err)
	assert.Same(t, ch, result)
}

func TestManager_LoadChart_CacheMissNewVersionReturnsOldOnMatch(t *testing.T) {
	t.Parallel()

	mgr := newTestManager()
	ctx := context.Background()

	ch := newTestChart()
	mgr.chartCache = ch
	mgr.chartVersion = testChartVersion

	// Same version should hit the cache (double-check read lock path).
	result, err := mgr.LoadChart(ctx, "oci://example.com/test", testChartVersion)

	require.NoError(t, err)
	assert.Same(t, ch, result)
}

func TestManager_Install_EmptyReleaseName(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()
	ch := newTestChart()

	_, err := mgr.Install(ctx, cfg, "", "default", ch, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to install release")
}

func TestManager_Upgrade_EmptyReleaseName(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()
	ch := newTestChart()

	_, err := mgr.Upgrade(ctx, cfg, "", ch, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to upgrade release")
}

func TestManager_Uninstall_EmptyReleaseName(t *testing.T) {
	t.Parallel()

	cfg := newTestActionConfig()
	mgr := newTestManager()
	ctx := context.Background()

	err := mgr.Uninstall(ctx, cfg, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to uninstall release")
}
