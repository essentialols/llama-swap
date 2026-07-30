package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestServer_NewLoggers(t *testing.T) {
	t.Run("proxy mode routes proxy into muxlog, discards upstream", func(t *testing.T) {
		mux, proxy, upstream := NewLoggers(config.LogToStdoutProxy)
		proxy.Info("PROXYLINE")
		upstream.Info("UPSTREAMLINE")
		h := string(mux.GetHistory())
		if !strings.Contains(h, "PROXYLINE") {
			t.Errorf("muxlog missing proxy line: %q", h)
		}
		if strings.Contains(h, "UPSTREAMLINE") {
			t.Errorf("muxlog should not contain upstream line: %q", h)
		}
	})

	t.Run("both mode routes proxy and upstream into muxlog", func(t *testing.T) {
		mux, proxy, upstream := NewLoggers(config.LogToStdoutBoth)
		proxy.Info("PROXYLINE")
		upstream.Info("UPSTREAMLINE")
		h := string(mux.GetHistory())
		if !strings.Contains(h, "PROXYLINE") || !strings.Contains(h, "UPSTREAMLINE") {
			t.Errorf("muxlog history = %q", h)
		}
	})

	t.Run("none mode discards everything from muxlog", func(t *testing.T) {
		mux, proxy, upstream := NewLoggers(config.LogToStdoutNone)
		proxy.Info("PROXYLINE")
		upstream.Info("UPSTREAMLINE")
		if len(mux.GetHistory()) != 0 {
			t.Errorf("muxlog should be empty, got %q", mux.GetHistory())
		}
	})
}

// TestServer_NewLoggers_UpstreamLogFile covers UpstreamLogFileEnv: upstream
// output must also land in the named file, in every logToStdout mode, without
// leaking proxy lines into it. The default "proxy" mode is the important case:
// there the upstream monitor's only other storage is its 100KiB ring buffer.
func TestServer_NewLoggers_UpstreamLogFile(t *testing.T) {
	for _, mode := range []string{
		config.LogToStdoutProxy,
		config.LogToStdoutBoth,
		config.LogToStdoutUpstream,
		config.LogToStdoutNone,
	} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upstream.log")
			t.Setenv(UpstreamLogFileEnv, path)

			_, proxy, upstream := NewLoggers(mode)
			upstream.Info("UPSTREAMLINE")
			proxy.Info("PROXYLINE")

			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("upstream log file not written: %v", err)
			}
			if !strings.Contains(string(b), "UPSTREAMLINE") {
				t.Errorf("file missing upstream line: %q", b)
			}
			if strings.Contains(string(b), "PROXYLINE") {
				t.Errorf("proxy line leaked into upstream file: %q", b)
			}
		})
	}

	t.Run("unset env writes no file and still builds loggers", func(t *testing.T) {
		t.Setenv(UpstreamLogFileEnv, "")
		if sink := upstreamFileSink(); sink != nil {
			t.Errorf("expected nil sink when env is empty, got %T", sink)
		}
	})

	t.Run("unopenable path is non-fatal", func(t *testing.T) {
		t.Setenv(UpstreamLogFileEnv, filepath.Join(t.TempDir(), "no-such-dir", "upstream.log"))
		_, _, upstream := NewLoggers(config.LogToStdoutProxy)
		if upstream == nil {
			t.Fatal("upstream logger must still be constructed when the file cannot be opened")
		}
		upstream.Info("STILLWORKS") // must not panic
	})
}

func TestServer_HandleLogs_Plain(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.muxlog.Write([]byte("a log line"))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/logs", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if w.Body.String() != "a log line" {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestServer_HandleLogs_HTMLRedirect(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	req := httptest.NewRequest(http.MethodGet, "/logs", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/ui/" {
		t.Errorf("Location = %q, want /ui/", got)
	}
}

func TestServer_ClientIP(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*http.Request)
		want  string
	}{
		{"remote addr", func(r *http.Request) { r.RemoteAddr = "10.0.0.5:1234" }, "10.0.0.5"},
		{"x-forwarded-for", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
		}, "1.2.3.4"},
		{"x-real-ip", func(r *http.Request) { r.Header.Set("X-Real-IP", "9.9.9.9") }, "9.9.9.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = ""
			c.setup(r)
			if got := clientIP(r); got != c.want {
				t.Errorf("clientIP() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestServer_RequestLogMiddleware(t *testing.T) {
	proxylog := logmon.NewWriter(io.Discard)
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("hello"))
	})
	mw := CreateRequestLogMiddleware(proxylog)

	t.Run("logs request", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = "192.168.1.1:5000"
		mw(final).ServeHTTP(httptest.NewRecorder(), r)

		line := string(proxylog.GetHistory())
		for _, want := range []string{"192.168.1.1", "POST /v1/chat/completions", "201", "5"} {
			if !strings.Contains(line, want) {
				t.Errorf("log line %q missing %q", line, want)
			}
		}
	})

	for _, path := range []string{"/wol-health", "/api/performance", "/metrics"} {
		t.Run("skips "+path, func(t *testing.T) {
			skipLog := logmon.NewWriter(io.Discard)
			skipMW := CreateRequestLogMiddleware(skipLog)
			skipMW(final).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
			if len(skipLog.GetHistory()) != 0 {
				t.Errorf("%s should not be logged; got %q", path, skipLog.GetHistory())
			}
		})
	}
}

// TestServer_RequestLogMiddleware_WebSocketUpgrade verifies that the access-log
// middleware (which wraps responses in statusRecorder) does not break websocket
// upgrades proxied through httputil.ReverseProxy. ReverseProxy requires the
// ResponseWriter to implement http.Hijacker to take over the connection; if
// statusRecorder does not forward Hijack, the upgrade is refused with 502.
func TestServer_RequestLogMiddleware_WebSocketUpgrade(t *testing.T) {
	// Upstream: complete the upgrade handshake then echo bytes back. This
	// stands in for an upstream that speaks websocket; ReverseProxy only cares
	// about the 101 response and then copies raw bytes both ways.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("upstream ResponseWriter is not an http.Hijacker")
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		// Echo whatever the client sends.
		buf := make([]byte, 64)
		n, err := brw.Read(buf)
		if err != nil {
			return
		}
		brw.Write(buf[:n])
		brw.Flush()
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// Front server: ReverseProxy wrapped in the access-log middleware, which is
	// the production statusRecorder-wrapped path.
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	mw := CreateRequestLogMiddleware(logmon.NewWriter(io.Discard))
	front := httptest.NewServer(mw(proxy))
	defer front.Close()

	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("parse front URL: %v", err)
	}

	conn, err := net.DialTimeout("tcp", frontURL.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := "GET / HTTP/1.1\r\n" +
		"Host: " + frontURL.Host + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("websocket upgrade failed: status line = %q, want 101 Switching Protocols", strings.TrimSpace(statusLine))
	}

	// Drain the rest of the response headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// Verify bytes flow through the hijacked connection.
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != "ping" {
		t.Errorf("echo = %q, want %q", echo, "ping")
	}
}
