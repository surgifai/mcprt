package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestServePublishesActualListenerAndProcessLifetimeFacts(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "mcprt.toml")
	if err := os.WriteFile(manifestPath, []byte(runtimeManifest("127.0.0.1:0")), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	rt, err := New(Options{
		ManifestPath: manifestPath,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		BasePort:     freeRuntimePort(t),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}

	beforeListen := rt.Servers()
	if len(beforeListen) != 1 {
		t.Fatalf("got %d local status rows before listen, want 1", len(beforeListen))
	}
	if beforeListen[0].AttestationVersion != "" || beforeListen[0].DaemonInstanceID != "" || beforeListen[0].DaemonPID != 0 || beforeListen[0].DaemonListenAddress != "" || beforeListen[0].ServerInstanceID != "" {
		t.Fatalf("daemon facts were available before Serve: %+v", beforeListen[0])
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	firstServeFinished := false
	t.Cleanup(func() {
		cancel()
		if !firstServeFinished {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("first Serve did not stop during cleanup")
			}
		}
	})
	go func() { done <- rt.Serve(ctx) }()
	first := waitForListenerFacts(t, rt)
	if first.attestationVersion != "mcprt.workflow-owner-status.v1" {
		t.Fatalf("attestation version = %q", first.attestationVersion)
	}
	if !uuidV4Pattern.MatchString(first.instanceID) {
		t.Fatalf("daemon instance ID is not a UUID v4: %q", first.instanceID)
	}
	if first.pid != os.Getpid() {
		t.Fatalf("daemon PID = %d, want actual process PID %d", first.pid, os.Getpid())
	}
	if first.listenAddress == "127.0.0.1:0" {
		t.Fatalf("listener snapshot reported the requested address instead of the bound address: %q", first.listenAddress)
	}
	if host, port, err := net.SplitHostPort(first.listenAddress); err != nil || host != "127.0.0.1" || port == "0" {
		t.Fatalf("listener snapshot %q is not the actual ephemeral loopback address", first.listenAddress)
	}

	// A manifest reload can change the next Serve request, but cannot rewrite
	// the address of the listener that is already bound.
	if err := os.WriteFile(manifestPath, []byte(runtimeManifest("127.0.0.1:1")), 0o600); err != nil {
		t.Fatalf("write changed manifest: %v", err)
	}
	rt.reload()
	if current := readListenerFacts(rt); current != first {
		t.Fatalf("reload changed active listener facts: before=%+v after=%+v", first, current)
	}
	if err := os.WriteFile(manifestPath, []byte(runtimeManifest("127.0.0.1:0")), 0o600); err != nil {
		t.Fatalf("restore manifest: %v", err)
	}
	rt.reload()
	if current := readListenerFacts(rt); current != first {
		t.Fatalf("restoring manifest changed active listener facts: before=%+v after=%+v", first, current)
	}
	// The manifest watcher uses a 500ms debounce. Let both fixture edits settle
	// before starting the child so a delayed reload cannot stop that process.
	time.Sleep(700 * time.Millisecond)

	if err := rt.StartServer(context.Background(), "fixture"); err != nil {
		cancel()
		<-done
		firstServeFinished = true
		t.Fatalf("start disposable child: %v", err)
	}
	rows := rt.Servers()
	if len(rows) != 1 {
		t.Fatalf("got %d local status rows, want 1", len(rows))
	}
	row := rows[0]
	if row.AttestationVersion != "mcprt.workflow-owner-status.v1" || row.DaemonInstanceID != first.instanceID || row.DaemonPID != first.pid || row.DaemonListenAddress != first.listenAddress {
		t.Fatalf("status row does not carry the active listener snapshot: %+v", row)
	}
	supervised := rt.sups["fixture"].Stats()
	if row.State != "running" || row.Port != rt.sups["fixture"].Port() || row.PID != supervised.PID || row.PID <= 0 {
		t.Fatalf("status row did not preserve distinct running PID and assigned backend port: %+v", row)
	}
	if row.PID == row.DaemonPID {
		t.Fatalf("supervised child PID %d equals daemon PID %d", row.PID, row.DaemonPID)
	}
	if !uuidV4Pattern.MatchString(row.ServerInstanceID) {
		t.Fatalf("server instance ID is not a UUID v4: %q", row.ServerInstanceID)
	}

	cancel()
	err = <-done
	firstServeFinished = true
	if err != nil {
		t.Fatalf("Serve returned an error after cancellation: %v", err)
	}
	if current := readListenerFacts(rt); current != (listenerFacts{}) {
		t.Fatalf("listener facts remained after Serve returned: %+v", current)
	}
	if stopped := rt.Servers()[0]; stopped.ServerInstanceID != "" || stopped.DaemonInstanceID != "" {
		t.Fatalf("stopped row retained active process or listener identity: %+v", stopped)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	secondServeFinished := false
	t.Cleanup(func() {
		cancel2()
		if !secondServeFinished {
			select {
			case <-done2:
			case <-time.After(3 * time.Second):
				t.Error("second Serve did not stop during cleanup")
			}
		}
	})
	go func() { done2 <- rt.Serve(ctx2) }()
	second := waitForListenerFacts(t, rt)
	if second.instanceID == first.instanceID {
		t.Fatalf("new listener lifetime reused daemon instance ID %q", second.instanceID)
	}
	cancel2()
	err = <-done2
	secondServeFinished = true
	if err != nil {
		t.Fatalf("second Serve returned an error after cancellation: %v", err)
	}
}

type listenerFacts struct {
	attestationVersion string
	instanceID         string
	pid                int
	listenAddress      string
}

func readListenerFacts(rt *Runtime) listenerFacts {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	version := ""
	if rt.daemonInstanceID != "" {
		version = "mcprt.workflow-owner-status.v1"
	}
	return listenerFacts{
		attestationVersion: version,
		instanceID:         rt.daemonInstanceID,
		pid:                rt.daemonPID,
		listenAddress:      rt.daemonListenAddress,
	}
}

func waitForListenerFacts(t *testing.T, rt *Runtime) listenerFacts {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if facts := readListenerFacts(rt); facts.instanceID != "" {
			return facts
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Serve did not publish listener facts")
	return listenerFacts{}
}

func runtimeManifest(listen string) string {
	return fmt.Sprintf(`[runtime]
listen = %q

[server.fixture]
exec = ["/usr/bin/python3", "-c", "import http.server, os; H=type('H',(http.server.BaseHTTPRequestHandler,),{'do_GET':lambda self:(self.send_response(204),self.end_headers()),'log_message':lambda *args:None}); http.server.HTTPServer(('127.0.0.1', int(os.environ['FIXTURE_PORT'])), H).serve_forever()"]
env = { FIXTURE_PORT = "${MCPRT_PORT}" }
health_type = "tcp"
health_timeout = "2s"
acknowledged_stdio_safe = true
`, listen)
}

func freeRuntimePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve disposable backend port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release disposable backend port: %v", err)
	}
	return port
}
