package helm

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/cockroachdb/errors"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

const (
	DefaultChartRef = "oci://ghcr.io/lexfrei/charts/cloudflare-tunnel"
)

type Manager struct {
	settings       *cli.EnvSettings
	registryClient *registry.Client
	metrics        metrics.Collector
	logger         *slog.Logger

	chartCache   chart.Charter
	chartVersion string
	cacheMu      sync.RWMutex
}

func NewManager(metricsCollector metrics.Collector, logger *slog.Logger) (*Manager, error) {
	settings := cli.New()

	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(false),
		registry.ClientOptEnableCache(true),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create registry client")
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		settings:       settings,
		registryClient: registryClient,
		metrics:        metricsCollector,
		logger:         logger.With("component", "helm-manager"),
	}, nil
}

func (m *Manager) GetLatestVersion(_ context.Context, chartRef string) (string, error) {
	repo := extractRepoFromOCI(chartRef)

	tags, err := m.registryClient.Tags(repo)
	if err != nil {
		return "", errors.Wrap(err, "failed to get tags from registry")
	}

	if len(tags) == 0 {
		return "", errors.New("no tags found in registry")
	}

	versions := make([]*semver.Version, 0, len(tags))

	for _, tag := range tags {
		ver, parseErr := semver.NewVersion(tag)
		if parseErr != nil {
			continue
		}

		if ver.Prerelease() == "" {
			versions = append(versions, ver)
		}
	}

	if len(versions) == 0 {
		return "", errors.New("no valid semver versions found")
	}

	sort.Sort(semver.Collection(versions))

	return versions[len(versions)-1].Original(), nil
}

func (m *Manager) LoadChart(_ context.Context, chartRef, version string) (chart.Charter, error) {
	m.cacheMu.RLock()

	if m.chartCache != nil && m.chartVersion == version {
		m.cacheMu.RUnlock()

		return m.chartCache, nil
	}

	m.cacheMu.RUnlock()

	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	if m.chartCache != nil && m.chartVersion == version {
		return m.chartCache, nil
	}

	m.logger.Info("pulling chart from registry", "ref", chartRef, "version", version)

	pullConfig := &action.Configuration{
		RegistryClient: m.registryClient,
	}

	pullClient := action.NewPull(action.WithConfig(pullConfig))
	pullClient.Settings = m.settings
	pullClient.Version = version
	pullClient.DestDir = os.TempDir()

	fullRef := chartRef
	if version != "" {
		fullRef = chartRef + ":" + version
	}

	output, err := pullClient.Run(fullRef)
	if err != nil {
		return nil, errors.Wrap(err, "failed to pull chart")
	}

	m.logger.Debug("chart pulled", "output", output)

	chartName := extractChartName(chartRef)
	chartPath := filepath.Join(os.TempDir(), chartName+"-"+version+".tgz")

	loadedChart, err := loader.Load(chartPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load chart")
	}

	m.chartCache = loadedChart
	m.chartVersion = version

	return loadedChart, nil
}

func (m *Manager) GetActionConfig(namespace string) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)

	err := actionConfig.Init(m.settings.RESTClientGetter(), namespace, "secret")
	if err != nil {
		return nil, errors.Wrap(err, "failed to init action config")
	}

	actionConfig.RegistryClient = m.registryClient

	return actionConfig, nil
}

func (m *Manager) Install(
	ctx context.Context,
	cfg *action.Configuration,
	releaseName, namespace string,
	loadedChart chart.Charter,
	values map[string]any,
) (release.Releaser, error) {
	startTime := time.Now()

	install := action.NewInstall(cfg)
	install.ReleaseName = releaseName
	install.Namespace = namespace
	install.CreateNamespace = false
	install.WaitStrategy = kube.StatusWatcherStrategy
	install.Timeout = installTimeout

	rel, err := install.RunWithContext(ctx, loadedChart, values)
	if err != nil {
		m.metrics.RecordHelmOperation(ctx, "install", "error", time.Since(startTime))
		m.metrics.RecordHelmError(ctx, "install", "install_failed")

		return nil, errors.Wrap(err, "failed to install release")
	}

	m.metrics.RecordHelmOperation(ctx, "install", "success", time.Since(startTime))
	m.recordChartInfo(ctx, loadedChart)

	return rel, nil
}

func (m *Manager) Upgrade(
	ctx context.Context,
	cfg *action.Configuration,
	releaseName string,
	loadedChart chart.Charter,
	values map[string]any,
) (release.Releaser, error) {
	startTime := time.Now()

	upgrade := action.NewUpgrade(cfg)
	upgrade.WaitStrategy = kube.StatusWatcherStrategy
	upgrade.Timeout = installTimeout
	upgrade.ReuseValues = false

	rel, err := upgrade.RunWithContext(ctx, releaseName, loadedChart, values)
	if err != nil {
		m.metrics.RecordHelmOperation(ctx, "upgrade", "error", time.Since(startTime))
		m.metrics.RecordHelmError(ctx, "upgrade", "upgrade_failed")

		return nil, errors.Wrap(err, "failed to upgrade release")
	}

	m.metrics.RecordHelmOperation(ctx, "upgrade", "success", time.Since(startTime))
	m.recordChartInfo(ctx, loadedChart)

	return rel, nil
}

func (m *Manager) Uninstall(ctx context.Context, cfg *action.Configuration, releaseName string) error {
	startTime := time.Now()

	uninstall := action.NewUninstall(cfg)
	uninstall.WaitStrategy = kube.StatusWatcherStrategy
	uninstall.Timeout = installTimeout

	_, err := uninstall.Run(releaseName)
	if err != nil {
		m.metrics.RecordHelmOperation(ctx, "uninstall", "error", time.Since(startTime))
		m.metrics.RecordHelmError(ctx, "uninstall", "uninstall_failed")

		return errors.Wrap(err, "failed to uninstall release")
	}

	m.metrics.RecordHelmOperation(ctx, "uninstall", "success", time.Since(startTime))

	return nil
}

func (m *Manager) GetRelease(cfg *action.Configuration, releaseName string) (release.Releaser, error) {
	get := action.NewGet(cfg)

	rel, err := get.Run(releaseName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get release")
	}

	return rel, nil
}

func (m *Manager) ReleaseExists(cfg *action.Configuration, releaseName string) bool {
	_, err := m.GetRelease(cfg, releaseName)

	return err == nil
}

func extractRepoFromOCI(chartRef string) string {
	const ociPrefix = "oci://"
	if len(chartRef) > len(ociPrefix) {
		return chartRef[len(ociPrefix):]
	}

	return chartRef
}

func extractChartName(chartRef string) string {
	return filepath.Base(chartRef)
}

func (m *Manager) recordChartInfo(ctx context.Context, loadedChart chart.Charter) {
	accessor, err := chart.NewAccessor(loadedChart)
	if err != nil {
		return
	}

	metadata := accessor.MetadataAsMap()
	name, _ := metadata["name"].(string)
	version, _ := metadata["version"].(string)
	appVersion, _ := metadata["appVersion"].(string)
	m.metrics.RecordHelmChartInfo(ctx, name, version, appVersion)
}
