package tunnel

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type TunnelClient struct {
	apiEndpoint   string
	tunnelData    TunnelData
	acceptBacklog int
	tlsconfig     *tls.Config

	timeout time.Duration

	connectionMap   map[string]net.Conn
	connectionCount int
	mtx             sync.Mutex

	abort  chan bool
	closed bool
}

func NewTunnelClient(apiEndpoint string, td TunnelData) (*TunnelClient, error) {
	log.Infof("NewTunnelClient to apiEndpoint [%s] with tunnel info: [%v]", apiEndpoint, td)

	roots := x509.NewCertPool()
	//ok := roots.AppendCertsFromPEM([]byte(rootCert))
	//if !ok {
	//	log.Fatal("failed to parse root certificate")
	//}
	tlsconfig := &tls.Config{RootCAs: roots}
	//tlsconfig.ServerName = net.SplitHostPort()
	tlsconfig.InsecureSkipVerify = true

	tc := TunnelClient{
		apiEndpoint:   apiEndpoint,
		tunnelData:    td,
		acceptBacklog: td.BackendAcceptBacklog,
		timeout:       10 * time.Second,
		connectionMap: make(map[string]net.Conn),
		tlsconfig:     tlsconfig,

		connectionCount: 0,
		abort:           make(chan bool),
		closed:          false,
	}

	if td.BackendAcceptBacklog == 0 {
		tc.acceptBacklog = 1
	}

	go tc.loopReconnect()

	return &tc, nil
}

func (tc *TunnelClient) Close() {
	log.Infof("Close to apiEndpoint [%s] with tunnel info: [%v]", tc.apiEndpoint, tc.tunnelData)

	tc.mtx.Lock()
	tc.closed = true
	for _, conn := range tc.connectionMap {
		conn.Close()
	}
	close(tc.abort)
	tc.mtx.Unlock()
}

func (tc *TunnelClient) UpdateTargetAddresses(addrs []string) {
	tc.mtx.Lock()
	tc.tunnelData.TargetAddresses = addrs
	tc.mtx.Unlock()
}

func (tc *TunnelClient) UpdateTargetPort(port int) {
	tc.mtx.Lock()
	tc.tunnelData.TargetPort = port
	tc.mtx.Unlock()
}

func (tc *TunnelClient) TargetAddresses() []string {
	tc.mtx.Lock()
	endpoints := tc.tunnelData.TargetAddresses
	tc.mtx.Unlock()

	return endpoints
}

func (tc *TunnelClient) loopReconnect() {
loop:
	for {
		if len(tc.connectionMap) == 0 {
			log.Debugf("Trying to reconnect to api server: [%s], [%v]", tc.apiEndpoint, tc)

			for i := 0; i < tc.acceptBacklog; i++ {
				err := tc.addBackendConnection()
				if err != nil {
					log.Errorf("failed to addBackendConnection: %s", err.Error())
				}
			}
		}

		select {
		case <-time.After(5 * time.Second):

		case <-tc.abort:
			break loop
		}
	}
}

func (tc *TunnelClient) addBackendConnection() error {
	var connp *tls.Conn

	defer func() {
		if connp != nil {
			(*connp).Close()
		}
	}()

	log.Debugf("Trying to add backend connection [%s] with tunnel info: [%v]", tc.apiEndpoint, tc)
	tc.mtx.Lock()
	if tc.closed {
		tc.mtx.Unlock()
		return fmt.Errorf("TunnelListener already closed")
	}
	tc.mtx.Unlock()

	timeout := 10 * time.Second
	tcpAddr, err := net.ResolveTCPAddr("tcp", tc.apiEndpoint)
	if err != nil {
		log.Errorf("failed to resolve server [%s]: %s", tcpAddr, err.Error())
		return err
	}

	d := net.Dialer{Timeout: timeout}
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

	connp = conn

	//tcpConn, ok := conn.(*net.TCPConn)
	// Serialize the tunnel info
	tc.mtx.Lock()
	tc.connectionCount = tc.connectionCount + 1
	tc.tunnelData.ID = strconv.Itoa(tc.connectionCount)
	tc.mtx.Unlock()

	err = sendSerializedObject(conn, tc.tunnelData)
	if err != nil {
		return err
	}

	log.Debugf("Added backend connection [%s] with tunnel info: [%v]", conn.LocalAddr().String(), tc.tunnelData)

	connp = nil
	tc.mtx.Lock()
	tc.connectionMap[conn.LocalAddr().String()] = conn
	tc.mtx.Unlock()

	// Start "listening" for connections on the tunnel
	go tc.listen(conn)

	return nil
}

func (tc *TunnelClient) listen(conn net.Conn) {
	connp := &conn
	log.Infof("service [%s] standing by waiting for frontend connections: %s",
		tc.tunnelData.ServiceName, conn.LocalAddr().String())

	defer func() {
		if connp != nil {
			tc.mtx.Lock()
			delete(tc.connectionMap, (*connp).LocalAddr().String())
			tc.mtx.Unlock()
			(*connp).Close()

			log.Infof("connection from [%s] ended", conn.RemoteAddr().String())
		}
	}()

	b, payloadLen, err := readFrame(conn)
	if err != nil {
		log.Errorf("readFrame error: [%s]", err.Error())
		return
	}

	log.Debugf("Got new backend connection info: [%s]", string(b))

	// add a new connection to the pool.
	go func() {
		err = tc.addBackendConnection()
		if err != nil {
			log.Errorf("failed to addBackendConnection: %s", err.Error())
		}
	}()

	// Deserialize the tunnel info
	var td TunnelConnecData
	err = json.Unmarshal(b[0:payloadLen], &td)
	if err != nil {
		log.Errorf("failed to deserialize tunnel connect data [%s]", err.Error())
		return
	}

	if len(tc.tunnelData.TargetAddresses) > 0 {
		err = tc.doProxy(conn)
		if err == nil {
			connp = nil
		}
	}
}

func (tc *TunnelClient) doProxy(conn net.Conn) error {

	var addr string
	tc.mtx.Lock()

	// #DARIO TODO: do lb to the backends
	addr = tc.tunnelData.TargetAddresses[0]
	tc.mtx.Unlock()

	addr = fmt.Sprintf("%s:%d", addr, tc.tunnelData.TargetPort)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		log.Errorf("failed to resolve server [%s]: %s", tcpAddr.String(), err.Error())
		return err
	}

	d := net.Dialer{Timeout: tc.timeout}
	backConn, err := d.Dial("tcp", tcpAddr.String())
	if err != nil {
		log.Errorf("failed to connect to [%s]: %s", tcpAddr.String(), err.Error())
		return err
	}

	go func() {
		l, err := tc.copybytes(backConn, conn)
		log.Debugf("proxy connection [%s] finished content -> back (bytes %d, error: [%v]", tc.tunnelData.ServiceName, l, err)
	}()

	go func() {
		l, err := tc.copybytes(conn, backConn)
		log.Debugf("proxy connection [%s] finished back -> content (bytes %d, error: [%v]", tc.tunnelData.ServiceName, l, err)
	}()

	return nil
}

func (tc *TunnelClient) copybytes(dst net.Conn, src net.Conn) (written int64, err error) {
	written, err = io.Copy(dst, src)

	tc.mtx.Lock()
	delete(tc.connectionMap, dst.LocalAddr().String())
	tc.mtx.Unlock()

	dst.Close()
	src.Close()

	return written, err
}
