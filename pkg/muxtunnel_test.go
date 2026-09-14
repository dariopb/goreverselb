package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestStandaloneClientHalfCloseAndPortReuse(t *testing.T) {
	cert, _ := clientTestCertificate(t)
	port := availablePortRange(t, 1)
	service := newSSHReverseTestService(t, port, 1)
	control, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	service.tunnelListener = control
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				service.handleMuxConnection(conn)
			}()
		}
	}()
	t.Cleanup(func() { service.Close(); workers.Wait() })
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	result := make(chan error, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := backend.Accept()
			if err != nil {
				result <- err
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			request, err := io.ReadAll(conn)
			if err != nil || string(request) != "request" {
				result <- fmt.Errorf("backend read %q: %v", request, err)
				conn.Close()
				return
			}
			time.Sleep(20 * time.Millisecond)
			_, err = conn.Write([]byte("response"))
			conn.Close()
			result <- err
		}
	}()
	for i := 0; i < 2; i++ {
		tc, err := NewMuxTunnelClient(control.Addr().String(), TunnelData{
			ServiceName: "service:instance", Token: "test-token",
			TargetAddresses: []string{"127.0.0.1"}, TargetPort: backend.Addr().(*net.TCPAddr).Port,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tc.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		status, err := tc.WaitReady(ctx)
		cancel()
		if err != nil || status.FrontendPort != port ||
			status.FrontendAddress != fmt.Sprintf("127.0.0.1:%d", port) || status.PublicationMode != "" {
			t.Fatalf("standalone readiness/address fallback: %+v %v", status, err)
		}
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		request := append([]byte(ProxyString), byte(len("instance")))
		request = append(request, []byte("instancerequest")...)
		if _, err := conn.Write(request); err != nil {
			t.Fatal(err)
		}
		conn.(*net.TCPConn).CloseWrite()
		response, err := io.ReadAll(conn)
		conn.Close()
		if err != nil || string(response) != "response" {
			t.Fatalf("standalone response %q: %v", response, err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		tc.Close()
		waitForTunnel(t, func() bool {
			if len(service.GetServices(DefaultUserID)) != 0 {
				return false
			}
			service.frontendPortPool.mtx.Lock()
			defer service.frontendPortPool.mtx.Unlock()
			return len(service.frontendPortPool.free) == 1
		})
	}
}

func waitForTunnel(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for tunnel state")
}
