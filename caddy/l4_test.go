package reverselb

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/mholt/caddy-l4/layer4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAdapterLegacyParsing(t *testing.T) {
	tests := []struct {
		name, input, instance, remainder string
		connect                          bool
	}{
		{"custom", "PROXY->\x05alphaextra", "alpha", "extra", false},
		{"empty explicit instance", "PROXY->\x00extra", "", "extra", false},
		{"connect", "CONNECT alpha HTTP/1.1\r\nHost: alpha\r\n\r\nextra", "alpha", "extra", true},
		{"authority", "CONNECT alpha:443 HTTP/1.1\r\nHost: ignored\r\n\r\nextra", "alpha", "extra", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := iotest.OneByteReader(strings.NewReader(test.input))
			instance, connect, err := readLegacyRoute(r)
			if err != nil || instance != test.instance || connect != test.connect {
				t.Fatalf("instance=%q connect=%v error=%v", instance, connect, err)
			}
			remaining, err := io.ReadAll(r)
			if err != nil || string(remaining) != test.remainder {
				t.Fatalf("remaining=%q error=%v", remaining, err)
			}
		})
	}
}

func TestAdapterLegacyRejectsMalformedAndOversized(t *testing.T) {
	for _, input := range []string{
		"", "PROXY->", "PROXY->\x05a", "PROXY->\x01\n",
		"GET / HTTP/1.1\r\n\r\n", "CONNECT alpha HTTP/1.1\r\n",
		"CONNECT alpha HTTP/1.1\r\nContent-Length: 4\r\n\r\n",
		"CONNECT alpha HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n",
		"CONNECT http://alpha HTTP/1.1\r\n\r\n",
		"CONNECT alpha HTTP/2.0\r\n\r\n",
		"CONNECT alpha: HTTP/1.1\r\n\r\n",
	} {
		if _, _, err := readLegacyRoute(strings.NewReader(input)); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	prefix, suffix := "CONNECT alpha HTTP/1.1\r\nX-Padding: ", "\r\n\r\n"
	exact := prefix + strings.Repeat("x", legacyHeaderLimit-len(prefix)-len(suffix)) + suffix
	if instance, connect, err := readLegacyRoute(strings.NewReader(exact)); err != nil || !connect || instance != "alpha" {
		t.Fatalf("exact %d-byte header rejected: %v", legacyHeaderLimit, err)
	}
	oversized := prefix + strings.Repeat("x", legacyHeaderLimit-len(prefix)-len(suffix)+1) + suffix
	if _, _, err := readLegacyRoute(strings.NewReader(oversized)); err == nil {
		t.Fatal("oversized header accepted")
	}
}

func adapterTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	front, err := listener.Accept()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); front.Close() })
	client.SetDeadline(time.Now().Add(3 * time.Second))
	front.SetDeadline(time.Now().Add(3 * time.Second))
	return client.(*net.TCPConn), front.(*net.TCPConn)
}

func TestAdapterL4ReplayAndHalfClose(t *testing.T) {
	for _, mode := range []string{"direct", "custom", "connect"} {
		t.Run(mode, func(t *testing.T) {
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			backendDone := make(chan error, 1)
			go func() {
				conn, err := backend.Accept()
				if err != nil {
					backendDone <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				body, err := io.ReadAll(conn)
				if err == nil {
					_, err = conn.Write(append([]byte("reply:"), body...))
				}
				backendDone <- err
			}()
			runtime := &adapterRegistryRuntime{
				registry: tunnelcore.NewRegistry(tunnelcore.Options{}),
				selector: tunnelcore.Selector{UserID: "test", Service: "web"},
			}
			t.Cleanup(func() { runtime.registry.Close() })
			adapterRegisterYamux(t, runtime, "l4", backend.Addr().String())
			client, front := adapterTCPPair(t)
			prefetched := "buffered:"
			var handler layer4.NextHandler = &L4Handler{Binding: "web", runtime: runtime}
			switch mode {
			case "custom":
				prefetched = "PROXY->\x05alphabuffered:"
				handler = &L4RouteHandler{Bindings: map[string]string{"alpha": "web"}, runtime: runtime}
			case "connect":
				prefetched = "CONNECT alpha HTTP/1.1\r\nHost: alpha\r\n\r\nbuffered:"
				handler = &L4RouteHandler{Bindings: map[string]string{"alpha": "web"}, runtime: runtime}
			}
			core, logs := observer.New(zap.DebugLevel)
			cx := layer4.WrapConnection(front, []byte(prefetched), zap.New(core))
			result := make(chan error, 1)
			go func() { result <- handler.Handle(cx, nil) }()
			if _, err := io.WriteString(client, "live"); err != nil {
				t.Fatal(err)
			}
			client.CloseWrite()
			response, err := io.ReadAll(client)
			if err != nil {
				t.Fatal(err)
			}
			want := "reply:buffered:live"
			if mode == "connect" {
				want = "HTTP/1.1 200 Connection Established\r\n\r\n" + want
			}
			if string(response) != want {
				t.Fatalf("response=%q want=%q", response, want)
			}
			if err := <-result; err != nil {
				t.Fatalf("proxy: %v", err)
			}
			if err := <-backendDone; err != nil {
				t.Fatal(err)
			}
			started := logs.FilterMessage("Forwarding L4 connection through tunnel").All()
			if len(started) != 1 {
				t.Fatal("missing L4 forwarding diagnostic")
			}
			fields := started[0].ContextMap()
			if fields["source_address"] != client.LocalAddr().String() ||
				fields["frontend_address"] != front.LocalAddr().String() || fields["binding"] != "web" {
				t.Fatal("L4 diagnostic lost the source, frontend address, or binding")
			}
			for _, key := range []string{"tunnel_local", "tunnel_remote", "stream_id"} {
				if _, ok := fields[key]; !ok {
					t.Errorf("missing L4 hop field %s", key)
				}
			}
			wantBytes := map[string]int64{
				"frontend_to_tunnel": int64(len("buffered:live")),
				"tunnel_to_frontend": int64(len("reply:buffered:live")),
			}
			for _, entry := range logs.FilterMessage("L4 copy finished").All() {
				fields := entry.ContextMap()
				direction, _ := fields["direction"].(string)
				if n, ok := wantBytes[direction]; !ok || fields["bytes"] != n {
					t.Errorf("incorrect L4 copy diagnostics for %s", direction)
				}
				delete(wantBytes, direction)
			}
			if len(wantBytes) != 0 {
				t.Fatal("missing L4 directional byte counts")
			}
		})
	}
}

func TestAdapterL4FailureIsTerminal(t *testing.T) {
	for _, mode := range []string{"direct", "unknown", "connect-dial-failure"} {
		t.Run(mode, func(t *testing.T) {
			client, front := adapterTCPPair(t)
			runtime := &adapterRuntime{err: tunnelcore.ErrUnavailable}
			var handler layer4.NextHandler = &L4Handler{Binding: "web", runtime: runtime}
			var preamble string
			if mode != "direct" {
				handler = &L4RouteHandler{Bindings: map[string]string{"alpha": "web"}, runtime: runtime}
				preamble = "CONNECT alpha HTTP/1.1\r\n\r\n"
				if mode == "unknown" {
					preamble = "PROXY->\x07unknown"
				}
			}
			cx := layer4.WrapConnection(front, []byte(preamble), zap.NewNop())
			err := handler.Handle(cx, adapterUnexpectedNext{t: t})
			if err == nil {
				t.Fatal("missing terminal error")
			}
			response, err := io.ReadAll(client)
			if err != nil || len(response) != 0 {
				t.Fatalf("failure emitted a success response: %q %v", response, err)
			}
			if mode == "unknown" && runtime.dials != 0 {
				t.Fatal("unknown instance fell back to a binding")
			}
		})
	}
}

type adapterUnexpectedNext struct{ t *testing.T }

func (n adapterUnexpectedNext) Handle(*layer4.Connection) error {
	n.t.Error("terminal handler called next")
	return errors.New("unexpected next handler")
}

func TestAdapterLegacyReadDeadline(t *testing.T) {
	client, front := adapterTCPPair(t)
	front.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	go io.WriteString(client, "CONNECT ")
	_, _, err := readLegacyRoute(front)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("incomplete prefix did not time out: %v", err)
	}
}

func TestAdapterL4Cancellation(t *testing.T) {
	client, front := adapterTCPPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime := &adapterRuntime{err: context.Canceled}
	cx := layer4.WrapConnection(front, nil, zap.NewNop())
	cx.Context = ctx
	h := &L4Handler{Binding: "web", runtime: runtime}
	if err := h.Handle(cx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	if body, err := io.ReadAll(client); err != nil || len(body) != 0 {
		t.Fatalf("frontend remained open: %q %v", body, err)
	}
}

func TestAdapterLegacyCancellationDuringPrefix(t *testing.T) {
	client, front := adapterTCPPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cx := layer4.WrapConnection(front, []byte("CONNECT "), zap.NewNop())
	cx.Context = ctx
	h := &L4RouteHandler{Bindings: map[string]string{"alpha": "web"}, runtime: &adapterRuntime{}}
	result := make(chan error, 1)
	go func() { result <- h.Handle(cx, nil) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("legacy cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		client.Close()
		t.Fatal("legacy prefix read ignored cancellation")
	}
}

func TestAdapterL4Caddyfile(t *testing.T) {
	for _, input := range []string{"goreverselb web", "goreverselb {\n binding web\n}"} {
		var h L4Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil || h.Binding != "web" {
			t.Fatalf("parse %q: binding=%q err=%v", input, h.Binding, err)
		}
	}
	var route L4RouteHandler
	err := route.UnmarshalCaddyfile(caddyfile.NewTestDispenser("goreverselb_route {\n binding alpha web\n}"))
	if err != nil || route.Bindings["alpha"] != "web" {
		t.Fatalf("legacy Caddyfile: %v", err)
	}
	for _, input := range []string{
		"goreverselb_route", "goreverselb_route extra",
		"goreverselb_route {\n binding alpha\n}",
		"goreverselb_route {\n binding alpha web\n binding alpha other\n}",
		"goreverselb_route {\n binding alpha not.a.label\n}",
	} {
		var invalid L4RouteHandler
		if err := invalid.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}
