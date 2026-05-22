//go:build darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/getlantern/systray"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// TrayConfig is read from ~/.config/mcprt/tray.toml on the main (non-headless) machine.
type TrayConfig struct {
	Remote RemoteConfig `toml:"remote"`
	API    APIConfig    `toml:"api"`
}

type RemoteConfig struct {
	Host             string `toml:"host"`
	User             string `toml:"user"`
	Key              string `toml:"key"`
	SSHPort          int    `toml:"ssh_port"`
	RemoteMcprtPort  int    `toml:"remote_mcprt_port"`
	LocalTunnelPort  int    `toml:"local_tunnel_port"`
}

type APIConfig struct {
	Key string `toml:"key"`
}

func trayCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "tray",
		Short: "macOS menubar app — shows server status and manages the SSH tunnel to the headless machine",
		Long: `mcprt tray connects to a remote mcprt daemon via SSH tunnel and shows
server status in the macOS menubar. Requires a tray.toml config file.

Example tray.toml (~/.config/mcprt/tray.toml):

  [remote]
  host             = "mac-mini.local"
  user             = "victor"
  key              = "~/.ssh/id_ed25519"
  ssh_port         = 22
  remote_mcprt_port = 9090
  local_tunnel_port = 19090

  [api]
  key = "your-api-key"   # or use ${MCPRT_API_KEY} resolved from local Keychain
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadTrayConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("loading tray config: %w", err)
			}
			if cfg.Remote.Host == "" {
				return fmt.Errorf("remote.host is required in tray.toml")
			}
			t := newTray(cfg)
			systray.Run(t.onReady, t.onExit)
			return nil
		},
	}
	home, _ := os.UserHomeDir()
	cmd.Flags().StringVar(&cfgPath, "config", filepath.Join(home, ".config", "mcprt", "tray.toml"), "tray config path")
	return cmd
}

type tray struct {
	cfg    TrayConfig
	client *http.Client
	base   string // http://127.0.0.1:<local_tunnel_port>
	logger *slog.Logger

	// menu items kept alive to allow updates
	mTitle   *systray.MenuItem
	mServers []*systray.MenuItem // one per server
	mRecon   *systray.MenuItem
	mDash    *systray.MenuItem
	mQuit    *systray.MenuItem

	// tunnel lifecycle
	sshClient  *ssh.Client
	tunnelLn   net.Listener
	cancelTunnel context.CancelFunc
}

func newTray(cfg TrayConfig) *tray {
	port := cfg.Remote.LocalTunnelPort
	if port == 0 {
		port = 19090
	}
	t := &tray{
		cfg:    cfg,
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		logger: slog.Default(),
	}
	t.client = &http.Client{Timeout: 5 * time.Second}
	return t
}

func (t *tray) onReady() {
	systray.SetTitle("mcprt")
	systray.SetTooltip("mcprt — MCP server manager")
	systray.SetIcon(trayIcon())

	t.mTitle = systray.AddMenuItem("Connecting…", "")
	t.mTitle.Disable()
	systray.AddSeparator()
	systray.AddSeparator()
	t.mRecon = systray.AddMenuItem("⟳ Reconnect tunnel", "Re-establish SSH tunnel")
	t.mDash = systray.AddMenuItem("Open Dashboard…", "Open mcprt dashboard in browser")
	systray.AddSeparator()
	t.mQuit = systray.AddMenuItem("Quit mcprt tray", "")

	go t.handleClicks()
	go t.connectTunnel()
	go t.pollLoop()
}

func (t *tray) onExit() {
	if t.cancelTunnel != nil {
		t.cancelTunnel()
	}
	if t.tunnelLn != nil {
		t.tunnelLn.Close()
	}
	if t.sshClient != nil {
		t.sshClient.Close()
	}
}

func (t *tray) handleClicks() {
	for {
		select {
		case <-t.mRecon.ClickedCh:
			go t.connectTunnel()
		case <-t.mDash.ClickedCh:
			openBrowser(t.base + "/_dashboard/")
		case <-t.mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func (t *tray) connectTunnel() {
	t.mTitle.SetTitle("Connecting…")
	systray.SetTitle("mcprt ···")

	// Close any existing tunnel.
	if t.cancelTunnel != nil {
		t.cancelTunnel()
	}
	if t.tunnelLn != nil {
		t.tunnelLn.Close()
	}
	if t.sshClient != nil {
		t.sshClient.Close()
	}

	cfg := t.cfg.Remote
	sshPort := cfg.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}
	remotePort := cfg.RemoteMcprtPort
	if remotePort == 0 {
		remotePort = 9090
	}
	localPort := cfg.LocalTunnelPort
	if localPort == 0 {
		localPort = 19090
	}

	keyPath := expandHomePath(cfg.Key)
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.mTitle.SetTitle(fmt.Sprintf("✗ key not found: %s", cfg.Key))
		return
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		t.mTitle.SetTitle("✗ invalid SSH key")
		return
	}

	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // acceptable for localhost tunnel
		Timeout:         10 * time.Second,
	}

	sshAddr := fmt.Sprintf("%s:%d", cfg.Host, sshPort)
	client, err := ssh.Dial("tcp", sshAddr, sshCfg)
	if err != nil {
		t.mTitle.SetTitle(fmt.Sprintf("✗ SSH: %v", err))
		systray.SetTitle("mcprt ✗")
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		t.mTitle.SetTitle(fmt.Sprintf("✗ local port %d in use", localPort))
		client.Close()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.sshClient = client
	t.tunnelLn = ln
	t.cancelTunnel = cancel

	remoteAddr := fmt.Sprintf("127.0.0.1:%d", remotePort)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				remote, err := client.Dial("tcp", remoteAddr)
				if err != nil {
					c.Close()
					return
				}
				done := make(chan struct{}, 2)
				go func() { io.Copy(remote, c); done <- struct{}{} }()
				go func() { io.Copy(c, remote); done <- struct{}{} }()
				<-done
				c.Close()
				remote.Close()
			}(conn)
		}
	}()

	// Watch for SSH disconnect.
	go func() {
		select {
		case <-ctx.Done():
		case err := <-waitSSH(client):
			t.logger.Warn("SSH connection dropped", "err", err)
			t.mTitle.SetTitle("⚡ tunnel dropped — reconnecting…")
			time.Sleep(3 * time.Second)
			go t.connectTunnel()
		}
	}()

	t.mTitle.SetTitle(fmt.Sprintf("⬤ %s (tunnel open)", cfg.Host))
}

func waitSSH(c *ssh.Client) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- c.Wait() }()
	return ch
}

func (t *tray) pollLoop() {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for range tick.C {
		t.refresh()
	}
}

type serverStatus struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

func (t *tray) refresh() {
	req, _ := http.NewRequest("GET", t.base+"/_api/servers", nil)
	if t.cfg.API.Key != "" {
		req.Header.Set("Authorization", "Bearer "+t.cfg.API.Key)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		systray.SetTitle("mcprt ✗")
		return
	}
	defer resp.Body.Close()

	var servers []serverStatus
	if err := json.NewDecoder(resp.Body).Decode(&servers); err != nil {
		systray.SetTitle("mcprt ?")
		return
	}

	running := 0
	for _, s := range servers {
		if s.State == "running" {
			running++
		}
	}
	total := len(servers)
	// Title: "mcprt 3/5" (running/total)
	systray.SetTitle(fmt.Sprintf("mcprt %d/%d", running, total))
}

func trayIcon() []byte {
	// Generate a minimal 16x16 dark icon at runtime — avoids embedding binary assets.
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	bg := color.RGBA{28, 32, 44, 255}
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, bg)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func openBrowser(url string) {
	_ = exec.Command("open", url).Start()
}

func expandHomePath(s string) string {
	if len(s) >= 2 && s[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, s[2:])
	}
	return s
}

func loadTrayConfig(path string) (TrayConfig, error) {
	path = expandHomePath(path)
	var cfg TrayConfig
	cfg.Remote.SSHPort = 22
	cfg.Remote.RemoteMcprtPort = 9090
	cfg.Remote.LocalTunnelPort = 19090
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil // return defaults; user hasn't configured yet
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
