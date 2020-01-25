package tunnel

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
)

type muxfrontendRuntimeData struct {
	serviceName    string
	port           int
	listener       net.Listener
	backendConnMap map[string]*yamux.Session
}

type MuxTunnelService struct {
	port  int
	token string

	frontendPortPool *PoolInts
	frontendMap      map[string]*muxfrontendRuntimeData
	mtx              sync.Mutex
	closeCh          chan bool
}

type TunnelFrontendServices struct {
	Name    string
	Address string
	Port    int
}

// NewMuxTunnelService creates a new tunnel service on the port using the passed cert
func NewMuxTunnelService(cert tls.Certificate, servicePort int, token string, dynport int, dymportcount int) (*MuxTunnelService, error) {
	ts := MuxTunnelService{
		port:        servicePort,
		token:       token,
		frontendMap: make(map[string]*muxfrontendRuntimeData),
		closeCh:     make(chan bool),
	}

	ts.frontendPortPool = NewPoolForRange(dynport, dymportcount)

	tlsconfig := &tls.Config{Certificates: []tls.Certificate{cert}}
	listener, err := tls.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", ts.port), tlsconfig)
	if err != nil {
		log.Fatal("tunnel service listener error:", err)
	}

	log.Infof("tunnel service listening on: tcp => %s", listener.Addr().String())

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Info("tunnel service  accept error", err)
				break
			}

			go ts.handleMuxConnection(conn)
		}
	}()

	return &ts, err
}

// Close closes the tunnel service
func (ts *MuxTunnelService) Close() {
	close(ts.closeCh)
}

func (ts *MuxTunnelService) GetServices() map[string]TunnelFrontendServices {
	srvs := make(map[string]TunnelFrontendServices)
	ts.mtx.Lock()

	for _, val := range ts.frontendMap {
		srvs[val.serviceName] = TunnelFrontendServices{
			Name:    val.serviceName,
			Port:    val.port,
			Address: "",
		}
	}

	ts.mtx.Unlock()

	return srvs
}

func (ts *MuxTunnelService) handleMuxConnection(conn net.Conn) {
	log.Infof("MuxTunnelService: new connection from: %s", conn.RemoteAddr().String())

	defer conn.Close()

	// Setup server side of yamux
	conf := yamux.DefaultConfig()
	conf.KeepAliveInterval = 60 * time.Second
	conf.ConnectionWriteTimeout = 60 * time.Second

	session, err := yamux.Server(conn, conf)
	if err != nil {
		log.Info("MuxTunnelService mux server error: ", err)
		return
	}

	defer session.Close()

	// Accept the control/config stream
	stream, err := session.Accept()
	if err != nil {
		log.Info("MuxTunnelService session accept error", err)
		return
	}

	defer session.Close()
	ts.handleStream(session, stream)

	log.Infof("MuxTunnelService: closed connection from: %s", conn.RemoteAddr().String())
}

func (ts *MuxTunnelService) handleStream(session *yamux.Session, conn net.Conn) {
	log.Infof("session: [%s], new stream connection from: %s", session.RemoteAddr().String(), conn.RemoteAddr().String())

	defer func() {
		conn.Close()
		log.Infof("stream connection from [%s] ended", conn.RemoteAddr().String())
	}()

	in := json.NewDecoder(conn)
	out := json.NewEncoder(conn)

	var td TunnelData
	err := in.Decode(&td)
	if err != nil {
		log.Errorf("failed to deserialize tunnel info [%s]", err.Error())
		return
	}

	if td.Token != ts.token {
		err := fmt.Errorf("stream: [%s] authorization failed, wrong token value", td.ServiceName)
		log.Error(err.Error())

		ts.sendResponse(&td, out, err)
		return
	}

	// Everything is alright, start the frontend if needed
	ts.mtx.Lock()
	var val *muxfrontendRuntimeData
	val, _ = ts.frontendMap[td.ServiceName]

	// check dynamic port condition
	if val != nil {
		if td.FrontendData.Port == 0 {
			td.FrontendData.Port = val.port
		}
	}

	if td.FrontendData.Port == 0 {
		port, err := ts.frontendPortPool.GetElement()
		if err != nil {
			ts.mtx.Unlock()
			err := fmt.Errorf("stream: [%s] out of ports for automatic frontend allocation", td.ServiceName)
			log.Error(err.Error())

			ts.sendResponse(&td, out, err)
			return
		}
		td.FrontendData.Port = port
		td.FrontendData.auto = true
	}

	if val == nil || val.port != td.FrontendData.Port {
		if val != nil && val.port != td.FrontendData.Port {
			log.Infof("stream: [%s] closing listener [%s]", td.ServiceName, val.listener.Addr().String())
			val.listener.Close()
		}

		// Create the frontend...
		val, err = ts.startFrontend(td.ServiceName, &td.FrontendData)
		if err != nil {
			if td.FrontendData.auto {
				ts.frontendPortPool.ReturnElement(td.FrontendData.Port)
			}
			log.Errorf("stream [%s] error starting: [%s]", td.ServiceName, err.Error())
			ts.mtx.Unlock()
			return
		}

		ts.frontendMap[td.ServiceName] = val
	}

	val.backendConnMap[session.RemoteAddr().String()] = session
	ts.mtx.Unlock()

	// Send the response so the client will get the endpoint info
	err = ts.sendResponse(&td, out, nil)
	if err != nil {
		log.Errorf("failed to send tunnel response info [%s]", err.Error())
		return
	}

	// check for closed session (closed/keepalive failed/etc) or shutdown
loop:
	for {
		select {
		case <-time.After(10 * time.Second):
			if session.IsClosed() {
				break loop
			}
		case <-ts.closeCh:
			break loop
		}
	}

	ts.mtx.Lock()
	delete(val.backendConnMap, session.RemoteAddr().String())

	if len(val.backendConnMap) == 0 {
		val.listener.Close()
		delete(ts.frontendMap, td.ServiceName)
	}
	ts.mtx.Unlock()

	log.Debugf("session: [%s] finished: service: [%s], listen port: [%d]",
		session.RemoteAddr().String(), td.ServiceName, td.FrontendData.Port)
}

func (ts *MuxTunnelService) sendResponse(td *TunnelData, out *json.Encoder, operror error) error {
	tdr := TunnelDataResponse{
		ID:              td.ID,
		ServiceName:     td.ServiceName,
		FrontendPort:    td.FrontendData.Port,
		FrontendAddress: "",
	}

	if operror != nil {
		tdr.Error = operror.Error()
	}
	err := out.Encode(&tdr)
	return err
}

func (ts *MuxTunnelService) startFrontend(serviceName string, fed *FrontendData) (*muxfrontendRuntimeData, error) {
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
				break
			}

			go ts.doProxy(serviceName, conn)
		}

		if fed.auto {
			ts.frontendPortPool.ReturnElement(fed.Port)
		}
	}()

	frontendRuntime := muxfrontendRuntimeData{
		serviceName:    serviceName,
		listener:       listener,
		port:           fed.Port,
		backendConnMap: make(map[string]*yamux.Session),
	}

	return &frontendRuntime, err
}

func (ts *MuxTunnelService) doProxy(serviceName string, conn net.Conn) {
	log.Debugf("frontend [%s:%s from %s] new connection", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())

	defer func() {
		ts.mtx.Unlock()
		conn.Close()
		log.Debugf("frontend [%s:%s from %s] connection closed", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())

	}()

	// pick a backend connection to proxy everything to there
	ts.mtx.Lock()
	frontend, _ := ts.frontendMap[serviceName]
	if frontend == nil {
		// didn't find a mapping, close the connection
		log.Errorf("frontend [%s:%s from %s] couldn't find any frontend connection definition", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())
		return
	}

	var session *yamux.Session
	var id string

	for id, session = range frontend.backendConnMap {
		break
	}

	if id == "" {
		log.Errorf("frontend [%s:%s from %s] couldn't find any backend endpoint to proxy to", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())
		return
	}

	ts.mtx.Unlock()

	// Wait until there is traffic on the channel to establish the backend connection
	// this is needed to handle tcp probes that will churn the end-to-end otherwise...
	b := make([]byte, 1024)
	n, err := conn.Read(b)
	if err != nil {
		ts.mtx.Lock()
		return
	}

	// Connect the service to relay to
	backConn, err := ts.connectBackend(session, conn, serviceName)
	if err != nil {

	}

	// Send the first leg that I saved
	l, err := backConn.Write((b[:n]))
	if l != n || err != nil {
		backConn.Close()
		return
	}

	go func() {
		l, err := ts.copybytes(serviceName, id, backConn, conn)
		log.Debugf("proxy connection [%s] finished front -> back (bytes %d, error: [%v]", id, l, err)
	}()

	{
		l, err := ts.copybytes(serviceName, id, conn, backConn)
		log.Debugf("proxy connection [%s] finished back -> front (bytes %d, error: [%v]", id, l, err)
	}

	ts.mtx.Lock()
}

func (ts *MuxTunnelService) copybytes(serviceName string, id string, dst net.Conn, src net.Conn) (written int64, err error) {
	written, err = io.Copy(dst, src)
	dst.Close()
	src.Close()

	return written, err
}

func (ts *MuxTunnelService) connectBackend(session *yamux.Session, conn net.Conn, serviceName string) (net.Conn, error) {
	// Open a new stream in the backend
	backConn, err := session.Open()
	if err != nil {
		return nil, err
	}

	tunnelConnect := TunnelConnecData{
		ServiceName:   serviceName,
		SourceAddress: session.RemoteAddr().String(),
	}
	err = sendSerializedObject(backConn, tunnelConnect)
	if err != nil {
		return nil, err
	}

	return backConn, nil
}
