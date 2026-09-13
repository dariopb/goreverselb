package tunnel

import (
	"crypto/subtle"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	gossh "golang.org/x/crypto/ssh"
)

const DefaultSSHBackendUser = "reverseuser"

const sshBackendHandshakeTimeout = 15 * time.Second

type sshForwardRequest struct {
	BindAddress string
	BindPort    uint32
}

type sshForwardedTCPIP struct {
	ConnectedAddress  string
	ConnectedPort     uint32
	OriginatorAddress string
	OriginatorPort    uint32
}

type sshReverseBackend struct {
	id           string
	conn         *gossh.ServerConn
	frontendPort int
	bindAddress  string
	registeredAt time.Time
	closed       bool
	mtx          sync.Mutex
}

type sshReverseFrontend struct {
	serviceName string
	port        int
	listener    net.Listener
	backends    map[string]*sshReverseBackend
}

func (backend *sshReverseBackend) isClosed() bool {
	backend.mtx.Lock()
	defer backend.mtx.Unlock()
	return backend.closed
}

func (backend *sshReverseBackend) markClosed() {
	backend.mtx.Lock()
	backend.closed = true
	backend.mtx.Unlock()
}

func (backend *sshReverseBackend) openConnection(frontendConn net.Conn) (net.Conn, error) {
	if backend.isClosed() {
		return nil, fmt.Errorf("SSH backend connection is closed")
	}

	originatorHost, originatorPort, err := splitAddress(frontendConn.RemoteAddr())
	if err != nil {
		return nil, err
	}

	payload := gossh.Marshal(&sshForwardedTCPIP{
		ConnectedAddress:  backend.bindAddress,
		ConnectedPort:     uint32(backend.frontendPort),
		OriginatorAddress: originatorHost,
		OriginatorPort:    uint32(originatorPort),
	})
	channel, requests, err := backend.conn.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		return nil, err
	}
	go gossh.DiscardRequests(requests)

	localAddress := &net.TCPAddr{IP: net.IPv4zero, Port: backend.frontendPort}
	remoteAddress := &net.TCPAddr{IP: net.ParseIP(originatorHost), Port: originatorPort}
	return newChannelConn(channel, localAddress, remoteAddress), nil
}

func splitAddress(address net.Addr) (string, int, error) {
	host, portText, err := net.SplitHostPort(address.String())
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func validSSHForwardBindAddress(address string) bool {
	switch address {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0", "::", "*":
		return true
	default:
		return false
	}
}

func (ts *MuxTunnelService) StartSSHBackend(port int, username string) error {
	if port == 0 {
		return nil
	}
	if strings.TrimSpace(username) == "" {
		return fmt.Errorf("SSH backend username must not be empty")
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return err
	}

	ts.mtx.Lock()
	if ts.sshBackendListener != nil {
		ts.mtx.Unlock()
		_ = listener.Close()
		return fmt.Errorf("SSH backend listener is already running")
	}
	ts.sshBackendListener = listener
	ts.mtx.Unlock()

	log.Infof("SSH backend listener started on %s", listener.Addr().String())
	go ts.acceptSSHBackends(listener, username)
	return nil
}

func (ts *MuxTunnelService) acceptSSHBackends(listener net.Listener, username string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ts.closeCh:
				log.Debug("SSH backend listener stopped")
				return
			default:
				log.Errorf("SSH backend accept failed: %v", err)
				return
			}
		}
		log.WithFields(log.Fields{
			"local":  conn.LocalAddr().String(),
			"remote": conn.RemoteAddr().String(),
		}).Debug("SSH backend TCP connection accepted")
		go ts.handleSSHBackendConnection(conn, username)
	}
}

func (ts *MuxTunnelService) handleSSHBackendConnection(conn net.Conn, username string) {
	remoteAddress := conn.RemoteAddr().String()
	connectionLog := log.WithField("remote", remoteAddress)
	ts.mtx.Lock()
	ts.sshBackendRawConns[conn] = struct{}{}
	ts.mtx.Unlock()
	defer func() {
		ts.mtx.Lock()
		delete(ts.sshBackendRawConns, conn)
		ts.mtx.Unlock()
		_ = conn.Close()
		connectionLog.Debug("SSH backend TCP connection closed")
	}()
	_ = conn.SetDeadline(time.Now().Add(sshBackendHandshakeTimeout))
	connectionLog.Debug("SSH backend handshake started")

	config := &gossh.ServerConfig{
		MaxAuthTries: 1,
		PasswordCallback: func(metadata gossh.ConnMetadata, password []byte) (*gossh.Permissions, error) {
			if metadata.User() != username {
				_ = conn.Close()
				connectionLog.Warn("SSH backend username mismatch")
				return nil, fmt.Errorf("SSH backend authentication failed")
			}

			ts.mtx.Lock()
			user, ok := ts.configData.Users[DefaultUserID]
			expectedToken := ""
			if ok && user != nil {
				expectedToken = user.Token
			}
			ts.mtx.Unlock()

			if !ok || user == nil || subtle.ConstantTimeCompare(password, []byte(expectedToken)) != 1 {
				connectionLog.Warn("SSH backend authentication failed")
				return nil, fmt.Errorf("SSH backend authentication failed")
			}
			connectionLog.Info("SSH backend authenticated")
			return nil, nil
		},
	}
	config.AddHostKey(ts.sshSigner)

	serverConn, channels, requests, err := gossh.NewServerConn(conn, config)
	if err != nil {
		connectionLog.Debugf("SSH backend handshake ended: %v", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	backendID := fmt.Sprintf("%p", serverConn)
	connectionLog = connectionLog.WithField("backend", backendID)
	connectionLog.Debug("SSH backend transport established")

	ts.mtx.Lock()
	ts.sshBackendConns[serverConn] = struct{}{}
	ts.mtx.Unlock()

	go rejectSSHBackendChannels(channels)
	ts.handleSSHBackendRequests(serverConn, requests)
	connectionLog.Debug("SSH backend request stream closed")

	ts.removeSSHBackend(serverConn)
	ts.mtx.Lock()
	delete(ts.sshBackendConns, serverConn)
	ts.mtx.Unlock()
	_ = serverConn.Close()
	connectionLog.Info("SSH backend disconnected")
}

func rejectSSHBackendChannels(channels <-chan gossh.NewChannel) {
	for channel := range channels {
		_ = channel.Reject(gossh.Prohibited, "SSH backend accepts remote forwarding only")
	}
}

func (ts *MuxTunnelService) handleSSHBackendRequests(conn *gossh.ServerConn, requests <-chan *gossh.Request) {
	backendID := fmt.Sprintf("%p", conn)
	for request := range requests {
		switch request.Type {
		case "tcpip-forward":
			port, dynamic, ok := ts.registerSSHForward(conn, request.Payload)
			var payload []byte
			if ok && dynamic {
				payload = gossh.Marshal(struct{ Port uint32 }{Port: uint32(port)})
			}
			log.WithFields(log.Fields{
				"backend": backendID,
				"dynamic": dynamic,
				"port":    port,
				"success": ok,
			}).Debug("SSH reverse forward request completed")
			_ = request.Reply(ok, payload)
		case "cancel-tcpip-forward":
			port, ok := ts.cancelSSHForward(conn, request.Payload)
			log.WithFields(log.Fields{
				"backend": backendID,
				"port":    port,
				"success": ok,
			}).Debug("SSH reverse forward cancellation completed")
			_ = request.Reply(ok, nil)
		case "keepalive@openssh.com":
			log.WithField("backend", backendID).Debug("SSH backend keepalive received")
			_ = request.Reply(true, nil)
		default:
			log.WithFields(log.Fields{
				"backend": backendID,
				"request": request.Type,
			}).Debug("SSH backend request rejected")
			_ = request.Reply(false, nil)
		}
	}
}

func (ts *MuxTunnelService) registerSSHForward(conn *gossh.ServerConn, payload []byte) (int, bool, bool) {
	backendID := fmt.Sprintf("%p", conn)
	var request sshForwardRequest
	if err := gossh.Unmarshal(payload, &request); err != nil {
		log.WithFields(log.Fields{
			"backend": backendID,
			"error":   err,
		}).Debug("SSH reverse forward request rejected: invalid payload")
		return 0, false, false
	}
	requestLog := log.WithFields(log.Fields{
		"backend":        backendID,
		"bind_address":   request.BindAddress,
		"requested_port": request.BindPort,
	})
	requestLog.Debug("SSH reverse forward requested")
	if !validSSHForwardBindAddress(request.BindAddress) {
		requestLog.Debug("SSH reverse forward request rejected: unsupported bind address")
		return 0, false, false
	}
	if request.BindPort > 65535 {
		requestLog.Debug("SSH reverse forward request rejected: invalid port")
		return 0, false, false
	}

	dynamic := request.BindPort == 0
	ts.mtx.Lock()
	defer ts.mtx.Unlock()

	for _, frontend := range ts.sshReverseFrontends {
		for _, backend := range frontend.backends {
			if backend.conn != conn {
				continue
			}
			if request.BindPort == 0 || int(request.BindPort) == frontend.port {
				requestLog.WithField("port", frontend.port).Debug("SSH reverse forward already registered")
				return frontend.port, dynamic, true
			}
			requestLog.WithField("registered_port", frontend.port).Debug("SSH reverse forward request rejected: connection already owns another port")
			return 0, false, false
		}
	}

	requestedPort := int(request.BindPort)
	if requestedPort != 0 {
		if existing := ts.sshReverseFrontends[requestedPort]; existing != nil {
			port := ts.addSSHBackendLocked(existing, conn, request.BindAddress)
			requestLog.WithFields(log.Fields{
				"port":     port,
				"backends": len(existing.backends),
			}).Debug("SSH reverse backend joined existing frontend")
			return port, false, true
		}
		if ts.yamuxFrontendUsesPortLocked(requestedPort) {
			requestLog.Debug("SSH reverse forward request rejected: port owned by yamux frontend")
			return 0, false, false
		}
	}

	allocatedPort, err := ts.frontendPortPool.GetElement(requestedPort)
	if err != nil {
		requestLog.WithField("error", err).Debug("SSH reverse forward request rejected: port unavailable")
		return 0, false, false
	}
	requestLog.WithFields(log.Fields{
		"allocated_port": allocatedPort,
		"dynamic":        dynamic,
	}).Debug("SSH reverse frontend port allocated")
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", allocatedPort))
	if err != nil {
		_ = ts.frontendPortPool.ReturnElement(allocatedPort)
		requestLog.WithFields(log.Fields{
			"allocated_port": allocatedPort,
			"error":          err,
		}).Error("SSH reverse frontend listener failed; port returned to pool")
		return 0, false, false
	}

	frontend := &sshReverseFrontend{
		serviceName: fmt.Sprintf("ssh-reverse:%d", allocatedPort),
		port:        allocatedPort,
		listener:    listener,
		backends:    make(map[string]*sshReverseBackend),
	}
	ts.sshReverseFrontends[allocatedPort] = frontend
	ts.addSSHBackendLocked(frontend, conn, request.BindAddress)
	go ts.acceptSSHReverseFrontend(frontend)
	requestLog.WithFields(log.Fields{
		"allocated_port": allocatedPort,
		"dynamic":        dynamic,
	}).Info("SSH reverse frontend listening")
	return allocatedPort, dynamic, true
}

func (ts *MuxTunnelService) addSSHBackendLocked(frontend *sshReverseFrontend, conn *gossh.ServerConn, bindAddress string) int {
	id := fmt.Sprintf("%p", conn)
	frontend.backends[id] = &sshReverseBackend{
		id:           id,
		conn:         conn,
		frontendPort: frontend.port,
		bindAddress:  bindAddress,
		registeredAt: time.Now(),
	}
	return frontend.port
}

func (ts *MuxTunnelService) yamuxFrontendUsesPortLocked(port int) bool {
	for _, services := range ts.frontendMap {
		for _, frontend := range services {
			if frontend.port == port {
				return true
			}
		}
	}
	return false
}

func (ts *MuxTunnelService) cancelSSHForward(conn *gossh.ServerConn, payload []byte) (int, bool) {
	var request sshForwardRequest
	if err := gossh.Unmarshal(payload, &request); err != nil || request.BindPort == 0 || request.BindPort > 65535 {
		return 0, false
	}

	port := int(request.BindPort)
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	frontend := ts.sshReverseFrontends[port]
	if frontend == nil {
		return port, false
	}
	id := fmt.Sprintf("%p", conn)
	backend := frontend.backends[id]
	if backend == nil || backend.conn != conn {
		return port, false
	}
	ts.removeSSHBackendLocked(frontend, backend, "client cancellation")
	return port, true
}

func (ts *MuxTunnelService) removeSSHBackend(conn *gossh.ServerConn) {
	ts.mtx.Lock()
	defer ts.mtx.Unlock()
	id := fmt.Sprintf("%p", conn)
	for _, frontend := range ts.sshReverseFrontends {
		if backend := frontend.backends[id]; backend != nil {
			ts.removeSSHBackendLocked(frontend, backend, "transport disconnected")
		}
	}
}

func (ts *MuxTunnelService) removeSSHBackendLocked(frontend *sshReverseFrontend, backend *sshReverseBackend, reason string) {
	backend.markClosed()
	delete(frontend.backends, backend.id)
	log.WithFields(log.Fields{
		"backend":   backend.id,
		"port":      frontend.port,
		"reason":    reason,
		"remaining": len(frontend.backends),
	}).Debug("SSH reverse backend registration removed")
	if len(frontend.backends) != 0 {
		return
	}
	delete(ts.sshReverseFrontends, frontend.port)
	_ = frontend.listener.Close()
	_ = ts.frontendPortPool.ReturnElement(frontend.port)
	log.WithFields(log.Fields{
		"port":   frontend.port,
		"reason": reason,
	}).Info("SSH reverse frontend closed and port returned to pool")
}

func (ts *MuxTunnelService) acceptSSHReverseFrontend(frontend *sshReverseFrontend) {
	frontendLog := log.WithField("port", frontend.port)
	for {
		conn, err := frontend.listener.Accept()
		if err != nil {
			frontendLog.Debugf("SSH reverse frontend accept loop ended: %v", err)
			return
		}
		frontendLog.WithFields(log.Fields{
			"local":  conn.LocalAddr().String(),
			"remote": conn.RemoteAddr().String(),
		}).Debug("SSH reverse frontend connection accepted")
		go ts.proxySSHReverseConnection(frontend.port, conn)
	}
}

func (ts *MuxTunnelService) proxySSHReverseConnection(port int, frontendConn net.Conn) {
	connectionLog := log.WithFields(log.Fields{
		"port":   port,
		"local":  frontendConn.LocalAddr().String(),
		"remote": frontendConn.RemoteAddr().String(),
	})
	defer func() {
		_ = frontendConn.Close()
		connectionLog.Debug("SSH reverse frontend connection closed")
	}()

	ts.mtx.Lock()
	frontend := ts.sshReverseFrontends[port]
	backends := make([]*sshReverseBackend, 0)
	if frontend != nil {
		for _, backend := range frontend.backends {
			if !backend.isClosed() {
				backends = append(backends, backend)
			}
		}
	}
	ts.mtx.Unlock()
	connectionLog.WithField("backends", len(backends)).Debug("SSH reverse backend selection started")

	rand.Shuffle(len(backends), func(first, second int) {
		backends[first], backends[second] = backends[second], backends[first]
	})

	var backendConn net.Conn
	var selectedBackend *sshReverseBackend
	for _, backend := range backends {
		connectionLog.WithField("backend", backend.id).Debug("Opening SSH forwarded-tcpip channel")
		conn, err := backend.openConnection(frontendConn)
		if err == nil {
			backendConn = conn
			selectedBackend = backend
			break
		}
		connectionLog.WithFields(log.Fields{
			"backend": backend.id,
			"error":   err,
		}).Warn("SSH reverse channel open failed")
	}
	if backendConn == nil {
		connectionLog.Warn("SSH reverse frontend connection closed: no backend accepted the channel")
		return
	}
	defer backendConn.Close()
	connectionLog = connectionLog.WithField("backend", selectedBackend.id)
	connectionLog.Debug("SSH reverse proxy established")

	type copyResult struct {
		direction string
		bytes     int64
		err       error
	}
	done := make(chan copyResult, 2)
	go func() {
		written, err := ts.copybytes("", "", backendConn, frontendConn)
		done <- copyResult{direction: "frontend-to-backend", bytes: written, err: err}
	}()
	go func() {
		written, err := ts.copybytes("", "", frontendConn, backendConn)
		done <- copyResult{direction: "backend-to-frontend", bytes: written, err: err}
	}()
	for completed := 0; completed < 2; completed++ {
		result := <-done
		connectionLog.WithFields(log.Fields{
			"bytes":     result.bytes,
			"direction": result.direction,
			"error":     result.err,
		}).Debug("SSH reverse proxy direction closed")
	}
	connectionLog.Debug("SSH reverse proxy closed")
}
