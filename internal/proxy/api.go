package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cockroachdb/errors"
)

// ConfigStatus is the response for GET /config.
type ConfigStatus struct {
	Version int64 `json:"version"`
	Ready   bool  `json:"ready"`
}

// ConfigAPI provides HTTP endpoints for runtime config management.
// When authToken is set, PUT /config requires Bearer token authentication.
type ConfigAPI struct {
	router    *Router
	mux       *http.ServeMux
	authToken string
}

// NewConfigAPI creates a ConfigAPI handler for the given Router.
// If authToken is non-empty, PUT /config requires "Authorization: Bearer <token>".
func NewConfigAPI(router *Router, authToken string) *ConfigAPI {
	api := &ConfigAPI{
		router:    router,
		mux:       http.NewServeMux(),
		authToken: authToken,
	}

	api.mux.HandleFunc("PUT /config", api.handlePutConfig)
	api.mux.HandleFunc("GET /config", api.handleGetConfig)
	api.mux.HandleFunc("GET /healthz", api.handleHealthz)
	api.mux.HandleFunc("GET /readyz", api.handleReadyz)

	return api
}

// ServeHTTP implements http.Handler.
func (a *ConfigAPI) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	a.mux.ServeHTTP(writer, req)
}

// maxConfigBodySize limits the request body for config updates (1 MiB).
const maxConfigBodySize = 1 << 20

func (a *ConfigAPI) handlePutConfig(writer http.ResponseWriter, req *http.Request) {
	if a.authToken != "" && !a.checkAuth(req) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	var cfg Config

	req.Body = http.MaxBytesReader(writer, req.Body, maxConfigBodySize)
	decoder := json.NewDecoder(req.Body)

	err := decoder.Decode(&cfg)
	if err != nil {
		slog.Warn("config API: invalid JSON", "error", err)
		http.Error(writer, "invalid JSON", http.StatusBadRequest)

		return
	}

	err = cfg.Validate()
	if err != nil {
		slog.Warn("config API: invalid config", "error", err)
		http.Error(writer, "invalid config", http.StatusBadRequest)

		return
	}

	err = a.router.UpdateConfig(&cfg)
	if err != nil {
		if errors.Is(err, errStaleVersion) {
			http.Error(writer, "stale config version", http.StatusConflict)

			return
		}

		slog.Error("config API: failed to apply config", "error", err)
		http.Error(writer, "internal server error", http.StatusInternalServerError)

		return
	}

	writer.WriteHeader(http.StatusOK)
}

func (a *ConfigAPI) handleGetConfig(writer http.ResponseWriter, _ *http.Request) {
	version := a.router.ConfigVersion()
	status := ConfigStatus{
		Version: version,
		Ready:   version > 0,
	}

	data, err := json.Marshal(status)
	if err != nil {
		http.Error(writer, "failed to encode response", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)

	_, writeErr := writer.Write(data)
	if writeErr != nil {
		slog.Error("failed to write config response", "error", writeErr)
	}
}

const bearerPrefix = "Bearer "

func (a *ConfigAPI) checkAuth(req *http.Request) bool {
	header := req.Header.Get("Authorization")
	if len(header) <= len(bearerPrefix) {
		return false
	}

	if !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return false
	}

	token := header[len(bearerPrefix):]

	return subtle.ConstantTimeCompare([]byte(token), []byte(a.authToken)) == 1
}

func (a *ConfigAPI) handleHealthz(writer http.ResponseWriter, _ *http.Request) {
	writer.WriteHeader(http.StatusOK)
}

func (a *ConfigAPI) handleReadyz(writer http.ResponseWriter, _ *http.Request) {
	if a.router.ConfigVersion() == 0 {
		http.Error(writer, "not ready: no config loaded", http.StatusServiceUnavailable)

		return
	}

	writer.WriteHeader(http.StatusOK)
}
