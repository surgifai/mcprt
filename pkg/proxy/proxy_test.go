package proxy

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/surgifai/mcprt/pkg/manifest"
	"github.com/surgifai/mcprt/pkg/supervisor"
)

func TestRewriteLocation(t *testing.T) {
	const server = "myserver"
	const reqHost = "127.0.0.1:9090"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "origin-relative path missing prefix gets prefix",
			in:   "/mcp/",
			want: "/myserver/mcp/",
		},
		{
			name: "origin-relative path with prefix passes through",
			in:   "/myserver/mcp/",
			want: "/myserver/mcp/",
		},
		{
			name: "origin-relative path equal to prefix passes through",
			in:   "/myserver",
			want: "/myserver",
		},
		{
			name: "absolute URL same host missing prefix gets prefix",
			in:   "http://127.0.0.1:9090/mcp/",
			want: "http://127.0.0.1:9090/myserver/mcp/",
		},
		{
			name: "absolute URL same host with prefix passes through",
			in:   "http://127.0.0.1:9090/myserver/mcp/",
			want: "http://127.0.0.1:9090/myserver/mcp/",
		},
		{
			name: "absolute URL same host preserves query and fragment",
			in:   "http://127.0.0.1:9090/mcp/?session=abc#x",
			want: "http://127.0.0.1:9090/myserver/mcp/?session=abc#x",
		},
		{
			name: "cross-host absolute URL passes through",
			in:   "https://other.example.com/mcp/",
			want: "https://other.example.com/mcp/",
		},
		{
			name: "relative path passes through (browser-resolved)",
			in:   "mcp/",
			want: "mcp/",
		},
		{
			name: "protocol-relative URL passes through",
			in:   "//evil.example.com/mcp/",
			want: "//evil.example.com/mcp/",
		},
		{
			name: "empty Location passes through",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteLocation(tc.in, server, reqHost)
			if got != tc.want {
				t.Errorf("rewriteLocation(%q, %q, %q) = %q, want %q",
					tc.in, server, reqHost, got, tc.want)
			}
		})
	}
}

func TestRewriteLocationDifferentReqHost(t *testing.T) {
	// When the proxy is reached via a hostname (not an IP), Location headers
	// from upstream that hard-code a different bind address (127.0.0.1:port)
	// are technically a different host and should pass through. Behavior we
	// document, not necessarily endorse.
	got := rewriteLocation("http://127.0.0.1:9090/mcp/", "myserver", "mcprt.local")
	want := "http://127.0.0.1:9090/mcp/"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestServeHTTPWaitsForConcurrentColdStart(t *testing.T) {
	port := freeTCPPort(t)
	script := `sleep 0.4; exec /usr/bin/python3 -c 'import http.server, os; H=type("H",(http.server.BaseHTTPRequestHandler,),{"do_GET":lambda self:(self.send_response(204),self.end_headers()),"log_message":lambda *args:None}); http.server.HTTPServer(("127.0.0.1", int(os.environ["MCPRT_PORT"])), H).serve_forever()'`
	spec := manifest.ServerSpec{
		Exec:          []string{"/bin/sh", "-c", script},
		Port:          port,
		HealthType:    "tcp",
		HealthTimeout: "2s",
	}
	sup := supervisor.New("slow", spec, port, supervisor.Options{
		Logger:        slog.Default(),
		HealthTimeout: 2 * time.Second,
		KillGrace:     time.Second,
	})
	defer sup.Stop()

	handler := NewHandler(slog.Default(), 2*time.Second)
	handler.Register("slow", &ServerEntry{
		Sup: sup,
		Ref: NewRefCounter(time.Second, func() {
			_ = sup.Stop()
		}),
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	firstDone := make(chan int, 1)
	go func() {
		resp, err := client.Get(server.URL + "/slow/")
		if err != nil {
			t.Errorf("first request failed: %v", err)
			firstDone <- 0
			return
		}
		defer resp.Body.Close()
		firstDone <- resp.StatusCode
	}()

	time.Sleep(100 * time.Millisecond)
	resp, err := client.Get(server.URL + "/slow/")
	if err != nil {
		t.Fatalf("concurrent request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadGateway {
		t.Fatalf("concurrent request proxied before upstream was ready: status %d", resp.StatusCode)
	}

	if status := <-firstDone; status == http.StatusBadGateway || status == 0 {
		t.Fatalf("first request returned status %d", status)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
