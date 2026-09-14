package reverselb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
)

func init() { caddy.RegisterModule(new(HTTPTransport)) }

type bindingDialer interface {
	dial(context.Context, string, tunnelcore.ConnectionInfo) (net.Conn, error)
	available(string) bool
}

type upstreamSourceKey struct{}

type HTTPTransport struct {
	Binding string `json:"binding"`
	Upstream

	runtime   bindingDialer
	transport *http.Transport
	mu        sync.Mutex
	conns     map[*httpTunnelConn]struct{}
	closed    bool
	retired   bool
	active    sync.WaitGroup
}

func (*HTTPTransport) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.transport.goreverselb",
		New: func() caddy.Module { return new(HTTPTransport) },
	}
}

func bindingRuntime(ctx caddy.Context, bindings ...string) (*runtime, error) {
	mod, err := ctx.App("goreverselb")
	if err != nil {
		return nil, err
	}
	a, ok := mod.(*App)
	if !ok {
		return nil, errors.New("goreverselb app has unexpected type")
	}
	for _, binding := range bindings {
		if !label.MatchString(binding) {
			return nil, fmt.Errorf("invalid binding %q", binding)
		}
		if _, ok := a.selector(binding); !ok {
			return nil, fmt.Errorf("unknown binding %q", binding)
		}
	}
	if a.runtime == nil {
		return nil, errors.New("goreverselb runtime is not provisioned")
	}
	return a.runtime, nil
}

func (h *HTTPTransport) Provision(ctx caddy.Context) error {
	r, err := bindingRuntime(ctx, h.Binding)
	if err != nil {
		return err
	}
	h.runtime = r
	return h.configure()
}

func (h *HTTPTransport) configure() error {
	if !label.MatchString(h.Binding) {
		return fmt.Errorf("invalid binding %q", h.Binding)
	}
	if err := h.Upstream.validate(); err != nil {
		return err
	}
	versions := h.Versions
	if len(versions) == 0 {
		versions = []string{"1.1", "2"}
	}
	protocols := new(http.Protocols)
	for _, version := range versions {
		switch version {
		case "1.1":
			protocols.SetHTTP1(true)
		case "2":
			protocols.SetHTTP2(true)
		case "h2c":
			protocols.SetUnencryptedHTTP2(true)
		}
	}
	if !protocols.HTTP1() && protocols.HTTP2() && h.TLS == nil {
		return errors.New("HTTP/2 requires upstream TLS; use h2c for cleartext")
	}
	var tlsConfig *tls.Config
	if h.TLS != nil {
		tlsConfig = &tls.Config{ServerName: h.TLS.ServerName, MinVersion: tls.VersionTLS12}
		if h.TLS.CAFile != "" {
			pem, err := os.ReadFile(h.TLS.CAFile)
			if err != nil {
				return fmt.Errorf("read upstream CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return errors.New("upstream CA file contains no certificates")
			}
			tlsConfig.RootCAs = pool
		}
	}
	h.conns = make(map[*httpTunnelConn]struct{})
	h.transport = &http.Transport{
		// TLS and HTTP/2 use this dialer too; no proxy or alternate TLS dialer.
		DialContext:           h.dial,
		TLSClientConfig:       tlsConfig,
		Protocols:             protocols,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Duration(h.ResponseHeaderTimeout),
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       128,
	}
	return nil
}

func (h *HTTPTransport) address() string { return h.Binding + ".revlb.invalid:80" }

func (h *HTTPTransport) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" || address != h.address() {
		return nil, fmt.Errorf("refusing non-binding upstream %q", address)
	}
	source, _ := ctx.Value(upstreamSourceKey{}).(string)
	conn, err := h.runtime.dial(ctx, h.Binding, tunnelcore.ConnectionInfo{SourceAddress: source})
	if err != nil {
		return nil, err
	}
	return h.track(conn)
}

func (h *HTTPTransport) track(conn net.Conn) (net.Conn, error) {
	c := &httpTunnelConn{Conn: conn, owner: h, closed: make(chan struct{})}
	if source, ok := conn.(interface{ Done() <-chan struct{} }); ok {
		c.invalid = source.Done()
	}
	h.mu.Lock()
	if h.retired {
		h.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	if c.invalid != nil {
		select {
		case <-c.invalid:
			_ = c.Close()
			return nil, tunnelcore.ErrUnavailable
		default:
		}
		go func() {
			select {
			case <-c.invalid:
				_ = c.Close()
			case <-c.closed:
			}
		}()
	}
	return c, nil
}

func (h *HTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	fail := func(err error) (*http.Response, error) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if req.Method == http.MethodConnect {
		return fail(errors.New("CONNECT forwarding is not supported"))
	}
	if req.URL == nil || req.URL.User != nil ||
		(req.URL.Scheme != "" && req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return fail(errors.New("request upstream does not match tunnel binding"))
	}
	address := req.URL.Host
	if req.URL.Scheme != "https" && address == h.Binding+".revlb.invalid" {
		address = h.address()
	}
	if address != h.address() {
		return fail(errors.New("request upstream does not match tunnel binding"))
	}
	if info, ok := reverseproxy.GetDialInfo(req.Context()); ok &&
		(info.Network != "tcp" || info.Address != h.address()) {
		return fail(errors.New("selected upstream does not match tunnel binding"))
	}
	if req.URL.Scheme == "https" && h.TLS == nil {
		return fail(errors.New("HTTPS upstream requires verified TLS configuration"))
	}
	if h.transport == nil || h.runtime == nil {
		return fail(errors.New("goreverselb transport is not provisioned"))
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return fail(net.ErrClosed)
	}
	h.active.Add(1)
	h.mu.Unlock()
	if !h.runtime.available(h.Binding) {
		h.active.Done()
		return fail(caddyhttp.Error(http.StatusServiceUnavailable, tunnelcore.ErrUnavailable))
	}
	// Caddy leaves the scheme unset and may strip default URL ports.
	// Normalize the pool identity; backend TLS still dials the synthetic :80.
	out := req.Clone(context.WithValue(req.Context(), upstreamSourceKey{}, req.RemoteAddr))
	out.URL.Host = h.address()
	if h.TLS != nil {
		out.URL.Scheme = "https"
	} else {
		out.URL.Scheme = "http"
	}
	resp, err := h.transport.RoundTrip(out)
	if err != nil {
		h.active.Done()
		if errors.Is(err, tunnelcore.ErrUnavailable) {
			return nil, caddyhttp.Error(http.StatusServiceUnavailable, err)
		}
		var timeout net.Error
		if !errors.Is(err, context.Canceled) && errors.As(err, &timeout) && timeout.Timeout() {
			return nil, caddyhttp.Error(http.StatusGatewayTimeout, err)
		}
		return nil, err
	}
	body := &httpResponseBody{ReadCloser: resp.Body, done: h.active.Done}
	if duplex, ok := resp.Body.(io.ReadWriteCloser); ok {
		resp.Body = &httpDuplexBody{httpResponseBody: body, writer: duplex}
	} else {
		resp.Body = body
	}
	return resp, nil
}

func (h *HTTPTransport) Cleanup() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	if h.transport != nil {
		h.transport.CloseIdleConnections()
	}
	// Stop admission immediately, but let already-admitted streams finish.
	// Session invalidation remains a hard close even during graceful cleanup.
	go func() {
		h.active.Wait()
		h.mu.Lock()
		h.retired = true
		conns := make([]*httpTunnelConn, 0, len(h.conns))
		for c := range h.conns {
			conns = append(conns, c)
		}
		h.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	return nil
}

type httpResponseBody struct {
	io.ReadCloser
	done func()
	once sync.Once
}

func (b *httpResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.done)
	}
	return n, err
}

func (b *httpResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

type httpDuplexBody struct {
	*httpResponseBody
	writer io.Writer
}

func (b *httpDuplexBody) Write(p []byte) (int, error) { return b.writer.Write(p) }

// Invalidation closes active multiplexed connections, not just idle pools.
// Checking writes also prevents reuse while the close watcher is unscheduled.
type httpTunnelConn struct {
	net.Conn
	owner   *HTTPTransport
	invalid <-chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *httpTunnelConn) Write(p []byte) (int, error) {
	select {
	case <-c.invalid:
		return 0, tunnelcore.ErrUnavailable
	default:
		return c.Conn.Write(p)
	}
}

func (c *httpTunnelConn) Close() error {
	var err error
	c.once.Do(func() {
		close(c.closed)
		err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		c.owner.mu.Unlock()
	})
	return err
}

func (h *HTTPTransport) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "binding":
				if !d.AllArgs(&h.Binding) {
					return d.ArgErr()
				}
			case "versions":
				h.Versions = d.RemainingArgs()
				if len(h.Versions) == 0 {
					return d.ArgErr()
				}
			case "tls":
				if d.NextArg() {
					return d.ArgErr()
				}
				if h.TLS == nil {
					h.TLS = new(UpstreamTLS)
				}
			case "tls_server_name", "tls_ca_file":
				key := d.Val()
				var value string
				if !d.AllArgs(&value) {
					return d.ArgErr()
				}
				if h.TLS == nil {
					h.TLS = new(UpstreamTLS)
				}
				if key == "tls_server_name" {
					h.TLS.ServerName = value
				} else {
					h.TLS.CAFile = value
				}
			case "response_header_timeout":
				var value string
				if !d.AllArgs(&value) {
					return d.ArgErr()
				}
				duration, err := caddy.ParseDuration(value)
				if err != nil {
					return d.Errf("invalid response_header_timeout: %v", err)
				}
				h.ResponseHeaderTimeout = caddy.Duration(duration)
			default:
				return d.Errf("unrecognized transport option %q", d.Val())
			}
		}
	}
	if !label.MatchString(h.Binding) {
		return d.Err("binding must be a DNS label")
	}
	return h.Upstream.validate()
}

var (
	_ caddy.Module          = (*HTTPTransport)(nil)
	_ caddy.Provisioner     = (*HTTPTransport)(nil)
	_ caddy.CleanerUpper    = (*HTTPTransport)(nil)
	_ caddyfile.Unmarshaler = (*HTTPTransport)(nil)
	_ http.RoundTripper     = (*HTTPTransport)(nil)
)
