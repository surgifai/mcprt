package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type statusController struct {
	servers []ServerStatus
}

func (c statusController) Servers() []ServerStatus                     { return c.servers }
func (statusController) StartServer(_ context.Context, _ string) error { return nil }
func (statusController) StopServer(_ string) error                     { return nil }

func TestListServersKeepsArrayAndAuthenticatesAdditiveStatus(t *testing.T) {
	lastSpawn := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	handler := New(statusController{servers: []ServerStatus{{
		Name:                "surgifai-coderag",
		State:               "running",
		Port:                19042,
		PID:                 42001,
		RestartCount:        2,
		LastSpawn:           lastSpawn,
		Requests:            12,
		Errors:              1,
		RSS:                 4096,
		AttestationVersion:  "mcprt.workflow-owner-status.v1",
		DaemonInstanceID:    "07be3c0d-854c-4070-ae8c-07bc81ce2115",
		DaemonPID:           41000,
		DaemonListenAddress: "127.0.0.1:9090",
		ServerInstanceID:    "7d25d457-7b11-48b7-a98b-d53605ee0252",
	}}}, nil, "status-test-key")

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/_api/servers", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status request = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if strings.Contains(unauthorized.Body.String(), "attestation_version") || strings.Contains(unauthorized.Body.String(), "daemon_instance_id") {
		t.Fatalf("unauthorized response exposed status fields: %s", unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/_api/servers", nil)
	request.Header.Set("Authorization", "Bearer status-test-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized status request = %d, want %d", response.Code, http.StatusOK)
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode status array: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("status array has %d rows, want 1", len(rows))
	}
	for _, field := range []string{
		"name", "state", "port", "pid", "restart_count", "last_spawn",
		"requests", "errors", "rss",
	} {
		if _, ok := rows[0][field]; !ok {
			t.Errorf("legacy status field %q is missing", field)
		}
	}
	for _, field := range []string{
		"attestation_version", "daemon_instance_id", "daemon_pid",
		"daemon_listen_address", "server_instance_id",
	} {
		if _, ok := rows[0][field]; !ok {
			t.Errorf("additive status field %q is missing", field)
		}
	}
	body := strings.TrimSpace(response.Body.String())
	if len(body) == 0 || body[0] != '[' {
		t.Fatalf("status response no longer has the legacy array shape: %s", response.Body.String())
	}
}
