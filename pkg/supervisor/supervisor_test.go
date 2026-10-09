package supervisor

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/surgifai/mcprt/pkg/manifest"
)

var serverUUIDV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestServerInstanceIDTracksSuccessfulChildLifetimes(t *testing.T) {
	sup := New("fixture", manifest.ServerSpec{
		Exec:       []string{"/bin/sh", "-c", "exec /bin/sleep 30"},
		HealthType: "none",
	}, 1, Options{Logger: slog.Default(), KillGrace: time.Second})
	t.Cleanup(func() { _ = sup.Stop() })

	if initial := sup.Stats(); initial.State != StateIdle || initial.PID != 0 || initial.ServerInstanceID != "" {
		t.Fatalf("idle supervisor exposed active process identity: %+v", initial)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("start first disposable child: %v", err)
	}
	first := sup.Stats()
	assertRunningChildStats(t, first, sup)
	if !serverUUIDV4Pattern.MatchString(first.ServerInstanceID) {
		t.Fatalf("first server instance ID is not a UUID v4: %q", first.ServerInstanceID)
	}

	if err := sup.Stop(); err != nil {
		t.Fatalf("stop first disposable child: %v", err)
	}
	stopped := sup.Stats()
	if stopped.State != StateIdle || stopped.PID != 0 || stopped.ServerInstanceID != "" {
		t.Fatalf("stopped supervisor retained active process identity: %+v", stopped)
	}

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("start second disposable child: %v", err)
	}
	second := sup.Stats()
	assertRunningChildStats(t, second, sup)
	if second.ServerInstanceID == first.ServerInstanceID {
		t.Fatalf("new child reused previous server instance ID %q", first.ServerInstanceID)
	}
	if err := sup.Stop(); err != nil {
		t.Fatalf("stop second disposable child: %v", err)
	}
}

func TestFailedSpawnHasNoActiveServerIdentity(t *testing.T) {
	sup := New("missing", manifest.ServerSpec{
		Exec:       []string{"/path/that/does/not/exist/mcprt-child"},
		HealthType: "none",
	}, 19001, Options{Logger: slog.Default()})
	if err := sup.Start(context.Background()); err == nil {
		t.Fatal("spawn of missing fixture executable unexpectedly succeeded")
	}
	failed := sup.Stats()
	if failed.State != StateIdle || failed.PID != 0 || failed.ServerInstanceID != "" {
		t.Fatalf("failed spawn exposed active process identity: %+v", failed)
	}
}

func assertRunningChildStats(t *testing.T, stats Stats, sup *Supervisor) {
	t.Helper()
	if stats.State != StateRunning {
		t.Fatalf("child state = %q, want %q", stats.State, StateRunning)
	}
	if stats.Port != sup.Port() || stats.Port <= 0 {
		t.Fatalf("reported backend port = %d, assigned port = %d", stats.Port, sup.Port())
	}
	if stats.PID <= 0 || stats.PID == stats.Port || stats.PID == os.Getpid() {
		t.Fatalf("reported child PID is not distinct from daemon/assigned port: %+v", stats)
	}
	if stats.LastSpawn.IsZero() {
		t.Fatalf("successful child has no last_spawn timestamp: %+v", stats)
	}
	if stats.ServerInstanceID == "" {
		t.Fatalf("successful child has no server instance ID: %+v", stats)
	}
}
