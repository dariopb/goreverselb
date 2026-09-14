package reverselb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/mholt/caddy-l4/layer4"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(L4Handler{})
	caddy.RegisterModule(L4RouteHandler{})
}

type L4Handler struct {
	Binding string `json:"binding"`
	runtime bindingDialer
}

func (L4Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "layer4.handlers.goreverselb", New: func() caddy.Module { return new(L4Handler) }}
}

func (h *L4Handler) Provision(ctx caddy.Context) error {
	var err error
	h.runtime, err = bindingRuntime(ctx, h.Binding)
	return err
}

func (h *L4Handler) Handle(cx *layer4.Connection, _ layer4.Handler) error {
	defer cx.Close()
	if h.runtime == nil {
		return errors.New("goreverselb handler is not provisioned")
	}
	back, err := h.runtime.dial(cx.Context, h.Binding, tunnelcore.ConnectionInfo{SourceAddress: cx.RemoteAddr().String()})
	if err != nil {
		return fmt.Errorf("binding %s: %w", h.Binding, err)
	}
	return proxyL4Connection(cx, h.Binding, back)
}

// Reads must pass through Connection for matcher replay; half-closes must
// reach the delegated socket rather than being hidden by its net.Conn field.
type l4FrontConn struct{ *layer4.Connection }

func (c *l4FrontConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

func (c *l4FrontConn) CloseRead() error {
	if conn, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return conn.CloseRead()
	}
	return nil
}

func (h *L4Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		args := d.RemainingArgs()
		if len(args) > 1 {
			return d.ArgErr()
		}
		if len(args) == 1 {
			h.Binding = args[0]
		}
		for d.NextBlock(0) {
			if d.Val() != "binding" {
				return d.Errf("unrecognized handler option %q", d.Val())
			}
			if !d.AllArgs(&h.Binding) {
				return d.ArgErr()
			}
		}
	}
	if !label.MatchString(h.Binding) {
		return d.Err("binding must be a DNS label")
	}
	return nil
}

type L4RouteHandler struct {
	Bindings map[string]string `json:"bindings"`
	runtime  bindingDialer
}

func (L4RouteHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "layer4.handlers.goreverselb_route", New: func() caddy.Module { return new(L4RouteHandler) }}
}

func (h *L4RouteHandler) Provision(ctx caddy.Context) error {
	if len(h.Bindings) == 0 {
		return errors.New("legacy routing requires explicit instance bindings")
	}
	bindings := make([]string, 0, len(h.Bindings))
	for instance, binding := range h.Bindings {
		if len(instance) > 255 || strings.ContainsAny(instance, "\x00\r\n \t") {
			return fmt.Errorf("invalid legacy instance %q", instance)
		}
		bindings = append(bindings, binding)
	}
	var err error
	h.runtime, err = bindingRuntime(ctx, bindings...)
	return err
}

const legacyHeaderLimit = 8192
const legacyHeaderTimeout = 10 * time.Second

func (h *L4RouteHandler) Handle(cx *layer4.Connection, _ layer4.Handler) error {
	defer cx.Close()
	if h.runtime == nil {
		return errors.New("goreverselb route handler is not provisioned")
	}
	if err := cx.SetReadDeadline(time.Now().Add(legacyHeaderTimeout)); err != nil {
		return err
	}
	stop := context.AfterFunc(cx.Context, func() { _ = cx.Conn.SetDeadline(time.Now()) })
	defer stop()
	instance, connect, err := readLegacyRoute(cx)
	if err != nil {
		if cause := cx.Context.Err(); cause != nil {
			err = cause
		}
		return fmt.Errorf("legacy route: %w", err)
	}
	if err := cx.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	binding, ok := h.Bindings[instance]
	if !ok {
		return fmt.Errorf("legacy instance %q has no binding", instance)
	}
	back, err := h.runtime.dial(cx.Context, binding, tunnelcore.ConnectionInfo{SourceAddress: cx.RemoteAddr().String()})
	if err != nil {
		return fmt.Errorf("binding %s: %w", binding, err)
	}
	defer back.Close()
	if connect {
		if err := cx.SetWriteDeadline(time.Now().Add(legacyHeaderTimeout)); err != nil {
			return err
		}
		if _, err := io.WriteString(cx, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return err
		}
		if err := cx.SetWriteDeadline(time.Time{}); err != nil {
			return err
		}
	}
	return proxyL4Connection(cx, binding, back)
}

func proxyL4Connection(cx *layer4.Connection, binding string, back net.Conn) error {
	logger := cx.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.With(
		zap.String("binding", binding),
		zap.String("source_address", cx.RemoteAddr().String()),
		zap.String("frontend_address", cx.LocalAddr().String()),
		zap.String("tunnel_local", back.LocalAddr().String()),
		zap.String("tunnel_remote", back.RemoteAddr().String()),
	)
	if stream, ok := back.(interface{ StreamID() uint32 }); ok {
		logger = logger.With(zap.Uint32("stream_id", stream.StreamID()))
	}
	logger.Debug("Forwarding L4 connection through tunnel")
	started := time.Now()
	return tunnelcore.ProxyWithObserver(cx.Context, &l4FrontConn{Connection: cx}, back, func(result tunnelcore.CopyResult) {
		direction := "frontend_to_tunnel"
		if result.Direction == tunnelcore.BToA {
			direction = "tunnel_to_frontend"
		}
		logger.Debug("L4 copy finished", zap.String("direction", direction),
			zap.Int64("bytes", result.Bytes), zap.Duration("duration", time.Since(started)), zap.Error(result.Err))
	})
}

func readLegacyRoute(r io.Reader) (instance string, connect bool, err error) {
	var prefix [8]byte
	if _, err = io.ReadFull(r, prefix[:]); err != nil {
		return "", false, err
	}
	if string(prefix[:7]) == "PROXY->" {
		name := make([]byte, int(prefix[7]))
		if _, err = io.ReadFull(r, name); err != nil {
			return "", false, err
		}
		if strings.ContainsAny(string(name), "\x00\r\n \t") {
			return "", false, errors.New("invalid instance name")
		}
		return string(name), false, nil
	}
	if string(prefix[:]) != "CONNECT " {
		return "", false, errors.New("expected PROXY-> or CONNECT preamble")
	}
	header := append([]byte(nil), prefix[:]...)
	var b [1]byte
	for !bytes.HasSuffix(header, []byte("\r\n\r\n")) {
		if len(header) == legacyHeaderLimit {
			return "", false, errors.New("CONNECT header exceeds limit")
		}
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return "", false, err
		}
		header = append(header, b[0])
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header)))
	if err != nil {
		return "", false, fmt.Errorf("invalid CONNECT: %w", err)
	}
	defer req.Body.Close()
	if req.Method != http.MethodConnect || req.URL == nil || req.URL.User != nil ||
		req.ContentLength > 0 || len(req.TransferEncoding) > 0 ||
		(req.Proto != "HTTP/1.0" && req.Proto != "HTTP/1.1") {
		return "", false, errors.New("invalid CONNECT request")
	}
	instance = req.RequestURI
	if strings.ContainsAny(instance, "/\\?#@\x00 \t") || instance == "" || len(instance) > 255 {
		return "", false, errors.New("invalid CONNECT instance")
	}
	// A CONNECT port is syntax only; it is never a dial target.
	if strings.Contains(instance, ":") {
		host, port, splitErr := net.SplitHostPort(instance)
		if splitErr != nil || host == "" || port == "" {
			return "", false, errors.New("invalid CONNECT authority")
		}
		instance = host
	}
	return instance, true, nil
}

func (h *L4RouteHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	h.Bindings = make(map[string]string)
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			var instance, binding string
			if d.Val() != "binding" || !d.AllArgs(&instance, &binding) {
				return d.Err("expected binding <instance> <binding>")
			}
			if !label.MatchString(binding) {
				return d.Err("binding must be a DNS label")
			}
			if _, ok := h.Bindings[instance]; ok {
				return d.Errf("duplicate instance %q", instance)
			}
			h.Bindings[instance] = binding
		}
	}
	if len(h.Bindings) == 0 {
		return d.Err("legacy routing requires explicit instance bindings")
	}
	return nil
}

var (
	_ caddy.Module          = (*L4Handler)(nil)
	_ caddy.Provisioner     = (*L4Handler)(nil)
	_ caddyfile.Unmarshaler = (*L4Handler)(nil)
	_ layer4.NextHandler    = (*L4Handler)(nil)
	_ caddy.Module          = (*L4RouteHandler)(nil)
	_ caddy.Provisioner     = (*L4RouteHandler)(nil)
	_ caddyfile.Unmarshaler = (*L4RouteHandler)(nil)
	_ layer4.NextHandler    = (*L4RouteHandler)(nil)
)
