package reverselb

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	"github.com/hashicorp/yamux"
)

type adapterRuntime struct {
	mu      sync.Mutex
	target  string
	invalid chan struct{}
	enabled bool
	dials   int
	err     error
}

func (r *adapterRuntime) available(string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled
}

func (r *adapterRuntime) dial(ctx context.Context, binding string, _ tunnelcore.ConnectionInfo) (net.Conn, error) {
	if binding != "web" {
		return nil, fmt.Errorf("unexpected binding %s", binding)
	}
	r.mu.Lock()
	r.dials++
	target, invalid, err := r.target, r.invalid, r.err
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	return &adapterSessionConn{Conn: c, invalid: invalid}, nil
}

type adapterSessionConn struct {
	net.Conn
	invalid <-chan struct{}
}

func (c *adapterSessionConn) Done() <-chan struct{} { return c.invalid }

func (c *adapterSessionConn) CloseWrite() error {
	return c.Conn.(interface{ CloseWrite() error }).CloseWrite()
}

func adapterHTTPServer(t *testing.T, protocol string, handler http.Handler) (*httptest.Server, Upstream) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	upstream := Upstream{Versions: []string{protocol}}
	switch protocol {
	case "2":
		server.EnableHTTP2 = true
		server.StartTLS()
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
			t.Fatal(err)
		}
		upstream.TLS = &UpstreamTLS{ServerName: "example.com", CAFile: ca}
	case "h2c":
		server.Config.Protocols = new(http.Protocols)
		server.Config.Protocols.SetUnencryptedHTTP2(true)
		server.Start()
	default:
		server.Start()
	}
	t.Cleanup(server.Close)
	return server, upstream
}

func adapterTransport(t *testing.T, target string, upstream Upstream) (*HTTPTransport, *adapterRuntime) {
	t.Helper()
	r := &adapterRuntime{target: target, enabled: true, invalid: make(chan struct{})}
	h := &HTTPTransport{Binding: "web", Upstream: upstream, runtime: r}
	if err := h.configure(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Cleanup() })
	return h, r
}

func adapterRequest(t *testing.T, ctx context.Context, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://web.revlb.invalid:80"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "public.example"
	return req
}

func TestAdapterHTTPProtocolsHostAndPooling(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, protocol := range []string{"1.1", "2", "h2c"} {
		t.Run(protocol, func(t *testing.T) {
			server, upstream := adapterHTTPServer(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				fmt.Fprintf(w, "%s %s", req.Host, req.Proto)
			}))
			h, runtime := adapterTransport(t, server.Listener.Addr().String(), upstream)
			if h.transport.Proxy != nil || h.transport.DialTLSContext != nil {
				t.Fatal("transport has a proxy or alternate dial path")
			}
			want := "public.example HTTP/2.0"
			if protocol == "1.1" {
				want = "public.example HTTP/1.1"
			}
			for _, requestURL := range []struct{ host, scheme string }{
				{"web.revlb.invalid:80", ""},
				{"web.revlb.invalid", ""},
				{"web.revlb.invalid:80", "http"},
				{"web.revlb.invalid", "http"},
			} {
				ctx := context.WithValue(context.Background(), caddyhttp.VarsCtxKey, map[string]any{
					"reverse_proxy.dial_info": reverseproxy.DialInfo{
						Network: "tcp", Address: "web.revlb.invalid:80", Host: "web.revlb.invalid", Port: "80",
					},
				})
				req := adapterRequest(t, ctx, "/")
				req.URL.Host = requestURL.host
				req.URL.Scheme = requestURL.scheme
				resp, err := h.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || string(body) != want {
					t.Fatalf("body=%q err=%v; want %q", body, err, want)
				}
				if req.URL.Scheme != requestURL.scheme || req.URL.Host != requestURL.host || req.Host != "public.example" {
					t.Fatal("transport mutated caller request")
				}
			}
			runtime.mu.Lock()
			dials := runtime.dials
			runtime.mu.Unlock()
			if dials != 1 {
				t.Fatalf("keepalive opened %d connections", dials)
			}
		})
	}
}

func TestAdapterHTTPRejectsOtherTargetsAndCONNECT(t *testing.T) {
	h, runtime := adapterTransport(t, "127.0.0.1:1", Upstream{})
	for _, url := range []string{
		"http://other.revlb.invalid:80/", "http://127.0.0.1:80/",
		"http://web.revlb.invalid:81/", "http://user@web.revlb.invalid:80/",
		"https://web.revlb.invalid:80/", "ftp://web.revlb.invalid:80/",
	} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if _, err := h.RoundTrip(req); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
	req := adapterRequest(t, context.Background(), "/")
	req.Method = http.MethodConnect
	if _, err := h.RoundTrip(req); err == nil {
		t.Error("accepted CONNECT")
	}
	if _, err := h.dial(context.Background(), "tcp", "other.revlb.invalid:80"); err == nil {
		t.Error("dial accepted another binding")
	}
	if runtime.dials != 0 {
		t.Fatal("invalid request reached runtime dialer")
	}
}

func TestAdapterHTTPRejectsMismatchedDialInfo(t *testing.T) {
	h, runtime := adapterTransport(t, "127.0.0.1:1", Upstream{})
	for _, info := range []reverseproxy.DialInfo{
		{Network: "tcp", Address: "other.revlb.invalid:80"},
		{Network: "tcp", Address: "127.0.0.1:80"},
		{Network: "tcp", Address: "web.revlb.invalid:81"},
		{Network: "unix", Address: "web.revlb.invalid:80"},
		{Network: "tcp"},
	} {
		ctx := context.WithValue(context.Background(), caddyhttp.VarsCtxKey, map[string]any{
			"reverse_proxy.dial_info": info,
		})
		req := adapterRequest(t, ctx, "/")
		req.URL.Host = "web.revlb.invalid"
		if _, err := h.RoundTrip(req); err == nil {
			t.Errorf("accepted mismatched DialInfo %+v", info)
		}
	}
	if runtime.dials != 0 {
		t.Fatal("mismatched dial metadata reached the runtime")
	}
}

func TestAdapterHTTPUnavailableStatus(t *testing.T) {
	h, runtime := adapterTransport(t, "", Upstream{})
	for _, dialFailure := range []bool{false, true} {
		runtime.enabled = dialFailure
		runtime.err = tunnelcore.ErrUnavailable
		resp, err := h.RoundTrip(adapterRequest(t, context.Background(), "/"))
		var status caddyhttp.HandlerError
		if resp != nil || !errors.As(err, &status) || status.StatusCode != 503 {
			t.Fatalf("dial failure=%v: response=%v error=%v", dialFailure, resp, err)
		}
		// reverse_proxy's statusError applies this same wrapper.
		if got := caddyhttp.Error(http.StatusBadGateway, err); got.StatusCode != 503 {
			t.Fatalf("Caddy replaced unavailable status with %d", got.StatusCode)
		}
	}
}

func TestAdapterHTTPTimeoutAndCancellation(t *testing.T) {
	for _, protocol := range []string{"1.1", "2", "h2c"} {
		t.Run(protocol, func(t *testing.T) {
			server, upstream := adapterHTTPServer(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				<-req.Context().Done()
			}))
			upstream.ResponseHeaderTimeout = caddy.Duration(40 * time.Millisecond)
			h, _ := adapterTransport(t, server.Listener.Addr().String(), upstream)
			start := time.Now()
			_, err := h.RoundTrip(adapterRequest(t, context.Background(), "/"))
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || time.Since(start) > 2*time.Second {
				t.Fatalf("header timeout did not fire: %v", err)
			}
			if status := caddyhttp.Error(http.StatusBadGateway, err); status.StatusCode != http.StatusGatewayTimeout {
				t.Fatalf("timeout status=%d", status.StatusCode)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = h.RoundTrip(adapterRequest(t, ctx, "/"))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error=%v", err)
			}
		})
	}
}

func TestAdapterHTTPStreamingAndRevocation(t *testing.T) {
	for _, protocol := range []string{"1.1", "2", "h2c"} {
		t.Run(protocol, func(t *testing.T) {
			var firstRequests atomic.Int32
			first, upstream := adapterHTTPServer(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				firstRequests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			}))
			second, _ := adapterHTTPServer(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				io.WriteString(w, "second")
			}))
			h, runtime := adapterTransport(t, first.Listener.Addr().String(), upstream)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := h.RoundTrip(adapterRequest(t, ctx, "/"))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			initial := make([]byte, len("data: first\n\n"))
			if _, err := io.ReadFull(resp.Body, initial); err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			old := runtime.invalid
			runtime.target = second.Listener.Addr().String()
			runtime.invalid = make(chan struct{})
			close(old)
			runtime.mu.Unlock()
			_, err = io.ReadAll(resp.Body)
			if err == nil {
				t.Fatal("hard revocation did not terminate active response")
			}
			next, err := h.RoundTrip(adapterRequest(t, ctx, "/after-revocation"))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(next.Body)
			next.Body.Close()
			if err != nil || string(body) != "second" {
				t.Fatalf("replacement response=%q err=%v", body, err)
			}
			if firstRequests.Load() != 1 {
				t.Fatal("revoked session received a new request")
			}
		})
	}
}

func TestAdapterHTTPWebSocketUpgrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			rw.Write(buf)
			rw.Flush()
		}
	}))
	defer server.Close()
	h, _ := adapterTransport(t, server.Listener.Addr().String(), Upstream{Versions: []string{"1.1"}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := adapterRequest(t, ctx, "/")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := h.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	stream, ok := resp.Body.(io.ReadWriteCloser)
	if !ok || resp.StatusCode != 101 {
		t.Fatal("upgrade is not bidirectional")
	}
	h.Cleanup()
	if _, err := h.RoundTrip(adapterRequest(t, ctx, "/new")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("new request admitted during upgrade drain: %v", err)
	}
	if _, err := stream.Write([]byte{0x82, 0x02, 'o', 'k'}); err != nil {
		t.Fatalf("cleanup closed upgraded connection: %v", err)
	}
	frame := make([]byte, 4)
	if _, err := io.ReadFull(stream, frame); err != nil || string(frame[2:]) != "ok" {
		t.Fatalf("upgrade echo=%v err=%v", frame, err)
	}
}

func TestAdapterHTTPVerifiedTLS(t *testing.T) {
	server, upstream := adapterHTTPServer(t, "2", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, mode := range []string{"trusted", "untrusted", "wrong-name"} {
		t.Run(mode, func(t *testing.T) {
			config := upstream
			copyTLS := *config.TLS
			config.TLS = &copyTLS
			switch mode {
			case "untrusted":
				config.TLS.CAFile = ""
			case "wrong-name":
				config.TLS.ServerName = "not-backend.example"
			}
			h, _ := adapterTransport(t, server.Listener.Addr().String(), config)
			resp, err := h.RoundTrip(adapterRequest(t, context.Background(), "/"))
			if resp != nil {
				resp.Body.Close()
			}
			if (err == nil) != (mode == "trusted") {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if h.transport.TLSClientConfig.InsecureSkipVerify || h.transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
				t.Fatal("insecure TLS configuration")
			}
		})
	}
}

func TestAdapterHTTPCaddyfileAndJSON(t *testing.T) {
	var h HTTPTransport
	err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`goreverselb {
		binding web
		versions 1.1 2
		tls_server_name backend.example
		tls
		tls_ca_file /etc/backend.pem
		response_header_timeout 3s
	}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&h)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"binding":"web"`, `"versions":["1.1","2"]`, `"server_name":"backend.example"`, `"response_header_timeout":3000000000`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("missing %s in %s", field, data)
		}
	}
	for _, input := range []string{
		"goreverselb", "goreverselb extra",
		"goreverselb {\n binding web\n versions 3\n}",
		"goreverselb {\n binding web\n tls\n}",
		"goreverselb {\n binding web\n versions h2c 1.1\n}",
		"goreverselb {\n binding web\n response_header_timeout -1s\n}",
		"goreverselb {\n binding web\n proxy http://proxy\n}",
	} {
		var invalid HTTPTransport
		if err := invalid.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err == nil {
			t.Errorf("accepted invalid config %q", input)
		}
	}
}

func TestAdapterHTTPInvalidatedWriteAndCleanup(t *testing.T) {
	h, _ := adapterTransport(t, "", Upstream{})
	left, right := net.Pipe()
	defer right.Close()
	invalid := make(chan struct{})
	conn, err := h.track(&adapterSessionConn{Conn: left, invalid: invalid})
	if err != nil {
		t.Fatal(err)
	}
	close(invalid)
	if _, err := conn.Write([]byte("must not be sent")); !errors.Is(err, tunnelcore.ErrUnavailable) {
		t.Fatalf("write after invalidation: %v", err)
	}
	h.Cleanup()
	if _, err := h.RoundTrip(adapterRequest(t, context.Background(), "/")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("request after cleanup: %v", err)
	}
}

func TestAdapterHTTPTrackRejectsAlreadyInvalidated(t *testing.T) {
	h, _ := adapterTransport(t, "", Upstream{})
	left, right := net.Pipe()
	defer right.Close()
	invalid := make(chan struct{})
	close(invalid)
	conn, err := h.track(&adapterSessionConn{Conn: left, invalid: invalid})
	if conn != nil || !errors.Is(err, tunnelcore.ErrUnavailable) {
		t.Fatalf("tracked invalidated connection: conn=%v err=%v", conn, err)
	}
	h.mu.Lock()
	count := len(h.conns)
	h.mu.Unlock()
	if count != 0 {
		t.Fatal("invalidated connection remains in transport pool")
	}
}

func TestAdapterHTTPGracefulCleanup(t *testing.T) {
	for _, version := range []string{"1.1", "2", "h2c"} {
		t.Run(version, func(t *testing.T) {
			release := make(chan struct{})
			server, upstream := adapterHTTPServer(t, version, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					io.WriteString(w, "data: last\n\n")
				case <-req.Context().Done():
				}
			}))
			h, _ := adapterTransport(t, server.Listener.Addr().String(), upstream)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			resp, err := h.RoundTrip(adapterRequest(t, ctx, "/"))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			first := make([]byte, len("data: first\n\n"))
			if _, err := io.ReadFull(resp.Body, first); err != nil {
				t.Fatal(err)
			}
			h.Cleanup()
			if _, err := h.RoundTrip(adapterRequest(t, ctx, "/new")); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("new request admitted during drain: %v", err)
			}
			close(release)
			last, err := io.ReadAll(resp.Body)
			if err != nil || string(last) != "data: last\n\n" {
				t.Fatalf("cleanup truncated admitted response: %q %v", last, err)
			}
		})
	}
}

type adapterRegistryRuntime struct {
	registry *tunnelcore.Registry
	selector tunnelcore.Selector
}

func (r *adapterRegistryRuntime) available(binding string) bool {
	return binding == "web" && r.registry.Available(r.selector)
}

func (r *adapterRegistryRuntime) dial(ctx context.Context, binding string, info tunnelcore.ConnectionInfo) (net.Conn, error) {
	if binding != "web" {
		return nil, tunnelcore.ErrUnavailable
	}
	return r.registry.DialContext(ctx, r.selector, info)
}

func adapterRegisterYamux(t *testing.T, r *adapterRegistryRuntime, id, target string) <-chan protocol.TunnelConnecData {
	t.Helper()
	left, right := net.Pipe()
	config := yamux.DefaultConfig()
	config.EnableKeepAlive = false
	config.LogOutput = io.Discard
	server, err := yamux.Server(left, config)
	if err != nil {
		t.Fatal(err)
	}
	client, err := yamux.Client(right, config)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	if err := r.registry.Register(id, r.selector, server); err != nil {
		server.Close()
		client.Close()
		t.Fatal(err)
	}
	metadata := make(chan protocol.TunnelConnecData, 16)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			stream, err := client.AcceptStream()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer stream.Close()
				frame, err := protocol.ReadFrame(stream)
				if err != nil {
					return
				}
				var info protocol.TunnelConnecData
				if err := json.Unmarshal(frame, &info); err != nil {
					t.Errorf("stream metadata: %v", err)
					return
				}
				select {
				case metadata <- info:
				case <-ctx.Done():
					return
				}
				backend, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
				if err != nil {
					return
				}
				_ = tunnelcore.Proxy(ctx, stream, backend)
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		server.Close()
		client.Close()
		wg.Wait()
	})
	return metadata
}

func TestAdapterHTTPYamuxRegistryRevocation(t *testing.T) {
	for _, version := range []string{"2", "h2c"} {
		t.Run(version, func(t *testing.T) {
			first, upstream := adapterHTTPServer(t, version, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				io.WriteString(w, "first")
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			}))
			second, _ := adapterHTTPServer(t, version, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, "second")
			}))
			r := &adapterRegistryRuntime{
				registry: tunnelcore.NewRegistry(tunnelcore.Options{}),
				selector: tunnelcore.Selector{UserID: "test", Service: "web"},
			}
			t.Cleanup(func() { r.registry.Close() })
			metadata := adapterRegisterYamux(t, r, "first", first.Listener.Addr().String())
			h := &HTTPTransport{Binding: "web", Upstream: upstream, runtime: r}
			if err := h.configure(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { h.Cleanup() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := adapterRequest(t, ctx, "/")
			req.RemoteAddr = "192.0.2.7:4567"
			resp, err := h.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			firstChunk := make([]byte, 5)
			if _, err := io.ReadFull(resp.Body, firstChunk); err != nil {
				t.Fatal(err)
			}
			select {
			case info := <-metadata:
				if info.SourceAddress != req.RemoteAddr {
					t.Fatalf("source metadata=%q", info.SourceAddress)
				}
			case <-ctx.Done():
				t.Fatal("missing stream metadata")
			}
			adapterRegisterYamux(t, r, "second", second.Listener.Addr().String())
			r.registry.Remove("first")
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Fatal("registry removal did not revoke active HTTP/2 stream")
			}
			next, err := h.RoundTrip(adapterRequest(t, ctx, "/next"))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(next.Body)
			next.Body.Close()
			if err != nil || string(body) != "second" {
				t.Fatalf("post-revocation response=%q error=%v", body, err)
			}
		})
	}
}
