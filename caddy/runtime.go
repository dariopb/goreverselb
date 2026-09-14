package reverselb

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/certmagic"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"
)

type registration struct {
	id       string
	selector tunnelcore.Selector
	data     protocol.TunnelData
	session  *yamux.Session
}

type registerJob struct {
	reg    registration
	result chan registerResult
}

type registerResult struct {
	response protocol.TunnelDataResponse
	err      error
}

type runtime struct {
	key           string
	id            string
	ctx           context.Context
	cancel        context.CancelFunc
	logger        *zap.Logger
	storage       certmagic.Storage
	admin         *adminAPI
	registry      *tunnelcore.Registry
	active        atomic.Pointer[App]
	activationMu  sync.Mutex
	startOnce     sync.Once
	startErr      error
	startedAt     time.Time
	mu            sync.Mutex
	listeners     []net.Listener
	conns         map[net.Conn]struct{}
	entries       map[string]registration
	recovered     map[string]time.Time
	jobs          chan registerJob
	wake          chan struct{}
	sequence      atomic.Uint64
	journalLoaded bool
	intents       map[string]Endpoint
}

func newRuntime(a *App, logger *zap.Logger, storage certmagic.Storage) (*runtime, error) {
	admin, err := adminClient(a.Publication.AdminEndpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &runtime{
		key: a.runtimeKey(), id: a.RuntimeID, ctx: ctx, cancel: cancel,
		logger: logger, storage: storage, admin: admin,
		registry: tunnelcore.NewRegistry(tunnelcore.Options{
			MaxStreams: 4096, MaxStreamsPerUser: 1024, OpenTimeout: 10 * time.Second,
		}),
		conns: make(map[net.Conn]struct{}), entries: make(map[string]registration),
		recovered: make(map[string]time.Time),
		jobs:      make(chan registerJob, 256), wake: make(chan struct{}, 1),
	}, nil
}

func (r *runtime) start(a *App) error {
	r.startOnce.Do(func() {
		r.startedAt = time.Now()
		for _, addr := range a.Control.Listen {
			na, err := caddy.ParseNetworkAddress(addr)
			if err != nil {
				r.startErr = err
				break
			}
			raw, err := na.Listen(r.ctx, 0, net.ListenConfig{})
			if err != nil {
				r.startErr = err
				break
			}
			ln, ok := raw.(net.Listener)
			if !ok {
				r.startErr = errors.New("control listener is not stream-oriented")
				break
			}
			r.listeners = append(r.listeners, ln)
		}
		if r.startErr != nil {
			for _, ln := range r.listeners {
				_ = ln.Close()
			}
			return
		}
		for _, ln := range r.listeners {
			go r.accept(ln)
		}
		go r.work()
	})
	return r.startErr
}

func (r *runtime) activate(a *App) {
	r.activationMu.Lock()
	current, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
	if err != nil || current != a || a.ctx.Err() != nil {
		r.activationMu.Unlock()
		return
	}
	previous := r.active.Load()
	if previous == a {
		r.activationMu.Unlock()
		return
	}
	r.mu.Lock()
	if previous == nil {
		for id := range a.Generated {
			r.recovered[id] = time.Now().Add(time.Duration(a.Publication.RestartGrace))
		}
	}
	var revoked []registration
	for _, reg := range r.entries {
		if _, err := a.authorize(reg.data); err != nil {
			revoked = append(revoked, reg)
		}
	}
	r.mu.Unlock()
	for _, reg := range revoked {
		r.registry.Remove(reg.id)
		_ = reg.session.Close()
	}
	r.active.Store(a)
	r.activationMu.Unlock()
	names := map[string]struct{}{a.Control.TLS.ServerName: {}}
	for _, t := range a.Publication.Templates {
		if t.FrontendTLS {
			names[t.TLSServerName] = struct{}{}
		}
	}
	for name := range names {
		if a.tlsApp.HasCertificateForSubject(name) {
			delete(names, name)
		}
	}
	if len(names) != 0 {
		if err := a.tlsApp.Manage(names); err != nil {
			r.logger.Error("certificate management failed", zap.Error(err))
		}
	}
	r.signal()
}

func (r *runtime) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *runtime) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if r.ctx.Err() == nil {
				r.logger.Error("control accept failed", zap.Error(err))
			}
			return
		}
		r.mu.Lock()
		if len(r.conns) >= 1024 {
			r.mu.Unlock()
			r.logger.Warn("control connection limit reached")
			_ = conn.Close()
			continue
		}
		r.conns[conn] = struct{}{}
		r.mu.Unlock()
		go r.handle(conn)
	}
}

func (r *runtime) handle(raw net.Conn) {
	defer func() {
		_ = raw.Close()
		r.mu.Lock()
		delete(r.conns, raw)
		r.mu.Unlock()
	}()
	conn := tls.Server(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			a := r.active.Load()
			if a == nil {
				return nil, errors.New("control plane has not committed")
			}
			return a.tlsConfig.GetConfigForClient(hello)
		},
	})
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	err := conn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		r.logger.Debug("control TLS handshake failed", zap.Error(err))
		return
	}
	config := yamux.DefaultConfig()
	config.Logger = zap.NewStdLog(r.logger)
	config.LogOutput = nil
	config.StreamOpenTimeout = 10 * time.Second
	session, err := yamux.Server(conn, config)
	if err != nil {
		r.logger.Error("create control session", zap.Error(err))
		return
	}
	defer session.Close()
	// AcceptStream has no context argument; closing the session interrupts it.
	timer := time.AfterFunc(10*time.Second, func() { _ = session.Close() })
	control, err := session.AcceptStream()
	timer.Stop()
	if err != nil {
		r.logger.Debug("accept registration stream", zap.Error(err))
		return
	}
	defer control.Close()
	_ = control.SetReadDeadline(time.Now().Add(10 * time.Second))
	var td protocol.TunnelData
	if err := json.NewDecoder(io.LimitReader(control, 64<<10)).Decode(&td); err != nil {
		r.logger.Warn("invalid registration JSON", zap.Error(err))
		return
	}
	_ = control.SetReadDeadline(time.Time{})
	id := fmt.Sprintf("%s-%d", r.key, r.sequence.Add(1))
	job := registerJob{reg: registration{id: id, data: td, session: session}, result: make(chan registerResult, 1)}
	select {
	case r.jobs <- job:
	default:
		r.sendResponse(control, protocol.TunnelDataResponse{Error: "publication queue is full"})
		return
	}
	var result registerResult
	select {
	case result = <-job.result:
	case <-r.ctx.Done():
		return
	case <-session.CloseChan():
		return
	}
	if result.err != nil {
		r.logger.Warn("registration failed", zap.Error(result.err))
		r.sendResponse(control, protocol.TunnelDataResponse{ID: td.ID, ServiceName: td.ServiceName, Error: result.err.Error()})
		return
	}
	defer func() {
		r.registry.Remove(id)
		r.mu.Lock()
		delete(r.entries, id)
		r.mu.Unlock()
		r.signal()
	}()
	if err := r.sendResponse(control, result.response); err != nil {
		return
	}
	// The control stream must remain open; additional messages are not part of v1.
	var b [1]byte
	if _, err := control.Read(b[:]); err != nil && !errors.Is(err, io.EOF) && r.ctx.Err() == nil {
		r.logger.Debug("control stream ended", zap.Error(err))
	}
}

func (r *runtime) sendResponse(conn net.Conn, response protocol.TunnelDataResponse) error {
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := json.NewEncoder(conn).Encode(response)
	_ = conn.SetWriteDeadline(time.Time{})
	if err != nil {
		r.logger.Debug("registration response failed", zap.Error(err))
	}
	return err
}

func (r *runtime) work() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case job := <-r.jobs:
			result := r.register(job.reg)
			if job.reg.session.IsClosed() && result.err == nil {
				r.registry.Remove(job.reg.id)
				r.mu.Lock()
				delete(r.entries, job.reg.id)
				r.mu.Unlock()
				r.signal()
			}
			job.result <- result
		case <-r.wake:
			r.reconcile()
		case <-ticker.C:
			r.reconcile()
		}
	}
}

func (r *runtime) register(reg registration) registerResult {
	a := r.active.Load()
	if a == nil || r.ctx.Err() != nil || reg.session.IsClosed() {
		return registerResult{err: errors.New("control plane is not ready")}
	}
	sel, err := a.authorize(reg.data)
	if err != nil {
		return registerResult{err: err}
	}
	reg.selector = sel
	r.mu.Lock()
	userSessions := 0
	for _, e := range r.entries {
		if e.selector.UserID == sel.UserID {
			userSessions++
		}
	}
	r.mu.Unlock()
	if userSessions >= 64 {
		return registerResult{err: errors.New("user session limit reached")}
	}
	port := 0
	if a.Publication.Mode == "bindings_only" {
		if reg.data.FrontendData.Port != 0 || reg.data.FrontendData.TLSWrap || reg.data.FrontendData.SSHWrap {
			return registerResult{err: errors.New("bindings_only does not allocate ports or apply wrapping")}
		}
	} else {
		_, template := a.templateFor(sel)
		if template.FrontendTLS && !a.tlsApp.HasCertificateForSubject(template.TLSServerName) {
			return registerResult{err: errors.New("frontend TLS certificate is not ready")}
		}
		ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		defer cancel()
		err = r.update(ctx, func(current *App) error {
			currentSel, err := current.authorize(reg.data)
			if err != nil {
				return err
			}
			port, err = current.addEndpoint(currentSel, reg.data)
			return err
		})
		if err != nil {
			return registerResult{err: err}
		}
	}
	// Config activation monitoring is asynchronous; wait without holding Caddy locks.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		current := r.active.Load()
		if current != nil {
			if _, ok := current.selector(bindingID(sel)); ok || current.Publication.Mode == "bindings_only" {
				if _, err := current.authorize(reg.data); err != nil {
					r.signal()
					return registerResult{err: err}
				}
				break
			}
		}
		select {
		case <-deadline.C:
			r.signal()
			return registerResult{err: errors.New("published configuration has not activated")}
		case <-r.ctx.Done():
			return registerResult{err: r.ctx.Err()}
		case <-time.After(10 * time.Millisecond):
		}
	}
	r.activationMu.Lock()
	committed, loadErr := caddy.ActiveContext().AppIfConfigured("goreverselb")
	if loadErr != nil || committed != r.active.Load() {
		r.activationMu.Unlock()
		r.signal()
		return registerResult{err: errors.New("publication policy is changing; retry registration")}
	}
	if _, err := r.active.Load().authorize(reg.data); err != nil {
		r.activationMu.Unlock()
		r.signal()
		return registerResult{err: err}
	}
	if err := r.registry.Register(reg.id, sel, reg.session); err != nil {
		r.activationMu.Unlock()
		r.signal()
		return registerResult{err: err}
	}
	r.mu.Lock()
	r.entries[reg.id] = reg
	r.mu.Unlock()
	r.activationMu.Unlock()
	r.logger.Info("tunnel registered",
		zap.String("user_id", sel.UserID), zap.String("service", sel.Service),
		zap.String("instance", sel.Instance), zap.Int("frontend_port", port))
	return registerResult{response: protocol.TunnelDataResponse{
		ID: reg.data.ID, ServiceName: reg.data.ServiceName, FrontendPort: port,
		FrontendAddress: a.Publication.AdvertiseHost, PublicationMode: a.Publication.Mode,
	}}
}

func (r *runtime) Destruct() error {
	r.cancel()
	// Cleanup can execute under Caddy's config lock. Never join the worker here.
	for _, ln := range r.listeners {
		_ = ln.Close()
	}
	r.mu.Lock()
	for conn := range r.conns {
		_ = conn.Close()
	}
	r.mu.Unlock()
	r.admin.client.CloseIdleConnections()
	return r.registry.Close()
}
