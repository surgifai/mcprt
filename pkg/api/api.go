// Package api exposes mcprt's REST control plane and web dashboard.
// All routes under /_api/ require a Bearer token when api_key is configured.
// The dashboard at /_dashboard/ is served as an embedded single-page app.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	headerContentType = "Content-Type"
	mediaTypeJSON     = "application/json"
)

// ServerStatus is the JSON representation of a supervised server.
type ServerStatus struct {
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Port         int       `json:"port"`
	PID          int       `json:"pid"`
	RestartCount int       `json:"restart_count"`
	LastSpawn    time.Time `json:"last_spawn,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Requests     int64     `json:"requests"`
	Errors       int64     `json:"errors"`
	RSS          int64     `json:"rss"` // resident memory in bytes
	// Lifecycle fields are additive and omitted until their live facts exist.
	AttestationVersion  string `json:"attestation_version,omitempty"`
	DaemonInstanceID    string `json:"daemon_instance_id,omitempty"`
	DaemonPID           int    `json:"daemon_pid,omitempty"`
	DaemonListenAddress string `json:"daemon_listen_address,omitempty"`
	ServerInstanceID    string `json:"server_instance_id,omitempty"`
}

// Controller is the interface the API uses to observe and control the runtime.
// Implemented by runtime.Runtime.
type Controller interface {
	Servers() []ServerStatus
	StartServer(ctx context.Context, name string) error
	StopServer(name string) error
}

// SecretStore is the interface the API uses to manage credentials.
// Implemented by secrets.Store.
type SecretStore interface {
	List() ([]string, error)
	Set(key, value string) error
	Delete(key string) error
}

// Handler serves the REST API and dashboard.
type Handler struct {
	ctrl   Controller
	store  SecretStore
	apiKey string
	mux    *http.ServeMux
}

// New creates the API handler. apiKey may be empty to disable auth.
func New(ctrl Controller, store SecretStore, apiKey string) *Handler {
	h := &Handler{ctrl: ctrl, store: store, apiKey: apiKey}
	h.mux = http.NewServeMux()

	// Health endpoint — no auth so the tray can probe without credentials.
	h.mux.HandleFunc("GET /_api/health", h.health)

	// Server control.
	h.mux.HandleFunc("GET /_api/servers", h.auth(h.listServers))
	h.mux.HandleFunc("POST /_api/servers/{name}/start", h.auth(h.startServer))
	h.mux.HandleFunc("POST /_api/servers/{name}/stop", h.auth(h.stopServer))

	// Secrets CRUD — returns key names only on GET, never values.
	h.mux.HandleFunc("GET /_api/secrets", h.auth(h.listSecrets))
	h.mux.HandleFunc("PUT /_api/secrets/{key}", h.auth(h.setSecret))
	h.mux.HandleFunc("DELETE /_api/secrets/{key}", h.auth(h.deleteSecret))

	// Dashboard.
	h.mux.HandleFunc("GET /_dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/_dashboard/", http.StatusMovedPermanently)
	})
	h.mux.HandleFunc("GET /_dashboard/", h.dashboard)

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Wide CORS — auth is enforced via Bearer token, not origin.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// auth wraps a handler with Bearer token validation.
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.apiKey != "" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token != h.apiKey {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, mediaTypeJSON)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) listServers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, mediaTypeJSON)
	json.NewEncoder(w).Encode(h.ctrl.Servers())
}

func (h *Handler) startServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.ctrl.StartServer(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) stopServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.ctrl.StopServer(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	w.Header().Set(headerContentType, mediaTypeJSON)
	json.NewEncoder(w).Encode(keys)
}

func (h *Handler) setSecret(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Value == "" {
		http.Error(w, "value must not be empty", http.StatusBadRequest)
		return
	}
	if err := h.store.Set(key, body.Value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := h.store.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
