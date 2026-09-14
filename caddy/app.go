package reverselb

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	_ "github.com/mholt/caddy-l4"
	"go.uber.org/zap"
)

func init() { caddy.RegisterModule(&App{}) }

var runtimes = caddy.NewUsagePool()

type App struct {
	RuntimeID   string                         `json:"runtime_id,omitempty"`
	Control     Control                        `json:"control"`
	Users       map[string]User                `json:"users"`
	Publication Publication                    `json:"publication,omitempty"`
	Bindings    map[string]tunnelcore.Selector `json:"bindings,omitempty"`
	Generated   map[string]Endpoint            `json:"generated,omitempty"`

	// Consumed by the extended Caddyfile adapter, never by the running app.
	CertificateFiles caddytls.FileLoader `json:"certificate_files,omitempty"`

	runtime           *runtime
	poolKey           string
	ctx               caddy.Context
	tlsConfig         *tls.Config
	tlsApp            *caddytls.TLS
	release           sync.Once
	unavailablePorts  map[int]bool
	recoverableLeases map[string]Endpoint
}

func (*App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "goreverselb", New: func() caddy.Module { return new(App) }}
}

func (a *App) runtimeKey() string {
	data, _ := json.Marshal(struct {
		ID      string
		Control Control
		Mode    string
		Admin   string
		Host    string
		Start   int
		Count   int
	}{a.RuntimeID, a.Control, a.Publication.Mode, a.Publication.AdminEndpoint,
		a.Publication.BindHost, a.Publication.PortStart, a.Publication.PortCount})
	return stableID(string(data))
}

func (a *App) Provision(ctx caddy.Context) error {
	a.ctx = ctx
	a.defaults()
	if err := a.Validate(); err != nil {
		return err
	}
	mod, err := ctx.App("tls")
	if err != nil {
		return err
	}
	var ok bool
	a.tlsApp, ok = mod.(*caddytls.TLS)
	if !ok {
		return errors.New("TLS app has unexpected type")
	}
	policies := caddytls.ConnectionPolicies{&caddytls.ConnectionPolicy{DefaultSNI: a.Control.TLS.ServerName}}
	if err := policies.Provision(ctx); err != nil {
		return fmt.Errorf("control TLS: %w", err)
	}
	a.tlsConfig = policies.TLSConfig(ctx)
	a.poolKey = a.runtimeKey()
	value, _, err := runtimes.LoadOrNew(a.poolKey, func() (caddy.Destructor, error) {
		return newRuntime(a, ctx.Logger(), ctx.Storage())
	})
	if err != nil {
		return err
	}
	a.runtime = value.(*runtime)
	return nil
}

func (a *App) Start() error {
	if err := a.runtime.start(a); err != nil {
		return err
	}
	// ActiveContext changes only after every app has started successfully.
	// Never recursively load configuration from a Caddy lifecycle hook.
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				mod, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
				if err == nil && mod == a {
					a.runtime.activate(a)
					return
				}
			}
		}
	}()
	return nil
}

func (a *App) Stop() error { return a.Cleanup() }

func (a *App) Cleanup() error {
	var err error
	a.release.Do(func() {
		if a.runtime != nil {
			_, err = runtimes.Delete(a.poolKey)
		}
	})
	return err
}

func (r *runtime) dial(ctx context.Context, binding string, info tunnelcore.ConnectionInfo) (net.Conn, error) {
	logger := r.logger.With(zap.String("runtime_id", r.id), zap.String("binding", binding),
		zap.String("source_address", info.SourceAddress))
	a := r.active.Load()
	if a == nil {
		logger.Debug("Tunnel runtime is not active")
		return nil, tunnelcore.ErrUnavailable
	}
	sel, ok := a.selector(binding)
	if !ok || !r.available(binding) {
		logger.Debug("Tunnel binding is unavailable")
		return nil, tunnelcore.ErrUnavailable
	}
	logger = logger.With(zap.String("user_id", sel.UserID), zap.String("service", sel.Service), zap.String("instance", sel.Instance))
	logger.Debug("Opening tunnel stream")
	conn, err := r.registry.DialContext(ctx, sel, info)
	if err != nil {
		logger.Debug("Tunnel stream open failed", zap.Error(err))
		return nil, err
	}
	fields := []zap.Field{
		zap.String("tunnel_local", conn.LocalAddr().String()), zap.String("tunnel_remote", conn.RemoteAddr().String()),
	}
	if stream, ok := conn.(interface{ StreamID() uint32 }); ok {
		fields = append(fields, zap.Uint32("stream_id", stream.StreamID()))
	}
	logger.Debug("Tunnel stream opened", fields...)
	return conn, nil
}

func (r *runtime) available(binding string) bool {
	a := r.active.Load()
	if a == nil {
		return false
	}
	current, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
	if err != nil || current != a {
		return false
	}
	sel, ok := a.selector(binding)
	return ok && r.registry.Available(sel)
}

var (
	_ caddy.App          = (*App)(nil)
	_ caddy.Provisioner  = (*App)(nil)
	_ caddy.Validator    = (*App)(nil)
	_ caddy.CleanerUpper = (*App)(nil)
)
