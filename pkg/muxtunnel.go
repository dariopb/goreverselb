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

type muxfrontendRuntimeData struct {
	serviceName    string
	instanceName   string
	port           int
	listener       net.Listener
	backendConnMap map[string]map[string]*yamux.Session
}

type MuxTunnelService struct {
	port       int
	token      string
	configData *ConfigData

	frontendPortPool *PoolInts
	frontendMap      map[string]map[string]*muxfrontendRuntimeData
	cert             tls.Certificate
	mtx              sync.Mutex
	closeCh          chan bool
}

type TunnelFrontendServices struct {
	Name    string
	Address string
	Port    int
}

// NewMuxTunnelService creates a new tunnel service on the port using the passed cert
func NewMuxTunnelService(configData *ConfigData, cert tls.Certificate, servicePort int, token string, dynport int, dymportcount int) (*MuxTunnelService, error) {
	rand.Seed(time.Now().UnixNano())

	ts := MuxTunnelService{
		port:        servicePort,
		token:       token,
		configData:  configData,
		frontendMap: make(map[string]map[string]*muxfrontendRuntimeData),
		closeCh:     make(chan bool),
		cert:        cert,
	}

	// create the frontendMap for the defined users
	for _, u := range configData.Users {
		ts.frontendMap[u.UserID] = make(map[string]*muxfrontendRuntimeData)
	}

	ts.frontendPortPool = NewPoolForRange(dynport, dymportcount)

	tlsconfig := &tls.Config{Certificates: []tls.Certificate{ts.cert}}
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

func (ts *MuxTunnelService) GetServices(userID string) map[string]TunnelFrontendServices {
	srvs := make(map[string]TunnelFrontendServices)
	ts.mtx.Lock()

	for _, val := range ts.frontendMap[userID] {
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
	l := log.WithFields(log.Fields{
		"session": session.RemoteAddr().String(),
		"remote":  conn.RemoteAddr().String(),
	})

	l.Debugf("new stream connection")

	defer func() {
		conn.Close()
		l.Debug("connection ended")
	}()

	in := json.NewDecoder(conn)
	out := json.NewEncoder(conn)

	var td TunnelData
	err := in.Decode(&td)
	if err != nil {
		l.Errorf("failed to deserialize tunnel info [%s]", err.Error())
		return
	}

	userID, serviceName, instanceName := ts.parseServiceContext(&td)
	if !ts.authorize(userID, serviceName, &td) {
		err := fmt.Errorf("stream: [%s] authorization failed, token presented is not valid", td.ServiceName)
		l.Error(err.Error())

		ts.sendResponse(&td, out, err)
		return
	}

	l = l.WithField("service", serviceName)

	// Everything is alright, start the frontend if needed
	ts.mtx.Lock()
	var val *muxfrontendRuntimeData
	val, _ = ts.frontendMap[userID][serviceName]

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
			l.Error(err.Error())

			ts.sendResponse(&td, out, err)
			return
		}
		td.FrontendData.Port = port
		td.FrontendData.auto = true
	}

	if val == nil || val.port != td.FrontendData.Port {
		if val != nil && val.port != td.FrontendData.Port {
			l.Infof("service: [%s] closing listener [%s]", serviceName, val.listener.Addr().String())
			val.listener.Close()
		}

		// Create the frontend...
		val, err = ts.startFrontend(userID, serviceName, instanceName, &td)
		if err != nil {
			ts.frontendPortPool.ReturnElement(td.FrontendData.Port)

			l.Errorf("service [%s] error starting: [%s]", serviceName, err.Error())
			ts.mtx.Unlock()
			return
		}

		ts.frontendMap[userID][serviceName] = val
	}

	if _, ok := val.backendConnMap[instanceName]; !ok {
		val.backendConnMap[instanceName] = make(map[string]*yamux.Session)
	}

	val.backendConnMap[instanceName][session.RemoteAddr().String()] = session
	ts.mtx.Unlock()

	// Send the response so the client will get the endpoint info
	err = ts.sendResponse(&td, out, nil)
	if err != nil {
		l.Errorf("failed to send tunnel response info [%s]", err.Error())
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
		if fem, ok := ts.frontendMap[userID]; ok {
			delete(fem, serviceName)
		}
	}
	ts.mtx.Unlock()

	l.Debugf("finished: service: [%s:%s], listen port: [%d]",
		serviceName, instanceName, td.FrontendData.Port)
}

func (ts *MuxTunnelService) parseServiceContext(td *TunnelData) (userID, serviceName, instanceName string) {
	instanceName = ""
	parts := strings.Split(td.ServiceName, ":")
	if len(parts) > 1 {
		instanceName = parts[1]
	}
	serviceName = parts[0]

	userID = DefaultUserID
	parts = strings.Split(td.Token, ":")
	if len(parts) > 1 {
		userID = parts[0]
	}

	return userID, serviceName, instanceName
}

func (ts *MuxTunnelService) authorize(userID, serviceName string, td *TunnelData) bool {
	authorized := false

	ts.mtx.Lock()
	u, ok := ts.configData.Users[userID]
	if ok {
		s, ok := u.Services[serviceName]
		if ok {
			if td.Token == s.Token {
				authorized = true
			}
		} else {
			if td.Token == u.Token {
				authorized = true
			}
		}
	}

	ts.mtx.Unlock()

	return authorized
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

func (ts *MuxTunnelService) startFrontend(userID string, serviceName string, instanceName string, td *TunnelData) (*muxfrontendRuntimeData, error) {
	fed := &td.FrontendData

	l := log.WithFields(log.Fields{
		"frontend": serviceName,
		"port":     fed.Port,
	})

	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", fed.Port))
	if err != nil {
		l.Errorf("listener error: [%s]", err.Error())
		return nil, err
	}

	l.Infof("Started listening")

	go func() {
		for {

			conn, err := listener.Accept()
			if err != nil {
				l.Errorf("accept error: [%s]", err.Error())
				break
			}
			instancename := ""

			// If the service requested to always be wrapped over TLS (most likely HTTP traffic), do it.
			if fed.TLSWrap {
				l.Info("Upgrading frontend connection to TLS")
				tlsconfig := &tls.Config{Certificates: []tls.Certificate{ts.cert}}
				tlsconn := tls.Server(conn, tlsconfig)
				err = tlsconn.Handshake()
				if err != nil {
					l.Errorf("TLS handshake error: [%s]", err.Error())
					conn.Close()
					continue
				}

				instancename = tlsconn.ConnectionState().ServerName
				l.Infof("TLS wrap asks for instancename [%s]", instancename)
				conn = tlsconn
			}

			go ts.doProxy(userID, serviceName, instancename, conn)
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

func (ts *MuxTunnelService) doProxy(userID, serviceName string, instanceName string, conn net.Conn) {
	l := log.WithFields(log.Fields{
		"frontend": serviceName,
		"local":    conn.LocalAddr().String(),
		"remote":   conn.RemoteAddr().String(),
	})

	l.Debugf("new connection")

	defer func() {
		conn.Close()
	}()

	// Validate if a connection comes from the allowed range of IPs
	//ts.mtx.Lock()
	//u, ok := ts.configData.Users[userID]
	//if ok {
	//	s, ok := u.Services[serviceName]
	//	if ok {
	//	}
	//}
	//ts.mtx.Unlock()

	var b []byte
	var err error

	if instanceName == "" {
		instanceName, b, err = ts.getSNITarget(serviceName, conn)
		if err != nil {
			l.Debugf("getSNITarget failed: %v", err)
			return
		}
	}
	defer func() {
		conn.Close()
		l.Debug("connection closed")
	}()

	// pick a backend connection to proxy everything there
	ts.mtx.Lock()
	var frontend *muxfrontendRuntimeData = nil
	if fem, ok := ts.frontendMap[userID]; ok {
		frontend, _ = fem[serviceName]
	}
	if frontend == nil {
		ts.mtx.Unlock()
		l.Error("couldn't find any frontend connection definition")
		return
	}

	var session *yamux.Session
	var id string

	bcm, _ := frontend.backendConnMap[instanceName]
	if bcm == nil {
		ts.mtx.Unlock()
		l.Errorf("couldn't find any backend connection definition for instance: [%s]", instanceName)
		return
	}

	// Pick a random backend
	count := len(bcm)
	if count == 0 {
		ts.mtx.Unlock()
		l.Errorf("couldn't find any backend endpoint to proxy to for instance: %s", instanceName)
		return
	}

	i := rand.Intn(count)
	for id, session = range bcm {
		if i == 0 {
			break
		}
		i = i - 1
	}

	if id == "" {
		ts.mtx.Unlock()
		l.Errorf("couldn't find any backend endpoint to proxy to for instance: %s", instanceName)
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
		count, err = backConn.Write(b)
		if count != len(b) || err != nil {
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

// Try to decode a proxy payload in the form: "PROXY->LENinstancename<cr>"
func (ts *MuxTunnelService) tryProxyDecode(b []byte) (string, int) {
	s := cryptobyte.String(b)
	dummy := []byte{}

	if len(b) > 256 || !s.ReadBytes(&dummy, 7) || len(dummy) == 0 || string(dummy) != ProxyString {
		return "", 0
	}

	var lenConsumed uint8
	if !s.ReadUint8(&lenConsumed) {
		return "", 0
	}

	if !s.ReadBytes(&dummy, int(lenConsumed)) || len(dummy) == 0 {
		return "", 0
	}

	return string(dummy), int(lenConsumed) + len(ProxyString) + 1
}

// Try to decode an HTTP proxy payload
func (ts *MuxTunnelService) tryHTTPProxyDecode(b []byte, conn net.Conn) string {
	r := bufio.NewReader(bytes.NewReader(b))

	if connect, err := r.ReadString(' '); err != nil || connect != HttpConnectString {
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
