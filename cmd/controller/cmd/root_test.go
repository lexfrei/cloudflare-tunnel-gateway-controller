package cmd

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

func TestSetVersion(t *testing.T) {
	// Save original values
	originalVersion := version
	originalGitsha := gitsha

	defer func() {
		// Restore original values
		version = originalVersion
		gitsha = originalGitsha
	}()

	SetVersion("v1.2.3", "abc123")

	assert.Equal(t, "v1.2.3", version)
	assert.Equal(t, "abc123", gitsha)
}

func TestRootCmd_Properties(t *testing.T) {
	assert.Equal(t, "cloudflare-tunnel-gateway-controller", rootCmd.Use)
	assert.Equal(t, "Kubernetes Gateway API controller for Cloudflare Tunnel", rootCmd.Short)
	assert.True(t, rootCmd.SilenceUsage)
	assert.True(t, rootCmd.SilenceErrors)
}

func TestInitConfig_Defaults(t *testing.T) {
	// Reset viper state
	viper.Reset()

	// Call init config
	initConfig()

	// Verify defaults are set
	assert.Equal(t, "cloudflare-tunnel", viper.GetString("gateway-class-name"))
	assert.Equal(t, "cf.k8s.lex.la/tunnel-controller", viper.GetString("controller-name"))
	assert.Equal(t, ":8080", viper.GetString("metrics-addr"))
	assert.Equal(t, ":8081", viper.GetString("health-addr"))
	assert.Equal(t, "info", viper.GetString("log-level"))
	assert.Equal(t, "json", viper.GetString("log-format"))
	assert.False(t, viper.GetBool("leader-elect"))
	assert.Equal(t, "cloudflare-tunnel-gateway-controller-leader", viper.GetString("leader-election-name"))
}

func TestInitConfig_EnvPrefix(t *testing.T) {
	viper.Reset()

	// Set an env variable before initializing
	t.Setenv("CF_GATEWAY_CLASS_NAME", "test-class")

	// Initialize to pick up env
	initConfig()

	// Viper with AutomaticEnv reads from env automatically
	// But the env var name needs to match exactly with dash replacement
	// CF_GATEWAY_CLASS_NAME -> gateway-class-name
	// Viper replaces dashes with underscores in env lookup
	// So the env var should be CF_GATEWAY_CLASS_NAME
	// This test verifies the env prefix is set correctly
	assert.Equal(t, "CF", viper.GetEnvPrefix())
}

func TestSetupLogger_Levels(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		format   string
		notEmpty bool
	}{
		{
			name:     "debug_json",
			level:    "debug",
			format:   "json",
			notEmpty: true,
		},
		{
			name:     "info_json",
			level:    "info",
			format:   "json",
			notEmpty: true,
		},
		{
			name:     "warn_text",
			level:    "warn",
			format:   "text",
			notEmpty: true,
		},
		{
			name:     "error_text",
			level:    "error",
			format:   "text",
			notEmpty: true,
		},
		{
			name:     "unknown_level_defaults_to_info",
			level:    "unknown",
			format:   "json",
			notEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			viper.Set("log-level", tt.level)
			viper.Set("log-format", tt.format)

			logger := setupLogger()

			assert.NotNil(t, logger)
		})
	}
}

func TestResolveClusterDomain_Configured(t *testing.T) {
	viper.Reset()
	viper.Set("cluster-domain", "custom.local")

	logger := setupLogger()
	domain := resolveClusterDomain(logger)

	assert.Equal(t, "custom.local", domain)
}

func TestResolveClusterDomain_AutoDetect(t *testing.T) {
	viper.Reset()
	// Don't set cluster-domain, let it auto-detect

	logger := setupLogger()
	domain := resolveClusterDomain(logger)

	// Should either auto-detect or fallback to default
	assert.NotEmpty(t, domain)
}

func TestRootCmd_Flags(t *testing.T) {
	// Test that all expected flags are registered
	flags := rootCmd.Flags()

	// Command flags
	flag := flags.Lookup("cluster-domain")
	assert.NotNil(t, flag)
	assert.Empty(t, flag.DefValue)

	flag = flags.Lookup("gateway-class-name")
	assert.NotNil(t, flag)
	assert.Equal(t, "cloudflare-tunnel", flag.DefValue)

	flag = flags.Lookup("controller-name")
	assert.NotNil(t, flag)
	assert.Equal(t, "cf.k8s.lex.la/tunnel-controller", flag.DefValue)

	flag = flags.Lookup("metrics-addr")
	assert.NotNil(t, flag)
	assert.Equal(t, ":8080", flag.DefValue)

	flag = flags.Lookup("health-addr")
	assert.NotNil(t, flag)
	assert.Equal(t, ":8081", flag.DefValue)

	flag = flags.Lookup("leader-elect")
	assert.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)

	flag = flags.Lookup("leader-election-namespace")
	assert.NotNil(t, flag)
	assert.Empty(t, flag.DefValue)

	flag = flags.Lookup("leader-election-name")
	assert.NotNil(t, flag)
	assert.Equal(t, "cloudflare-tunnel-gateway-controller-leader", flag.DefValue)
}

func TestRootCmd_PersistentFlags(t *testing.T) {
	flags := rootCmd.PersistentFlags()

	flag := flags.Lookup("log-level")
	assert.NotNil(t, flag)
	assert.Equal(t, "info", flag.DefValue)

	flag = flags.Lookup("log-format")
	assert.NotNil(t, flag)
	assert.Equal(t, "json", flag.DefValue)
}

func TestVersion_InitialValues(t *testing.T) {
	// These are the default values in development
	// Note: Tests may run with different values if SetVersion was called
	// Just verify they are non-empty
	assert.NotEmpty(t, version)
	assert.NotEmpty(t, gitsha)
}
