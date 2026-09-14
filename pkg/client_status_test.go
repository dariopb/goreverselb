package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
)

func awaitClientStatus(t *testing.T, client *MuxTunnelClient, match func(ClientStatus) bool) ClientStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := client.Status()
		if match(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("client status did not reach expected state: %+v", client.Status())
	return ClientStatus{}
}

func TestClientStatusRegistrationReconnectAndClose(t *testing.T) {
	const token = "status-test-token"
	hook := captureClientLogs(t, token, log.DebugLevel)
	cert, roots := clientTestCertificate(t)
	firstAck, nextAck := make(chan struct{}), make(chan struct{})
	var ackOnce, nextOnce sync.Once
	defer ackOnce.Do(func() { close(firstAck) })
	defer nextOnce.Do(func() { close(nextAck) })
	var attempts atomic.Int32
	sessions := make(chan *yamux.Session, 8)
	endpoint, _ := mockClientControl(t, cert, func(session *yamux.Session, control net.Conn, td TunnelData) error {
		attempt := attempts.Add(1)
		sessions <- session
		if attempt == 2 {
			return json.NewEncoder(control).Encode(TunnelDataResponse{Error: "denied " + token})
		}
		ack := firstAck
		if attempt > 2 {
			ack = nextAck
		}
		select {
		case <-ack:
		case <-session.CloseChan():
			return nil
		}
		return json.NewEncoder(control).Encode(TunnelDataResponse{
			ServiceName: td.ServiceName, FrontendPort: 7445 + int(attempt-1),
			FrontendAddress: "front.example.test", PublicationMode: "dynamic",
		})
	})
	client, err := NewMuxTunnelClientWithOptions(endpoint, TunnelData{
		ServiceName: "example", Token: token, FrontendData: FrontendData{Port: 7000},
	}, ClientOptions{
		TLSConfig:      &tls.Config{RootCAs: roots, ServerName: "tunnel.test"},
		ReconnectDelay: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	status := awaitClientStatus(t, client, func(s ClientStatus) bool { return s.State == ClientStateRegistering })
	if status.ConnectedConnections != 1 || status.ReadyConnections != 0 ||
		status.FrontendPort != 0 || status.FrontendAddress != "" || client.FrontendPort() != 7000 {
		t.Fatalf("requested port was mistaken for an accepted registration: %+v", status)
	}
	if status.Endpoint != endpoint || status.ServiceName != "example" || status.DesiredConnections != 1 {
		t.Fatalf("missing client identity: %+v", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	waiting, err := client.WaitReady(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || waiting.State != ClientStateRegistering {
		t.Fatalf("registration wait did not respect deadline: %+v %v", waiting, err)
	}
	const waiters = 4
	waitResults := make(chan error, waiters)
	for range waiters {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := client.WaitReady(ctx)
			waitResults <- err
		}()
	}
	ackOnce.Do(func() { close(firstAck) })
	for range waiters {
		if err := <-waitResults; err != nil {
			t.Fatal(err)
		}
	}
	status = client.Status()
	if status.State != ClientStateReady || status.ReadyConnections != 1 || status.FrontendPort != 7445 ||
		status.FrontendAddress != "front.example.test:7445" || status.PublicationMode != "dynamic" || status.LastError != "" {
		t.Fatalf("wrong accepted frontend status: %+v", status)
	}
	status.Connections[0].State = ClientStateClosed
	if client.Status().Connections[0].State != ClientStateReady {
		t.Fatal("Status returned mutable internal state")
	}
	first := <-sessions
	first.Close()
	awaitClientStatus(t, client, func(s ClientStatus) bool {
		return s.State == ClientStateReconnecting && s.ReadyConnections == 0 &&
			s.FrontendPort == 0 && s.FrontendAddress == ""
	})
	status = awaitClientStatus(t, client, func(s ClientStatus) bool {
		return s.State == ClientStateReconnecting && strings.Contains(s.LastError, "denied")
	})
	if strings.Contains(status.LastError, token) || !strings.Contains(status.LastError, "[redacted]") ||
		status.LastErrorAt.IsZero() || status.Connections[0].LastErrorAt.IsZero() {
		t.Fatal("status leaked the token or lost failure details")
	}
	nextOnce.Do(func() { close(nextAck) })
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	status, err = client.WaitReady(ctx)
	cancel()
	if err != nil || status.FrontendPort != 7447 || status.LastError != "" ||
		status.FrontendAddress != "front.example.test:7447" {
		t.Fatalf("recovery did not publish the new server response or clear the error: %+v %v", status, err)
	}
	client.Close()
	status, err = client.WaitReady(context.Background())
	if !errors.Is(err, net.ErrClosed) || status.State != ClientStateClosed ||
		status.ConnectedConnections != 0 || status.FrontendPort != 0 {
		t.Fatalf("closed client remained ready: %+v %v", status, err)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.found {
		t.Fatal("status-related error logging leaked the token")
	}
}

func TestClientStatusBacklogAndBindingsOnly(t *testing.T) {
	cert, roots := clientTestCertificate(t)
	sessions := make(chan *yamux.Session, 4)
	endpoint, _ := mockClientControl(t, cert, func(session *yamux.Session, control net.Conn, td TunnelData) error {
		sessions <- session
		return json.NewEncoder(control).Encode(TunnelDataResponse{
			ServiceName: td.ServiceName, PublicationMode: "bindings_only",
		})
	})
	client, err := NewMuxTunnelClientWithOptions(endpoint, TunnelData{ServiceName: "example", BackendAcceptBacklog: 2},
		ClientOptions{TLSConfig: &tls.Config{RootCAs: roots, ServerName: "tunnel.test"}, ReconnectDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	awaitClientStatus(t, client, func(s ClientStatus) bool { return s.ReadyConnections == 2 })
	status, err := client.WaitReady(context.Background())
	if err != nil || status.State != ClientStateReady || status.FrontendPort != 0 ||
		status.FrontendAddress != "" || status.PublicationMode != "bindings_only" {
		t.Fatalf("bindings-only readiness depends on a nonzero port: %+v %v", status, err)
	}
	first, second := <-sessions, <-sessions
	first.Close()
	status = awaitClientStatus(t, client, func(s ClientStatus) bool { return s.ReadyConnections == 1 })
	if status.State != ClientStateReady || status.ConnectedConnections != 1 ||
		status.DesiredConnections != 2 || status.LastError == "" {
		t.Fatalf("partial disconnect hid the remaining ready session: %+v", status)
	}
	second.Close()
	awaitClientStatus(t, client, func(s ClientStatus) bool { return s.State == ClientStateReconnecting })
	done := make(chan error, 1)
	go func() { _, err := client.WaitReady(context.Background()); done <- err }()
	client.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake WaitReady")
	}
}

func TestClientStatusConnectingAndCancelledWait(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := NewMuxTunnelClient(listener.Addr().String(), TunnelData{ServiceName: "example"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if status := client.Status(); status.State != ClientStateConnecting || status.ConnectedConnections != 0 {
		t.Fatalf("TCP accept alone was marked TLS-connected: %+v", status)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.WaitReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled readiness wait returned %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				_ = client.Status()
				client.UpdateTargetPort(8080)
				client.UpdateTargetAddresses([]string{"127.0.0.1"})
			}
			client.Close()
		}()
	}
	wg.Wait()
	if status := client.Status(); status.State != ClientStateClosed {
		t.Fatalf("shutdown did not become terminal: %+v", status)
	}
}
