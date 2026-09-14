package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
)

// ClientOptions configures the control connection, independently of frontend TLS.
// A nil TLSConfig uses system roots and verifies the endpoint hostname.
type ClientOptions struct {
	TLSConfig      *tls.Config
	DialTimeout    time.Duration
	ReconnectDelay time.Duration
}

// LegacyClientOptions explicitly preserves the historical self-signed-server mode.
// Prefer NewMuxTunnelClientWithOptions with verified TLS for new deployments.
func LegacyClientOptions() ClientOptions {
	return ClientOptions{TLSConfig: &tls.Config{InsecureSkipVerify: true}}
}

func normalizeClientOptions(options ClientOptions) (ClientOptions, error) {
	if options.DialTimeout < 0 || options.ReconnectDelay < 0 {
		return options, fmt.Errorf("client timeouts must not be negative")
	}
	if options.DialTimeout == 0 {
		options.DialTimeout = 10 * time.Second
	}
	if options.ReconnectDelay == 0 {
		options.ReconnectDelay = 10 * time.Second
	}
	if options.TLSConfig == nil {
		options.TLSConfig = &tls.Config{}
	} else {
		options.TLSConfig = options.TLSConfig.Clone()
		if options.TLSConfig.RootCAs != nil {
			options.TLSConfig.RootCAs = options.TLSConfig.RootCAs.Clone()
		}
	}
	if options.TLSConfig.MinVersion == 0 {
		options.TLSConfig.MinVersion = tls.VersionTLS12
	}
	return options, nil
}

type MuxTunnelClient struct {
	apiEndpoint     string
	apiEndpointHost string
	frontendAddress string
	tunnelData      TunnelData
	tlsconfig       *tls.Config
	dialerTimeout   time.Duration
	reconnectDelay  time.Duration
	logger          *log.Entry
	connections     []ClientConnectionStatus
	statusChanged   chan struct{}
	statusClosed    bool
	mtx             sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	closeOnce       sync.Once
	wg              sync.WaitGroup
}

// NewMuxTunnelClient preserves legacy certificate-verification behavior.
func NewMuxTunnelClient(apiEndpoint string, td TunnelData) (*MuxTunnelClient, error) {
	return NewMuxTunnelClientWithOptions(apiEndpoint, td, LegacyClientOptions())
}

// NewMuxTunnelClientWithOptions starts a reconnecting client. Its zero options
// verify TLS using system roots and the API endpoint hostname.
func NewMuxTunnelClientWithOptions(apiEndpoint string, td TunnelData, options ClientOptions) (*MuxTunnelClient, error) {
	host, _, err := net.SplitHostPort(apiEndpoint)
	if err != nil {
		return nil, err
	}
	if td.BackendAcceptBacklog < 0 {
		return nil, fmt.Errorf("backend accept backlog must not be negative")
	}
	options, err = normalizeClientOptions(options)
	if err != nil {
		return nil, err
	}
	if options.TLSConfig.ServerName == "" {
		options.TLSConfig.ServerName = host
	}
	ctx, cancel := context.WithCancel(context.Background())
	td.TargetAddresses = append([]string(nil), td.TargetAddresses...)
	tc := &MuxTunnelClient{
		apiEndpoint: apiEndpoint, apiEndpointHost: host, tunnelData: td,
		tlsconfig: options.TLSConfig, dialerTimeout: options.DialTimeout,
		reconnectDelay: options.ReconnectDelay, ctx: ctx, cancel: cancel,
		logger: log.WithFields(log.Fields{"endpoint": apiEndpoint, "service": td.ServiceName}),
	}
	log.Infof("New tunnel client: endpoint [%s], service [%s]", apiEndpoint, td.ServiceName)
	count := td.BackendAcceptBacklog
	if count == 0 {
		count = 1
	}
	tc.connections = make([]ClientConnectionStatus, count)
	tc.statusChanged = make(chan struct{})
	for i := range tc.connections {
		tc.connections[i] = ClientConnectionStatus{ID: i, State: ClientStateConnecting, UpdatedAt: time.Now().UTC()}
	}
	tc.logger.WithFields(log.Fields{
		"connections": count, "backend_count": len(td.TargetAddresses), "backend_port": td.TargetPort,
		"tls_server_name": tc.tlsconfig.ServerName, "verify_tls": !tc.tlsconfig.InsecureSkipVerify,
		"dial_timeout": tc.dialerTimeout, "reconnect_delay": tc.reconnectDelay,
	}).Debug("Starting tunnel client")
	tc.wg.Add(count)
	for i := 0; i < count; i++ {
		go tc.run(i)
	}
	return tc, nil
}

func (tc *MuxTunnelClient) run(id int) {
	defer tc.wg.Done()
	for tc.ctx.Err() == nil {
		tc.setConnectionState(id, ClientStateConnecting, nil)
		if err := tc.addBackendConnection(id); err != nil && tc.ctx.Err() == nil {
			log.Errorf("Tunnel connection to [%s] failed: %s", tc.apiEndpoint, redactClientError(err, tc.tunnelData.Token))
		}
		if tc.ctx.Err() != nil {
			return
		}
		tc.logger.WithField("reconnect_delay", tc.reconnectDelay).Debug("Waiting to reconnect tunnel")
		timer := time.NewTimer(tc.reconnectDelay)
		select {
		case <-tc.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Close cancels pending dials, registrations, streams and retries, and waits for
// the client's goroutines to exit. It is safe to call repeatedly or concurrently.
func (tc *MuxTunnelClient) Close() {
	tc.closeOnce.Do(func() {
		tc.logger.Debug("Stopping tunnel client")
		tc.mtx.Lock()
		tc.statusClosed = true
		for i := range tc.connections {
			conn := &tc.connections[i]
			conn.State, conn.UpdatedAt = ClientStateClosed, time.Now().UTC()
			conn.FrontendPort, conn.FrontendAddress, conn.PublicationMode = 0, "", ""
		}
		tc.notifyStatusLocked()
		tc.mtx.Unlock()
		tc.cancel()
	})
	tc.wg.Wait()
}

func (tc *MuxTunnelClient) TargetAddresses() []string {
	tc.mtx.Lock()
	defer tc.mtx.Unlock()
	return append([]string(nil), tc.tunnelData.TargetAddresses...)
}

func (tc *MuxTunnelClient) UpdateTargetAddresses(addrs []string) {
	tc.mtx.Lock()
	tc.tunnelData.TargetAddresses = append([]string(nil), addrs...)
	tc.mtx.Unlock()
}

func (tc *MuxTunnelClient) TargetPort() int {
	tc.mtx.Lock()
	defer tc.mtx.Unlock()
	return tc.tunnelData.TargetPort
}

func (tc *MuxTunnelClient) UpdateTargetPort(port int) {
	tc.mtx.Lock()
	tc.tunnelData.TargetPort = port
	tc.mtx.Unlock()
}

// FrontendPort returns the legacy requested/last-reported port, even before
// registration or after disconnection. Use Status or WaitReady for readiness
// and the currently confirmed frontend.
func (tc *MuxTunnelClient) FrontendPort() int {
	tc.mtx.Lock()
	defer tc.mtx.Unlock()
	return tc.tunnelData.FrontendData.Port
}

func (tc *MuxTunnelClient) addBackendConnection(id int) (err error) {
	tc.logger.Debug("Connecting to tunnel control plane")
	ctx, cancel := context.WithCancel(tc.ctx)
	var conn net.Conn
	var session *yamux.Session
	var streams sync.WaitGroup
	defer func() {
		tc.setConnectionState(id, ClientStateReconnecting, err)
		cancel()
		if session != nil {
			_ = session.Close()
		}
		if conn != nil {
			_ = conn.Close()
		}
		streams.Wait()
	}()
	dialCtx, dialCancel := context.WithTimeout(ctx, tc.dialerTimeout)
	dialer := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: tc.dialerTimeout},
		Config:    tc.tlsconfig,
	}
	conn, err = dialer.DialContext(dialCtx, "tcp", tc.apiEndpoint)
	dialCancel()
	if err != nil {
		return fmt.Errorf("control TLS dial: %w", err)
	}
	tc.setConnectionState(id, ClientStateRegistering, nil)
	tc.logger.WithFields(log.Fields{
		"local_address": conn.LocalAddr().String(), "remote_address": conn.RemoteAddr().String(),
	}).Debug("Control TLS connection established")
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	session, err = yamux.Client(conn, nil)
	if err != nil {
		return fmt.Errorf("establish session: %w", err)
	}
	// Also bound Open(), whose initial send is not controlled by stream deadlines.
	registrationTimer := time.AfterFunc(tc.dialerTimeout, func() { _ = conn.Close() })
	defer registrationTimer.Stop()
	control, err := session.Open()
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	tc.logger.WithFields(tunnelStreamFields(control)).Debug("Yamux control stream opened")
	if err := control.SetDeadline(time.Now().Add(tc.dialerTimeout)); err != nil {
		return err
	}
	tc.mtx.Lock()
	if tc.statusClosed {
		tc.mtx.Unlock()
		return net.ErrClosed
	}
	td := tc.tunnelData
	td.TargetAddresses = append([]string(nil), td.TargetAddresses...)
	tc.mtx.Unlock()
	tc.logger.WithFields(log.Fields{
		"requested_frontend_port": td.FrontendData.Port,
		"tls_wrap":                td.FrontendData.TLSWrap, "ssh_wrap": td.FrontendData.SSHWrap,
	}).Debug("Registering tunnel")
	if err := json.NewEncoder(control).Encode(td); err != nil {
		return fmt.Errorf("send registration: %w", err)
	}
	var response TunnelDataResponse
	if err := json.NewDecoder(control).Decode(&response); err != nil {
		return fmt.Errorf("read registration response: %w", err)
	}
	if response.Error != "" {
		return fmt.Errorf("registration rejected for service %q: %s", td.ServiceName, response.Error)
	}
	registrationTimer.Stop()
	if err := control.SetDeadline(time.Time{}); err != nil {
		return err
	}
	tc.mtx.Lock()
	if tc.statusClosed {
		tc.mtx.Unlock()
		return net.ErrClosed
	}
	tc.tunnelData.FrontendData.Port = response.FrontendPort
	tc.frontendAddress = ""
	if response.FrontendPort > 0 {
		host := response.FrontendAddress
		if host == "" {
			host = tc.apiEndpointHost
		}
		tc.frontendAddress = net.JoinHostPort(host, strconv.Itoa(response.FrontendPort))
	}
	tc.connections[id] = ClientConnectionStatus{
		ID: id, State: ClientStateReady, FrontendPort: response.FrontendPort,
		FrontendAddress: tc.frontendAddress, PublicationMode: response.PublicationMode,
		UpdatedAt: time.Now().UTC(),
	}
	tc.notifyStatusLocked()
	tc.mtx.Unlock()
	tc.logger.WithFields(tunnelStreamFields(control)).WithFields(log.Fields{
		"frontend_port": response.FrontendPort, "publication_mode": response.PublicationMode,
	}).Debug("Tunnel registration accepted")
	if response.PublicationMode == "bindings_only" {
		log.Infof("Tunnel binding ready: [%s] (no dedicated frontend)", response.ServiceName)
	} else {
		host := response.FrontendAddress
		if host == "" {
			host = tc.apiEndpointHost
		}
		log.Infof("Tunnel ready on frontend: [%s] => [%s]", response.ServiceName, net.JoinHostPort(host, strconv.Itoa(response.FrontendPort)))
	}
	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		streams.Add(1)
		go func() {
			defer streams.Done()
			tc.handleStream(ctx, stream)
		}()
	}
}

func (tc *MuxTunnelClient) handleStream(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	logger := tc.logger.WithFields(tunnelStreamFields(conn))
	logger.Debug("Receiving tunnel stream metadata")
	if err := conn.SetReadDeadline(time.Now().Add(tc.dialerTimeout)); err != nil {
		logger.WithError(err).Error("Set tunnel metadata deadline")
		return
	}
	b, _, err := readFrame(conn)
	if err != nil {
		logger.WithError(err).Error("Read tunnel metadata")
		return
	}
	var td TunnelConnecData
	if err := json.Unmarshal(b, &td); err != nil {
		logger.WithError(err).Error("Decode tunnel metadata")
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		logger.WithError(err).Error("Clear tunnel metadata deadline")
		return
	}
	tc.mtx.Lock()
	frontendPort, frontendAddress := tc.tunnelData.FrontendData.Port, tc.frontendAddress
	tc.mtx.Unlock()
	logger = logger.WithFields(log.Fields{
		"source_address": td.SourceAddress, "registered_service": td.ServiceName,
		"frontend_port": frontendPort, "frontend_address": frontendAddress,
	})
	logger.Debug("Accepted tunnel stream")
	if err := tc.doProxy(ctx, conn, logger); err != nil && ctx.Err() == nil {
		logger.WithError(err).Error("Proxy tunnel stream failed")
	}
}

func (tc *MuxTunnelClient) doProxy(ctx context.Context, conn net.Conn, logger *log.Entry) error {
	tc.mtx.Lock()
	if len(tc.tunnelData.TargetAddresses) == 0 {
		tc.mtx.Unlock()
		return fmt.Errorf("no backend target addresses configured")
	}
	host := tc.tunnelData.TargetAddresses[rand.Intn(len(tc.tunnelData.TargetAddresses))]
	port := tc.tunnelData.TargetPort
	tc.mtx.Unlock()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	logger = logger.WithField("backend", addr)
	logger.Debug("Opening backend connection")
	dialer := net.Dialer{Timeout: tc.dialerTimeout}
	backend, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial backend %s: %w", addr, err)
	}
	defer backend.Close()
	logger = logger.WithFields(log.Fields{
		"backend_local": backend.LocalAddr().String(), "backend_remote": backend.RemoteAddr().String(),
	})
	logger.Debug("Backend connection established")
	started := time.Now()
	err = tunnelcore.ProxyWithObserver(ctx, conn, backend,
		logTunnelCopy(logger, "tunnel_to_backend", "backend_to_tunnel", started))
	logger.WithField("duration", time.Since(started)).Debug("Tunnel stream finished")
	return err
}
