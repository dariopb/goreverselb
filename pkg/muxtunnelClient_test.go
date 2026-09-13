package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
)

func clientTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tunnel.test"},
		DNSNames: []string{"tunnel.test"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func mockClientControl(t *testing.T, cert tls.Certificate, handler func(*yamux.Session, net.Conn, TunnelData) error) (string, <-chan error) {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				if err := conn.(*tls.Conn).HandshakeContext(ctx); err != nil {
					result <- err
					return
				}
				session, err := yamux.Server(conn, nil)
				if err != nil {
					result <- err
					return
				}
				defer session.Close()
				stream, err := session.Accept()
				if err != nil {
					result <- err
					return
				}
				var td TunnelData
				if err := json.NewDecoder(stream).Decode(&td); err != nil {
					result <- err
					return
				}
				_ = conn.SetDeadline(time.Time{})
				if handler != nil {
					if err := handler(session, stream, td); err != nil {
						result <- err
						return
					}
				} else {
					if err := json.NewEncoder(stream).Encode(TunnelDataResponse{ServiceName: td.ServiceName}); err != nil {
						result <- err
						return
					}
				}
				result <- nil
				<-session.CloseChan()
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		wg.Wait()
	})
	return listener.Addr().String(), result
}

func TestClientTLSVerification(t *testing.T) {
	cert, roots := clientTestCertificate(t)
	tests := []struct {
		name    string
		options ClientOptions
		success bool
	}{
		{"verified custom roots", ClientOptions{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "tunnel.test"}}, true},
		{"untrusted certificate", ClientOptions{TLSConfig: &tls.Config{ServerName: "tunnel.test"}}, false},
		{"wrong hostname", ClientOptions{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "wrong.test"}}, false},
		{"endpoint hostname default", ClientOptions{TLSConfig: &tls.Config{RootCAs: roots}}, false},
		{"explicit compatibility", LegacyClientOptions(), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, result := mockClientControl(t, cert, nil)
			client, err := NewMuxTunnelClientWithOptions(address, TunnelData{ServiceName: "test"}, test.options)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			select {
			case err := <-result:
				if (err == nil) != test.success {
					t.Fatalf("TLS success=%v, error=%v", test.success, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("TLS attempt stalled")
			}
		})
	}
}

func TestClientRegistryEndToEndHalfClose(t *testing.T) {
	hook := captureClientLogs(t, "test-only-token", log.DebugLevel)
	cert, roots := clientTestCertificate(t)
	registry := tunnelcore.NewRegistry(tunnelcore.Options{})
	defer registry.Close()
	selector := tunnelcore.Selector{UserID: "user", Service: "echo", Instance: "instance"}
	address, ready := mockClientControl(t, cert, func(session *yamux.Session, control net.Conn, td TunnelData) error {
		if td.ServiceName != "echo:instance" || td.Token != "test-only-token" {
			return fmt.Errorf("incorrect registration fields")
		}
		if err := registry.Register("session", selector, session); err != nil {
			return err
		}
		return json.NewEncoder(control).Encode(TunnelDataResponse{ServiceName: td.ServiceName, PublicationMode: "bindings_only"})
	})
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	backendResult := make(chan error, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			backendResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		request, err := io.ReadAll(conn)
		if err != nil || string(request) != "request" {
			backendResult <- fmt.Errorf("backend request %q: %v", request, err)
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, err = conn.Write([]byte("response after EOF"))
		backendResult <- err
	}()
	targets := []string{"127.0.0.1"}
	tc, err := NewMuxTunnelClientWithOptions(address, TunnelData{
		ServiceName: "echo:instance", Token: "test-only-token", TargetAddresses: targets,
		TargetPort: backend.Addr().(*net.TCPAddr).Port,
	}, ClientOptions{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "tunnel.test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	targets[0] = "not-the-target"
	copy := tc.TargetAddresses()
	copy[0] = "also-not-the-target"
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registration stalled")
	}
	conn, err := registry.DialContext(context.Background(), selector, tunnelcore.ConnectionInfo{SourceAddress: "127.0.0.1:1234"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	conn.(interface{ CloseWrite() error }).CloseWrite()
	response, err := io.ReadAll(conn)
	if err != nil || string(response) != "response after EOF" {
		t.Fatalf("round trip: %q %v", response, err)
	}
	if err := <-backendResult; err != nil {
		t.Fatal(err)
	}
	registry.Remove("session")
	done := make(chan struct{})
	go func() { tc.Close(); tc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client shutdown stalled")
	}
	assertClientDebugLogs(t, hook, "Accepted tunnel stream", "Opening backend connection",
		"Backend connection established", "Tunnel copy finished", "Tunnel stream finished")
	hook.mu.Lock()
	defer hook.mu.Unlock()
	accepted := hook.debugEntries["Accepted tunnel stream"]
	if len(accepted) != 1 || accepted[0]["source_address"] != "127.0.0.1:1234" {
		t.Fatal("original source IP and port missing from stream diagnostics")
	}
	for _, field := range []string{"tunnel_local", "tunnel_remote", "stream_id", "frontend_port", "frontend_address"} {
		if _, ok := accepted[0][field]; !ok {
			t.Errorf("missing stream hop field %s", field)
		}
	}
	connected := hook.debugEntries["Backend connection established"]
	if len(connected) != 1 || connected[0]["backend_remote"] != backend.Addr().String() {
		t.Fatal("selected backend IP and port missing from diagnostics")
	}
	for _, field := range []string{"backend", "backend_local", "source_address", "stream_id"} {
		if _, ok := connected[0][field]; !ok {
			t.Errorf("missing backend hop field %s", field)
		}
	}
	want := map[string]int64{"tunnel_to_backend": int64(len("request")), "backend_to_tunnel": int64(len("response after EOF"))}
	for _, fields := range hook.debugEntries["Tunnel copy finished"] {
		direction, _ := fields["direction"].(string)
		if count, ok := want[direction]; !ok || fields["bytes"] != count {
			t.Errorf("incorrect byte count for %s: %v", direction, fields["bytes"])
		}
		delete(want, direction)
	}
	if len(want) != 0 {
		t.Error("missing directional transfer diagnostics")
	}
}

func TestClientCloseInterruptsHandshakeAndRegistration(t *testing.T) {
	for _, stage := range []string{"handshake", "registration"} {
		t.Run(stage, func(t *testing.T) {
			cert, _ := clientTestCertificate(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			ready := make(chan struct{})
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				accepted <- conn
				if stage == "registration" {
					tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
					session, err := yamux.Server(tlsConn, nil)
					if err == nil {
						defer session.Close()
						stream, err := session.Accept()
						if err == nil {
							var td TunnelData
							_ = json.NewDecoder(stream).Decode(&td)
						}
						close(ready)
						<-session.CloseChan()
						return
					}
				}
				close(ready)
			}()
			tc, err := NewMuxTunnelClient(listener.Addr().String(), TunnelData{})
			if err != nil {
				t.Fatal(err)
			}
			defer tc.Close()
			select {
			case conn := <-accepted:
				defer conn.Close()
			case <-time.After(time.Second):
				t.Fatal("client failed to connect")
			}
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("did not reach blocked stage")
			}
			done := make(chan struct{})
			go func() {
				var wg sync.WaitGroup
				for i := 0; i < 5; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); tc.Close() }()
				}
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("concurrent Close failed to cancel blocked client")
			}
		})
	}
}

func TestClientTargetsAndServiceGroupLifecycle(t *testing.T) {
	cert, roots := clientTestCertificate(t)
	endpoint, _ := mockClientControl(t, cert, nil)
	options := ClientOptions{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "tunnel.test"}}
	group, err := NewMuxTunnelClientServiceGroupWithOptions(endpoint, "test-only-token", options)
	if err != nil {
		t.Fatal(err)
	}

	defer group.Close()
	service := &ServiceInfo{Name: "service", Ports: []PortData{{Port: 8000, Protocol: "tcp", TargetPort: 80}}, BackendIPs: []string{"127.0.0.1"}}
	if err := group.ReconcileServiceGroup(map[string]*ServiceInfo{"service": service}); err != nil {
		t.Fatal(err)
	}
	tc := group.tunnels["service"]["tcp-8000"]
	if tc.tlsconfig.InsecureSkipVerify || tc.tlsconfig.ServerName != "tunnel.test" {
		t.Fatal("group lost verified TLS configuration")
	}
	service.BackendIPs[0] = "127.0.0.2"
	service.Ports[0].TargetPort = 81
	if err := group.ReconcileServiceGroup(map[string]*ServiceInfo{"service": service}); err != nil {
		t.Fatal(err)
	}
	if tc.TargetAddresses()[0] != "127.0.0.2" || tc.TargetPort() != 81 {
		t.Fatal("reconciliation failed to update existing targets")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				tc.UpdateTargetPort(80 + j)
				tc.UpdateTargetAddresses([]string{"127.0.0." + strconv.Itoa(j+1)})
				_ = tc.TargetAddresses()
				_ = tc.TargetPort()
				_ = tc.FrontendPort()
			}
		}()
	}
	wg.Wait()
	group.Close()
	group.Close()
	if err := group.ReconcileServiceGroup(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed group accepted reconciliation: %v", err)
	}
}
