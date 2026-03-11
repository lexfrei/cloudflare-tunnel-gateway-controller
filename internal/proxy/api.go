package proxy

import (
	"encoding/json"
	"net/http"
)

// ConfigStatus is the response for GET /config.
type ConfigStatus struct {
	Version int64 `json:"version"`
	Ready   bool  `json:"ready"`
}

// ConfigAPI provides HTTP endpoints for runtime config management.
type ConfigAPI struct {
	router *Router
	mux    *http.ServeMux
}

// NewConfigAPI creates a ConfigAPI handler for the given Router.
func NewConfigAPI(router *Router) *ConfigAPI {
	api := &ConfigAPI{
		router: router,
		mux:    http.NewServeMux(),
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
	var cfg Config

	req.Body = http.MaxBytesReader(writer, req.Body, maxConfigBodySize)
	decoder := json.NewDecoder(req.Body)

	err := decoder.Decode(&cfg)
	if err != nil {
		http.Error(writer, "invalid JSON: "+err.Error(), http.StatusBadRequest)

		return
	}

	err = cfg.Validate()
	if err != nil {
		http.Error(writer, "invalid config: "+err.Error(), http.StatusBadRequest)

		return
	}

	err = a.router.UpdateConfig(&cfg)
	if err != nil {
		http.Error(writer, "failed to apply config: "+err.Error(), http.StatusInternalServerError)

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

	writer.Header().Set("Content-Type", "application/json")

	err := json.NewEncoder(writer).Encode(status)
	if err != nil {
		http.Error(writer, "failed to encode response", http.StatusInternalServerError)

		return
	}
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
