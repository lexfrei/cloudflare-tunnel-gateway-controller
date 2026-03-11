package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvOrDefault_DefaultValue(t *testing.T) {
	t.Parallel()

	result := envOrDefault("NONEXISTENT_ENV_VAR_FOR_TESTING_12345", "fallback")
	assert.Equal(t, "fallback", result)
}

func TestEnvOrDefault_EnvSet(t *testing.T) {
	t.Setenv("TEST_ENV_OR_DEFAULT", "custom")

	result := envOrDefault("TEST_ENV_OR_DEFAULT", "fallback")
	assert.Equal(t, "custom", result)
}

func TestNewServer(t *testing.T) {
	t.Parallel()

	handler := http.NewServeMux()
	server := newServer(":0", handler)

	require.NotNil(t, server)
	assert.Equal(t, ":0", server.Addr)
	assert.Equal(t, readHeaderTimeout, server.ReadHeaderTimeout)
}

func TestConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ":8081", defaultConfigAddr)
	assert.Equal(t, ":8080", defaultProxyAddr)
}
