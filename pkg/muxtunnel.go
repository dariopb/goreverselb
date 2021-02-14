package tunnel

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/cryptobyte"

	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
)

const (
	proxyString     string = "PROXY->"
	httpProxyString string = "CONNECT "
)

type muxfrontendRuntimeData struct {
	serviceName    string
	instanceName   string
	port           int
	listener       net.Listener
	backendConnMap map[string]map[string]*yamux.Session
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
	rand.Seed(time.Now().UnixNano())

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
	log.Debugf("session: [%s], new stream connection from: %s", session.RemoteAddr().String(), conn.RemoteAddr().String())

	defer func() {
		conn.Close()
		log.Debugf("stream connection from [%s] ended", conn.RemoteAddr().String())
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
	instanceName := ""
	serviceParts := strings.Split(td.ServiceName, ":")
	if len(serviceParts) > 1 {
		instanceName = serviceParts[1]
	}
	serviceName := serviceParts[0]

	ts.mtx.Lock()
	var val *muxfrontendRuntimeData
	val, _ = ts.frontendMap[serviceName]

	// check dynamic port condition
	if val != nil {
		if td.FrontendData.Port == 0 {
			td.FrontendData.Port = val.port
		}
	}

	port, err := ts.frontendPortPool.GetElement(td.FrontendData.Port)
	if td.FrontendData.Port == 0 {
		if err != nil {
			ts.mtx.Unlock()
			err := fmt.Errorf("stream: [%s] out of ports for automatic frontend allocation", serviceName)
			log.Error(err.Error())

			ts.sendResponse(&td, out, err)
			return
		}
		td.FrontendData.Port = port
		td.FrontendData.auto = true
	}

	if val == nil || val.port != td.FrontendData.Port {
		if val != nil && val.port != td.FrontendData.Port {
			log.Infof("stream: [%s] closing listener [%s]", serviceName, val.listener.Addr().String())
			val.listener.Close()
		}

		// Create the frontend...
		val, err = ts.startFrontend(serviceName, instanceName, &td)
		if err != nil {
			ts.frontendPortPool.ReturnElement(td.FrontendData.Port)

			log.Errorf("stream [%s] error starting: [%s]", serviceName, err.Error())
			ts.mtx.Unlock()
			return
		}

		ts.frontendMap[serviceName] = val
	}

	if _, ok := val.backendConnMap[instanceName]; !ok {
		val.backendConnMap[instanceName] = make(map[string]*yamux.Session)
	}

	val.backendConnMap[instanceName][session.RemoteAddr().String()] = session
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
	delete(val.backendConnMap[instanceName], session.RemoteAddr().String())
	if len(val.backendConnMap[instanceName]) == 0 {
		delete(val.backendConnMap, instanceName)
	}

	if len(val.backendConnMap) == 0 {
		val.listener.Close()
		delete(ts.frontendMap, serviceName)
	}
	ts.mtx.Unlock()

	log.Debugf("session: [%s] finished: service: [%s:%s], listen port: [%d]",
		session.RemoteAddr().String(), serviceName, instanceName, td.FrontendData.Port)
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

func (ts *MuxTunnelService) startFrontend(serviceName string, instanceName string, td *TunnelData) (*muxfrontendRuntimeData, error) {
	fed := &td.FrontendData
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

		ts.frontendPortPool.ReturnElement(fed.Port)
	}()

	frontendRuntime := muxfrontendRuntimeData{
		serviceName:    serviceName,
		instanceName:   instanceName,
		listener:       listener,
		port:           fed.Port,
		backendConnMap: make(map[string]map[string]*yamux.Session),
	}

	return &frontendRuntime, err
}

func (ts *MuxTunnelService) doProxy(serviceName string, conn net.Conn) {
	log.Debugf("frontend [%s:%s from %s] new connection", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())

	defer func() {
		conn.Close()
	}()

	instanceName, b, err := ts.getSNITarget(serviceName, conn)
	if err != nil {
		log.Debugf("frontend [%s:%s from %s] connection closed: %v", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String(), err)
		return
	}

	defer func() {
		conn.Close()
		log.Debugf("frontend [%s:%s from %s] connection closed", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())
	}()

	// pick a backend connection to proxy everything there
	ts.mtx.Lock()
	frontend, _ := ts.frontendMap[serviceName]
	if frontend == nil {
		ts.mtx.Unlock()
		log.Errorf("frontend [%s:%s from %s] couldn't find any frontend connection definition", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String())
		return
	}

	var session *yamux.Session
	var id string

	bcm, _ := frontend.backendConnMap[instanceName]
	if bcm == nil {
		ts.mtx.Unlock()
		log.Errorf("frontend [%s:%s from %s] couldn't find any frontend connection definition for instance: [%s]", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String(), instanceName)
		return
	}

	// Pick a random backend
	l := len(bcm)
	if l == 0 {
		ts.mtx.Unlock()
		log.Errorf("frontend [%s:%s from %s] couldn't find any backend endpoint to proxy to for instance: %s", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String(), instanceName)
		return
	}

	i := rand.Intn(l)
	for id, session = range bcm {
		if i == 0 {
			break
		}
		i = i - 1
	}

	if id == "" {
		ts.mtx.Unlock()
		log.Errorf("frontend [%s:%s from %s] couldn't find any backend endpoint to proxy to for instance: %s", serviceName, conn.LocalAddr().String(), conn.RemoteAddr().String(), instanceName)
		return
	}
	ts.mtx.Unlock()

	// Connect to the backend endpoint
	backConn, err := ts.connectBackend(session, conn, serviceName, instanceName)
	if err != nil {
		return
	}

	// Send the first leg that I saved, if needed
	if b != nil {
		l, err = backConn.Write(b)
		if l != len(b) || err != nil {
			backConn.Close()
			return
		}
	}

	go func() {
		l, err := ts.copybytes(serviceName, id, backConn, conn)
		log.Debugf("proxy connection [%s] finished front -> back (bytes %d, error: [%v]", id, l, err)
	}()

	{
		l, err := ts.copybytes(serviceName, id, conn, backConn)
		log.Debugf("proxy connection [%s] finished back -> front (bytes %d, error: [%v]", id, l, err)
	}
}

// Read the first packet and try to identify if there is any sni type of redirection that could be used.
// If there is, then return the "instance" name for downstream lookup.
func (ts *MuxTunnelService) getSNITarget(serviceName string, conn net.Conn) (string, []byte, error) {
	// Wait until there is traffic on the channel to establish the backend connection
	// this is needed to handle tcp probes that will churn the end-to-end otherwise...
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Second)); err != nil {
		return "", nil, err
	}

	b := make([]byte, 1024)
	n, err := conn.Read(b)
	if err != nil {
		return "", nil, err
	}
	b = b[:n]

	// try TLS
	sn := ""
	if serverName := ts.tryTLSDecode(b); len(serverName) > 0 {
		sn = serverName
	} else if serverName, lenConsumed := ts.tryProxyDecode(b); len(serverName) > 0 {
		sn = serverName
		return sn, b[lenConsumed:], nil
	} else if serverName := ts.tryHTTPProxyDecode(b, conn); len(serverName) > 0 {
		sn = serverName
		return sn, nil, nil
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return "", nil, err
	}

	return sn, b, nil
}

// Copyright 2009 The Go Authors. All rights reserved.
// Extracted from go stdlib crypto\tls\handshake_messages.go
func (ts *MuxTunnelService) tryTLSDecode(b []byte) string {
	s := cryptobyte.String(b)
	dummy := cryptobyte.String{}

	if !s.Skip(5+6+32) || // handshake type and tls version, start of packet, 32 random
		!s.ReadUint8LengthPrefixed(&dummy) {
		return ""
	}

	// cipherSuites
	if !s.ReadUint16LengthPrefixed(&dummy) {
		return ""
	}

	// compressionMethods
	if !s.ReadUint8LengthPrefixed(&dummy) {
		return ""
	}

	// Extension data is optional...
	if s.Empty() {
		return ""
	}

	var extensions cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&extensions) {
		return ""
	}

	servername := ""

	for !extensions.Empty() {
		var extension uint16
		var extData cryptobyte.String
		if !extensions.ReadUint16(&extension) ||
			!extensions.ReadUint16LengthPrefixed(&extData) {
			return ""
		}

		if extension == 0x0 { //extensionServerName

			var nameList cryptobyte.String
			if !extData.ReadUint16LengthPrefixed(&nameList) || nameList.Empty() {
				return ""
			}
			for !nameList.Empty() {
				var nameType uint8
				var serverName cryptobyte.String
				if !nameList.ReadUint8(&nameType) ||
					!nameList.ReadUint16LengthPrefixed(&serverName) ||
					serverName.Empty() {
					return ""
				}
				if nameType != 0 {
					continue
				}
				if len(servername) != 0 {
					// Multiple names of the same name_type are prohibited.
					return ""
				}
				servername = string(serverName)
				// An SNI value may not include a trailing dot.
				if strings.HasSuffix(servername, ".") {
					return ""
				}
			}
		}
	}

	return servername
}

// Try to decode a proxy payload in the form: "PROXY destinationString<cr>"
func (ts *MuxTunnelService) tryProxyDecode(b []byte) (string, int) {
	s := cryptobyte.String(b)
	dummy := []byte{}

	if len(b) > 256 || !s.ReadBytes(&dummy, 7) || len(dummy) == 0 || string(dummy) != proxyString {
		return "", 0
	}

	var lenConsumed uint8
	if !s.ReadUint8(&lenConsumed) {
		return "", 0
	}

	if !s.ReadBytes(&dummy, int(lenConsumed)) || len(dummy) == 0 {
		return "", 0
	}

	return string(dummy), int(lenConsumed) + len(proxyString) + 1
}

// Try to decode an HTTP proxy payload
func (ts *MuxTunnelService) tryHTTPProxyDecode(b []byte, conn net.Conn) string {
	r := bufio.NewReader(bytes.NewReader(b))

	if connect, err := r.ReadString(' '); err != nil || connect != httpProxyString {
		return ""
	}

	target, err := r.ReadString(' ')
	if err != nil {
		return ""
	}

	_, err = conn.Write([]byte("HTTP/1.1 200 OK"))
	if err != nil {
		return ""
	}

	return target
}

func (ts *MuxTunnelService) copybytes(serviceName string, id string, dst net.Conn, src net.Conn) (written int64, err error) {
	written, err = io.Copy(dst, src)
	dst.Close()
	src.Close()

	return written, err
}

func (ts *MuxTunnelService) connectBackend(session *yamux.Session, conn net.Conn, serviceName string, instanceName string) (net.Conn, error) {
	// Open a new stream in the backend
	backConn, err := session.Open()
	if err != nil {
		return nil, err
	}

	if len(instanceName) > 0 {
		serviceName = serviceName + ":" + instanceName
	}

	tunnelConnect := TunnelConnecData{
		ServiceName:   serviceName,
		SourceAddress: conn.RemoteAddr().String(),
	}
	err = sendSerializedObject(backConn, tunnelConnect)
	if err != nil {
		return nil, err
	}

	return backConn, nil
}
