package tunnel

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
)

type MuxTunnelClient struct {
	apiEndpoint string
	tunnelData  TunnelData
	tlsconfig   *tls.Config

	dialerTimeout   time.Duration
	connectionCount int
	mtx             sync.Mutex

	closeCh chan bool
	closed  bool
}

// NewMuxTunnelClient creates a new service frontend
func NewMuxTunnelClient(apiEndpoint string, td TunnelData) (*MuxTunnelClient, error) {
	log.Infof("NewTunnelClient to apiEndpoint [%s] with tunnel info: [%v]", apiEndpoint, td)

	rand.Seed(time.Now().UnixNano())
	roots := x509.NewCertPool()
	//ok := roots.AppendCertsFromPEM([]byte(rootCert))
	//if !ok {
	//	log.Fatal("failed to parse root certificate")
	//}
	tlsconfig := &tls.Config{RootCAs: roots}
	//tlsconfig.ServerName = net.SplitHostPort()
	tlsconfig.InsecureSkipVerify = true

	tc := MuxTunnelClient{
		apiEndpoint:   apiEndpoint,
		tunnelData:    td,
		dialerTimeout: 10 * time.Second,
		tlsconfig:     tlsconfig,

		connectionCount: td.BackendAcceptBacklog,
		closeCh:         make(chan bool),
		closed:          false,
	}

	if td.BackendAcceptBacklog == 0 {
		tc.connectionCount = 1
	}

	for i := 0; i < tc.connectionCount; i++ {
		go func() {
			for {
				tc.addBackendConnection()

				select {
				case <-tc.closeCh:
					return
				default:
					time.Sleep(10 * time.Second)
				}
			}
		}()
	}

	return &tc, nil
}

// Close closes the frontends
func (tc *MuxTunnelClient) Close() {
	log.Infof("Close to apiEndpoint [%s] with tunnel info: [%v]", tc.apiEndpoint, tc.tunnelData)

	tc.mtx.Lock()
	tc.closed = true

	close(tc.closeCh)
	tc.mtx.Unlock()
}

func (tc *MuxTunnelClient) TargetAddresses() []string {
	tc.mtx.Lock()
	endpoints := tc.tunnelData.TargetAddresses
	tc.mtx.Unlock()

	return endpoints
}

// UpdateTargetAddresses updates the list of backend addresses
func (tc *MuxTunnelClient) UpdateTargetAddresses(addrs []string) {
	tc.mtx.Lock()
	tc.tunnelData.TargetAddresses = addrs
	tc.mtx.Unlock()
}

// TargetPort returns the current service port
func (tc *MuxTunnelClient) TargetPort() int {
	tc.mtx.Lock()
	defer tc.mtx.Unlock()
	return tc.tunnelData.TargetPort
}

// UpdateTargetPort updates the backend target port
func (tc *MuxTunnelClient) UpdateTargetPort(port int) {
	tc.mtx.Lock()
	tc.tunnelData.TargetPort = port
	tc.mtx.Unlock()
}

// FrontendPort returns the current frontend port
func (tc *MuxTunnelClient) FrontendPort() int {
	tc.mtx.Lock()
	defer tc.mtx.Unlock()
	return tc.tunnelData.FrontendData.Port
}

func (tc *MuxTunnelClient) addBackendConnection() error {

	log.Debugf("Trying to reconnect to api server [%s] with tunnel info: [%v]", tc.apiEndpoint, tc)
	tc.mtx.Lock()
	if tc.closed {
		tc.mtx.Unlock()
		return fmt.Errorf("TunnelListener already closed")
	}
	tc.mtx.Unlock()

	dialerTimeout := 10 * time.Second
	tcpAddr, err := net.ResolveTCPAddr("tcp", tc.apiEndpoint)
	if err != nil {
		log.Errorf("failed to resolve server [%s]: %s", tcpAddr, err.Error())
		return err
	}

	d := net.Dialer{Timeout: dialerTimeout}
	tcpconn, err := d.Dial("tcp", tc.apiEndpoint)
	if err != nil {
		log.Errorf("failed to connect to [%s]: %s", tc.apiEndpoint, err.Error())
		return err
	}
	conn := tls.Client(tcpconn, tc.tlsconfig)
	err = conn.Handshake()
	if err != nil {
		log.Errorf("TLS handshake failed: [%s]: %s", tc.apiEndpoint, err.Error())
		return err
	}

	defer conn.Close()
	defer tcpconn.Close()

	// Serialize the tunnel info
	session, err := yamux.Client(conn, nil)
	if err != nil {
		log.Errorf("Failed to establish session: [%s]: %s", tc.apiEndpoint, err.Error())
		return err
	}
	defer session.Close()

	stream, err := session.Open()
	if err != nil {
		log.Errorf("Failed to create controller stream: [%s]: %s", tc.apiEndpoint, err.Error())
		return err
	}
	defer stream.Close()

	o := json.NewEncoder(stream)

	err = o.Encode(tc.tunnelData)
	if err != nil {
		log.Errorf("Failed sending tunnel info:[%s], stream:[%s], %s: %s", session.RemoteAddr().String(), tc.apiEndpoint, err.Error())
		return err
	}

	log.Infof("Added backend connection [%s] with tunnel info: [%v]", conn.LocalAddr().String(), tc.tunnelData)

	// Accept a new stream
	go func() {
		for {
			stream, err := session.Accept()
			if err != nil {
				log.Info("tunnel client service session accept error", err)
				break
			}

			go tc.handleStream(session, stream)
		}
	}()

	in := json.NewDecoder(stream)

	var tdr TunnelDataResponse
	err = in.Decode(&tdr)
	if err != nil {
		err := fmt.Errorf("failed to deserialize tunnel info [%s]", err.Error())
		log.Error(err.Error())
		return err
	}

	if tdr.Error != "" {
		err := fmt.Errorf("Tunnel failed to setup frontend: [%s] => [%s]", tdr.ServiceName, tdr.Error)
		log.Error(err.Error())
		return err
	}

	tc.mtx.Lock()
	tc.tunnelData.FrontendData.Port = tdr.FrontendPort
	tc.mtx.Unlock()
	log.Infof("Tunnel ready on frontend: [%s] => [%s:%d]", tdr.ServiceName, tc.apiEndpoint, tdr.FrontendPort)

	// check for closed session (closed/keepalive failed/etc) or shutdown
loop:
	for {
		select {
		case <-time.After(5 * time.Second):
			if session.IsClosed() {
				break loop
			}
		case <-tc.closeCh:
			break loop
		}
	}

	return nil
}

func (tc *MuxTunnelClient) handleStream(session *yamux.Session, conn net.Conn) {
	log.Debugf("session: [%s], new stream connection from: %s", tc.tunnelData.ServiceName, conn.RemoteAddr().String())

	defer func() {
		conn.Close()
		log.Debugf("session: [%s], stream from [%s] ended", tc.tunnelData.ServiceName, conn.RemoteAddr().String())
	}()

	b, payloadLen, err := readFrame(conn)
	if err != nil {
		log.Errorf("readFrame error: [%s]", err.Error())
		return
	}

	// Deserialize the tunnel info
	var td TunnelConnecData
	err = json.Unmarshal(b[0:payloadLen], &td)
	if err != nil {
		log.Errorf("failed to deserialize tunnel connect data [%s]", err.Error())
		return
	}

	log.Debugf("session: [%s], new stream connection from: [%s] via [%s]", td.ServiceName, td.SourceAddress, conn.RemoteAddr().String())

	err = tc.doProxy(conn)
	if err == nil {
	}
}

func (tc *MuxTunnelClient) doProxy(conn net.Conn) error {
	var addr string
	tc.mtx.Lock()

	// Pick a random backend
	l := len(tc.tunnelData.TargetAddresses)
	if l == 0 {
		tc.mtx.Unlock()
		return nil
	}

	addr = tc.tunnelData.TargetAddresses[rand.Intn(l)]
	tc.mtx.Unlock()

	addr = fmt.Sprintf("%s:%d", addr, tc.tunnelData.TargetPort)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		log.Errorf("session: [%s], failed to resolve server [%s]: %s", tc.tunnelData.ServiceName, tcpAddr.String(), err.Error())
		return err
	}

	d := net.Dialer{Timeout: tc.dialerTimeout}
	backConn, err := d.Dial("tcp", tcpAddr.String())
	if err != nil {
		log.Errorf("session: [%s], failed to connect to [%s]: %s", tc.tunnelData.ServiceName, tcpAddr.String(), err.Error())
		return err
	}

	go func() {
		l, err := tc.copybytes(backConn, conn)
		log.Debugf("proxy connection [%s] finished content -> back (bytes %d, error: [%v]", tc.tunnelData.ServiceName, l, err)
	}()

	{
		l, err := tc.copybytes(conn, backConn)
		log.Debugf("proxy connection [%s] finished back -> content (bytes %d, error: [%v]", tc.tunnelData.ServiceName, l, err)
	}

	return nil
}

func (tc *MuxTunnelClient) copybytes(dst net.Conn, src net.Conn) (written int64, err error) {
	written, err = io.Copy(dst, src)
	dst.Close()
	src.Close()

	return written, err
}
