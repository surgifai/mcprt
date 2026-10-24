// Package runtime is the public library API for mcprt.
// It wires together the manifest loader, policy validator, supervisor registry,
// proxy handler, and fsnotify hot-reload loop.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/surgifai/mcprt/pkg/api"
	"github.com/surgifai/mcprt/pkg/manifest"
	"github.com/surgifai/mcprt/pkg/policy"
	"github.com/surgifai/mcprt/pkg/proxy"
	"github.com/surgifai/mcprt/pkg/secrets"
	"github.com/surgifai/mcprt/pkg/supervisor"
)

const mcprtConfigDir = ".config"

// Runtime is the central coordinator.
type Runtime struct {
	cfg          *manifest.Config
	manifestPath string
	logger       *slog.Logger

	mu       sync.RWMutex
	sups     map[string]*supervisor.Supervisor
	refs     map[string]*proxy.RefCounter
	ports    map[string]int
	nextPort int

	handler *proxy.Handler
	mux     *http.ServeMux
	store   *secrets.Store

	// These fields describe the active Serve listener and are guarded by mu.
	daemonInstanceID    string
	daemonPID           int
	daemonListenAddress string

	// Lifecycle hooks for embedders.
	OnSpawn          func(name string)
	OnExit           func(name string)
	OnRefcountChange func(name string, count int)
}

// Options configures the runtime.
type Options struct {
	ManifestPath string
	Logger       *slog.Logger
	LogDir       string
	BasePort     int // first port to allocate from (default 19000)
}

// New loads the manifest, resolves secrets, validates, and returns a configured Runtime.
// It does NOT start any servers or listen on any port yet.
func New(opts Options) (*Runtime, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ManifestPath == "" {
		opts.ManifestPath = defaultConfigPath("mcprt.toml")
	}

	cfg, store, err := loadConfig(opts.ManifestPath, logger)
	if err != nil {
		return nil, err
	}

	if err := validateConfigPolicy(cfg, logger, "policy violation", "policy warning"); err != nil {
		return nil, err
	}

	base := opts.BasePort
	if base == 0 {
		base = 19000
	}

	proxyHandler := proxy.NewHandler(logger, 10*time.Second)

	r := &Runtime{
		cfg:          cfg,
		manifestPath: opts.ManifestPath,
		logger:       logger,
		sups:         make(map[string]*supervisor.Supervisor),
		refs:         make(map[string]*proxy.RefCounter),
		ports:        make(map[string]int),
		nextPort:     base,
		handler:      proxyHandler,
		store:        store,
	}

	gracePeriod, err := time.ParseDuration(cfg.Runtime.GracePeriod)
	if err != nil {
		gracePeriod = 5 * time.Second
	}

	for name, spec := range cfg.Server {
		r.registerServer(name, spec, gracePeriod, opts.LogDir)
	}

	// Build top-level mux: /_api/ and /_dashboard/ are handled before the proxy.
	var apiStore api.SecretStore
	if store != nil {
		apiStore = store
	}
	apiHandler := api.New(r, apiStore, cfg.Runtime.APIKey)
	mux := http.NewServeMux()
	mux.Handle("/_api/", apiHandler)
	mux.Handle("/_dashboard/", apiHandler)
	mux.HandleFunc("/_dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/_dashboard/", http.StatusMovedPermanently)
	})
	mux.Handle("/", proxyHandler)
	r.mux = mux

	return r, nil
}

func defaultConfigPath(filename string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, mcprtConfigDir, "mcprt", filename)
}

func loadConfig(path string, logger *slog.Logger) (*manifest.Config, *secrets.Store, error) {
	cfg, err := manifest.Load(path)
	if err != nil {
		return nil, nil, err
	}
	store, vars, err := loadSecrets(cfg, logger)
	if err != nil {
		logger.Warn("secrets store unavailable; ${VAR} substitution skipped", "err", err)
		vars = map[string]string{}
	}
	logMissingVars(manifest.ExpandVars(cfg, vars), logger, "unresolved secret reference")
	return cfg, store, nil
}

func logMissingVars(missing []string, logger *slog.Logger, msg string) {
	for _, k := range missing {
		logger.Warn(msg, "var", k, "hint", "run: mcprt secrets set "+k+" <value>")
	}
}

func validateConfigPolicy(cfg *manifest.Config, logger *slog.Logger, errorMsg, warningMsg string) error {
	vv := policy.Validate(cfg)
	if policy.HasErrors(vv) {
		for _, v := range vv {
			if v.Severity == policy.SeverityError {
				logger.Error(errorMsg, "rule", v.Rule, "server", v.Server, "msg", v.Message)
			}
		}
		return fmt.Errorf("manifest has policy errors — see above")
	}
	for _, v := range vv {
		if v.Severity == policy.SeverityWarning {
			logger.Warn(warningMsg, "rule", v.Rule, "server", v.Server, "msg", v.Message)
		}
	}
	return nil
}

// secretsPath returns the resolved secrets file path: explicit config value or
// the default ~/.config/mcprt/secrets.env.
func secretsPath(cfg *manifest.Config) string {
	if cfg.Runtime.SecretsFile != "" {
		return cfg.Runtime.SecretsFile
	}
	return defaultConfigPath("secrets.env")
}

// loadSecrets opens the secrets file and returns the store + all resolved vars.
func loadSecrets(cfg *manifest.Config, logger *slog.Logger) (*secrets.Store, map[string]string, error) {
	store, err := secrets.Open(secretsPath(cfg))
	if err != nil {
		return nil, nil, err
	}
	vars, err := store.LoadAll()
	if err != nil {
		return store, map[string]string{}, fmt.Errorf("reading secrets file: %w", err)
	}
	return store, vars, nil
}

// Servers returns the current status of all registered servers as API types, sorted A-Z.
// Implements api.Controller.
func (r *Runtime) Servers() []api.ServerStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]api.ServerStatus, 0, len(r.sups))
	for _, sup := range r.sups {
		s := sup.Stats()
		status := api.ServerStatus{
			Name:         s.Name,
			State:        string(s.State),
			Port:         s.Port,
			PID:          s.PID,
			RestartCount: s.RestartCount,
			LastSpawn:    s.LastSpawn,
			LastError:    s.LastError,
			Requests:     s.Requests,
			Errors:       s.Errors,
			RSS:          s.RSS,
		}
		if r.daemonInstanceID != "" {
			status.AttestationVersion = "mcprt.workflow-owner-status.v1"
			status.DaemonInstanceID = r.daemonInstanceID
			status.DaemonPID = r.daemonPID
			status.DaemonListenAddress = r.daemonListenAddress
		}
		status.ServerInstanceID = s.ServerInstanceID
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StartServer spawns the named server. Implements api.Controller.
func (r *Runtime) StartServer(ctx context.Context, name string) error {
	r.mu.RLock()
	sup, ok := r.sups[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown server %q", name)
	}
	return sup.Start(ctx)
}

// StopServer stops the named server. Implements api.Controller.
func (r *Runtime) StopServer(name string) error {
	r.mu.RLock()
	sup, ok := r.sups[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown server %q", name)
	}
	return sup.Stop()
}

// Serve starts the HTTP listener and blocks until ctx is cancelled.
func (r *Runtime) Serve(ctx context.Context) error {
	r.mu.RLock()
	listenAddress := r.cfg.Runtime.Listen
	r.mu.RUnlock()
	srv := &http.Server{
		Addr:    listenAddress,
		Handler: r.mux,
	}

	ln, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddress, err)
	}
	daemonInstanceID, err := newInstanceID()
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("create daemon instance id: %w", err)
	}
	actualListenAddress := ln.Addr().String()
	r.mu.Lock()
	r.daemonInstanceID = daemonInstanceID
	r.daemonPID = os.Getpid()
	r.daemonListenAddress = actualListenAddress
	r.mu.Unlock()
	defer r.clearListenerFacts(daemonInstanceID)
	r.logger.Info("mcprt listening", "addr", actualListenAddress)

	// Start fsnotify hot-reload.
	go r.watchManifest(ctx)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		r.logger.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)

		// Stop all running servers.
		r.mu.RLock()
		names := make([]string, 0, len(r.sups))
		for n := range r.sups {
			names = append(names, n)
		}
		r.mu.RUnlock()
		for _, n := range names {
			r.mu.RLock()
			sup := r.sups[n]
			r.mu.RUnlock()
			_ = sup.Stop()
		}
		return nil
	}
}

func (r *Runtime) clearListenerFacts(instanceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.daemonInstanceID != instanceID {
		return
	}
	r.daemonInstanceID = ""
	r.daemonPID = 0
	r.daemonListenAddress = ""
}

func newInstanceID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(id[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// AllStats returns a snapshot of all registered servers.
func (r *Runtime) AllStats() []supervisor.Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]supervisor.Stats, 0, len(r.sups))
	for _, sup := range r.sups {
		out = append(out, sup.Stats())
	}
	return out
}

// --- internal ---

func (r *Runtime) registerServer(name string, spec manifest.ServerSpec, gracePeriod time.Duration, logDir string) {
	if spec.UpstreamURL != "" {
		r.registerRemoteServer(name, spec)
		return
	}

	port := r.allocPort(name, spec)

	ref := r.newRefCounter(name, gracePeriod)

	var healthTimeout time.Duration
	if spec.HealthTimeout != "" {
		if d, err := time.ParseDuration(spec.HealthTimeout); err == nil {
			healthTimeout = d
		}
	}

	sup := supervisor.New(name, spec, port, supervisor.Options{
		LogDir:        logDir,
		Logger:        r.logger,
		HealthTimeout: healthTimeout,
		OnStateChange: func(n string, from, to supervisor.State) {
			r.logger.Info("state change", "server", n, "from", from, "to", to)
			if to == supervisor.StateRunning && r.OnSpawn != nil {
				go r.OnSpawn(n)
			}
		},
	})

	r.mu.Lock()
	r.sups[name] = sup
	r.refs[name] = ref
	r.mu.Unlock()

	r.handler.Register(name, &proxy.ServerEntry{Sup: sup, Ref: ref, SpawnTimeout: healthTimeout})
}

func (r *Runtime) registerRemoteServer(name string, spec manifest.ServerSpec) {
	r.handler.Register(name, &proxy.ServerEntry{
		UpstreamURL: spec.UpstreamURL,
		Headers:     spec.Headers,
	})
}

func (r *Runtime) newRefCounter(name string, gracePeriod time.Duration) *proxy.RefCounter {
	return proxy.NewRefCounter(gracePeriod, func() {
		r.mu.RLock()
		sup, ok := r.sups[name]
		r.mu.RUnlock()
		if !ok || sup.CurrentState() != supervisor.StateRunning {
			return
		}
		r.logger.Info("refcount debounce expired, stopping server", "server", name)
		if err := sup.Stop(); err != nil {
			r.logger.Error("stop error", "server", name, "err", err)
		}
		if r.OnExit != nil {
			go r.OnExit(name)
		}
	})
}

func (r *Runtime) allocPort(name string, spec manifest.ServerSpec) int {
	if spec.Port > 0 {
		r.ports[name] = spec.Port
		return spec.Port
	}
	if p, ok := r.ports[name]; ok {
		return p
	}
	p := r.nextPort
	r.nextPort++
	r.ports[name] = p
	return p
}

func (r *Runtime) watchManifest(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		r.logger.Error("fsnotify init failed", "err", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(r.manifestPath); err != nil {
		r.logger.Error("fsnotify watch failed", "err", err, "path", r.manifestPath)
		return
	}

	var debounce *time.Timer
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(500*time.Millisecond, func() {
					r.reload()
				})
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			r.logger.Error("fsnotify error", "err", err)
		}
	}
}

func (r *Runtime) reload() {
	cfg, err := manifest.Load(r.manifestPath)
	if err != nil {
		r.logger.Error("hot-reload: parse failed", "err", err)
		return
	}

	// Re-resolve secrets on every reload so edits to secrets.env take effect.
	if r.store != nil {
		vars, err := r.store.LoadAll()
		if err != nil {
			r.logger.Warn("hot-reload: secrets read failed", "err", err)
			vars = map[string]string{}
		}
		logMissingVars(manifest.ExpandVars(cfg, vars), r.logger, "hot-reload: unresolved secret reference")
	}

	if validateConfigPolicy(cfg, r.logger, "hot-reload: policy error — refusing reload", "hot-reload: policy warning") != nil {
		return
	}

	gracePeriod, _ := time.ParseDuration(cfg.Runtime.GracePeriod)
	if gracePeriod == 0 {
		gracePeriod = 5 * time.Second
	}

	r.mu.RLock()
	existing := make(map[string]bool, len(r.sups))
	for n := range r.sups {
		existing[n] = true
	}
	r.mu.RUnlock()

	r.applyServerConfig(cfg, existing, gracePeriod)

	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	r.logger.Info("hot-reload: complete")
}

func (r *Runtime) applyServerConfig(cfg *manifest.Config, existing map[string]bool, gracePeriod time.Duration) {
	for name, spec := range cfg.Server {
		if r.applyServerSpec(name, spec, gracePeriod) {
			delete(existing, name)
		}
	}
	for name := range existing {
		r.removeServer(name)
	}
}

func (r *Runtime) applyServerSpec(name string, spec manifest.ServerSpec, gracePeriod time.Duration) bool {
	r.mu.RLock()
	sup, alreadyExists := r.sups[name]
	r.mu.RUnlock()
	if !alreadyExists {
		r.logger.Info("hot-reload: adding server", "server", name)
		r.registerServer(name, spec, gracePeriod, "")
		return false
	}
	if sup.CurrentState() == supervisor.StateRunning {
		r.logger.Info("hot-reload: spec changed, restarting running server", "server", name)
		_ = sup.Stop()
	}
	sup.UpdateSpec(spec)
	return true
}

func (r *Runtime) removeServer(name string) {
	r.logger.Info("hot-reload: removing server", "server", name)
	r.mu.RLock()
	sup := r.sups[name]
	r.mu.RUnlock()
	_ = sup.Stop()
	r.mu.Lock()
	delete(r.sups, name)
	delete(r.refs, name)
	r.mu.Unlock()
	r.handler.Deregister(name)
}
