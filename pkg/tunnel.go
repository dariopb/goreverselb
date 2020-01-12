package tunnel

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type backendRuntimeData struct {
	conn         net.Conn
	frontendConn net.Conn
}

type frontendRuntimeData struct {
	serviceName        string
	port               int
	listener           net.Listener
	lastBackendRemoval time.Time
	backendConnMap     map[string]*backendRuntimeData
}

type FrontendData struct {
	Port int `yaml:"port" json:"port"`
}

type TunnelData struct {
	ID           string       `yaml:"id" json:"id"`
	ServiceName  string       `yaml:"serviceName" json:"serviceName"`
	Token        string       `yaml:"token" json:"token"`
	FrontendData FrontendData `yaml:"frontendData" json:"frontendData"`

	BackendAcceptBacklog int
	TargetPort           int
	TargetAddresses      []string
}

type TunnelService struct {
	Port      int `yaml:"port" json:"port"`
	token     string
	tlsconfig *tls.Config

	frontendMap map[string]*frontendRuntimeData
	mtx         sync.Mutex
}

type TunnelConnecData struct {
	ID            string `yaml:"id" json:"id"`
	ServiceName   string `yaml:"serviceName" json:"serviceName"`
	SourceAddress string `yaml:"sourceAddress" json:"sourceAddress"`
}

func (frd *frontendRuntimeData) removeBackendConn(id string) {
	log.Debugf("frontend removeBackendConn for: [%s]", id)

	delete(frd.backendConnMap, id)
	frd.lastBackendRemoval = time.Now()
}

func readFrame(conn net.Conn) ([]byte, int, error) {
	b := make([]byte, 1000)
	l, err := conn.Read(b[0:2])
	if err != nil || l != 2 {
		return nil, 0, fmt.Errorf("Bad frame format")
	}
	payloadLen := binary.LittleEndian.Uint16(b[0:2])
	if payloadLen > uint16(len(b)) {
		return nil, 0, fmt.Errorf("payload len too long: [%d]", payloadLen)
	}

	l, err = conn.Read(b[0:payloadLen])
	if err != nil || uint16(l) != payloadLen {
		return nil, 0, fmt.Errorf("Not enought contiguous data")
	}

	return b[:payloadLen], int(payloadLen), nil
}

func sendSerializedObject(conn net.Conn, obj interface{}) error {
	b, err := json.Marshal(obj)
	if err != nil {
		log.Errorf("failed to serialize data [%s]", err.Error())
		return err
	}

	payloadLen := uint16(len(b))
	l := make([]byte, 2)
	binary.LittleEndian.PutUint16(l, payloadLen)

	ll, err := conn.Write(l)
	if uint16(ll) != 2 {
		return err
	}
	ll, err = conn.Write(b)
	if uint16(ll) != payloadLen {
		return err
	}

	return nil
}

// NewTunnelService creates a new tunnel service on the port using the passed cert
func NewTunnelService(cert tls.Certificate, servicePort int, token string) (*TunnelService, error) {
	ts := TunnelService{
		Port:        servicePort,
		token:       token,
		tlsconfig:   &tls.Config{Certificates: []tls.Certificate{cert}},
		frontendMap: make(map[string]*frontendRuntimeData),
	}

	l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", ts.Port))
	if err != nil {
		log.Fatal("tunnel service listener error:", err)
	}
	listener := l.(*net.TCPListener)

	log.Infof("tunnel service listening on: tcp => %s", listener.Addr().String())

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				log.Info("tunnel service  accept error", err)
			}

			go ts.handleConnection(conn)
		}
	}()

	go ts.monitorBackends()

	return &ts, err
}

func (ts *TunnelService) monitorBackends() {
	for {
		time.Sleep(10 * time.Second)

		ts.mtx.Lock()
		for _, frd := range ts.frontendMap {
			if len(frd.backendConnMap) == 0 &&
				time.Now().Sub(frd.lastBackendRemoval) > 10*time.Second {
				log.Infof("Removing frontend for service %s", frd.serviceName)
				delete(ts.frontendMap, frd.serviceName)
				frd.listener.Close()
			}
		}
		ts.mtx.Unlock()
	}
}

func (ts *TunnelService) handleConnection(tcpconn *net.TCPConn) {
	log.Infof("new connection from: %s", tcpconn.RemoteAddr().String())

	tcpconn.SetKeepAlive(true)
	tcpconn.SetKeepAlivePeriod(2 * time.Second)

	conn := tls.Server(tcpconn, ts.tlsconfig)
	connp := &conn

	defer func() {
		if connp != nil {
			(*connp).Close()
			log.Infof("connection from [%s] ended", conn.RemoteAddr().String())
		}
	}()

	err := conn.Handshake()
	if err != nil {
		log.Errorf("tls handshake error: [%s]", err.Error())
		return
	}

	{
		b, payloadLen, err := readFrame(conn)
		if err != nil {
			log.Errorf("readFrame error: [%s]", err.Error())
			return
		}
		log.Debugf("Got tunnel info: [%s]", string(b))

		// Deserialize the tunnel info
		var td TunnelData
		err = json.Unmarshal(b[0:payloadLen], &td)
		if err != nil {
			log.Errorf("failed to deserialize tunnel info [%s]", err.Error())
			return
		}

		if td.Token != ts.token {
			log.Errorf("authorization failed, wrong token value")
			return
		}

		// Everything is alright, try to associate it with the frontend
		ts.mtx.Lock()
		var val *frontendRuntimeData
		//var ok bool
		val, _ = ts.frontendMap[td.ServiceName]

		if val == nil || val.port != td.FrontendData.Port {
			if val != nil && val.port != td.FrontendData.Port {
				log.Infof("frontend: [%s] closing listener [%s]", td.ServiceName, val.listener.Addr().String())
				val.listener.Close()
			}

			// Create the frontend...
			val, err = ts.startFrontend(td.ServiceName, &td.FrontendData)
			if err != nil {
				log.Errorf("frontend error starting: [%s]", err.Error())
				ts.mtx.Unlock()
				return
			}

			ts.frontendMap[td.ServiceName] = val
		}

		dataConnData := &backendRuntimeData{conn: conn}
		val.backendConnMap[td.ID] = dataConnData

		// Start reading from the backend and copying data.
		// Done here so we get when the connection closes even before a frontend connection is made.
		go func() {
			var frontConn net.Conn
			defer func() {
				conn.Close()
				if frontConn != nil {
					frontConn.Close()
				}
			}()

			b := make([]byte, 1024)
			n, err := conn.Read(b)

			ts.mtx.Lock()
			log.Debugf("frontend read(0) finished reading first portion: [%s]", td.ID)
			val.removeBackendConn(td.ID)
			ts.mtx.Unlock()

			// Send the first leg that I saved
			frontConn = dataConnData.frontendConn
			if frontConn == nil {
				log.Errorf("frontConn not set for ID: [%s]", td.ID)
				return
			}

			if err != nil {
				log.Errorf("proxy connection [%s] failed back -> front (bytes %d, error: [%v]", td.ID, err)
				return
			}

			ll, err := frontConn.Write((b[:n]))
			if ll != n || err != nil {
				log.Errorf("proxy connection [%s] failed back -> front (bytes %d, error: [%v]", td.ID, err)
				return
			}

			// keep copying data
			l, err := ts.copybytes(td.ServiceName, td.ID, frontConn, conn)
			log.Debugf("proxy connection [%s] finished back -> front (bytes %d, error: [%v]", td.ID, l, err)
		}()

		ts.mtx.Unlock()
		connp = nil

		log.Debugf("Tunnel ID: [%s] established: service: [%s], listen port: [%d]",
			td.ID, td.ServiceName, td.FrontendData.Port)
	}

}

func (ts *TunnelService) startFrontend(serviceName string, fed *FrontendData) (*frontendRuntimeData, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", fed.Port))
	if err != nil {
		log.Errorf("frontend [%s] on port [%d] listener error: [%s]", serviceName, fed.Port, err.Error())
		return nil, err
	}

	log.Infof("frontend [%s] listening on: %s", serviceName, listener.Addr().String())

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Errorf("frontend [%s] on port [%d] listener error: [%s]", serviceName, fed.Port, err.Error())
				return
			}

			go ts.doProxy(serviceName, conn)
		}
	}()

	frontendRuntime := frontendRuntimeData{
		serviceName:    serviceName,
		listener:       listener,
		port:           fed.Port,
		backendConnMap: make(map[string]*backendRuntimeData),
	}

	return &frontendRuntime, err
}

func (ts *TunnelService) doProxy(serviceName string, conn net.Conn) {
	log.Debugf("frontend [%s:%s from %s] new connection", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())

	connp := &conn
	defer func() {
		ts.mtx.Unlock()
		if connp != nil {
			(*connp).Close()
			log.Debugf("frontend [%s:%s from %s] connection closed", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())
		}
	}()

	// pick a backend connection to proxy everything to
	ts.mtx.Lock()
	if frontend, ok := ts.frontendMap[serviceName]; ok {
		var backConnData *backendRuntimeData
		//var backConn net.Conn
		var id string

		for id, backConnData = range frontend.backendConnMap {
			break
		}

		if id != "" {
			// Wait until there is traffic on the channel to establish the backend connection
			// this is needed to handle tcp probes that will churn the end-to-end otherwise...
			//conn.SetReadDeadline(time.Now().Add(5000 * time.Millisecond))
			ts.mtx.Unlock()
			b := make([]byte, 1024)
			n, err := conn.Read(b)
			if err != nil {
				ts.mtx.Lock()
				return
			}

			//conn.SetReadDeadline(time.Time{})

			// "connect" the backend first, then start relaying IO.
			backConn := backConnData.conn
			err = ts.connectBackend(backConn, conn)
			if err != nil {
				ts.mtx.Lock()
				backConn.Close()
				frontend.removeBackendConn(id)
				return
			}

			ts.mtx.Lock()

			// Connection is already used, take it out.
			frontend.removeBackendConn(id)
			backConnData.frontendConn = conn

			// Send the first leg that I saved
			l, err := backConn.Write((b[:n]))
			if l != n || err != nil {
				backConn.Close()
				return
			}

			connp = nil

			go func() {
				l, err := ts.copybytes(serviceName, id, backConn, conn)
				log.Debugf("proxy connection [%s] finished front -> back (bytes %d, error: [%v]", id, l, err)
			}()

			//go func() {
			//	l, err := ts.copybytes(serviceName, id, conn, backConn)
			//	log.Debugf("proxy connection [%s] finished back -> front (bytes %d, error: [%v]", id, l, err)
			//}()
		} else {
			log.Errorf("proxy connection [%s] couldn't find any backend endpoint to proxy to", serviceName)
		}
	} else {
		// didn't find a mapping, close the connection
		log.Errorf("frontend [%s:%s from %s] couldn't find connection map service", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())
	}
}

func (ts *TunnelService) copybytes(serviceName string, id string, dst net.Conn, src net.Conn) (written int64, err error) {
	written, err = io.Copy(dst, src)
	dst.Close()
	src.Close()

	return written, err
}

func (ts *TunnelService) connectBackend(backendConn net.Conn, conn net.Conn) error {

	tunnelConnect := TunnelConnecData{
		SourceAddress: conn.RemoteAddr().String(),
	}
	err := sendSerializedObject(backendConn, tunnelConnect)
	if err != nil {
		return err
	}

	return nil
}
