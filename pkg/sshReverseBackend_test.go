package tunnel

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func newSSHReverseTestService(t *testing.T, lowerPort, portCount int) *MuxTunnelService {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &MuxTunnelService{
		configData: &ConfigData{Users: map[string]*UserData{
			DefaultUserID: {UserID: DefaultUserID, Token: "test-token"},
		}},
		frontendPortPool:    NewPoolForRange(lowerPort, portCount),
		frontendMap:         map[string]map[string]*muxfrontendRuntimeData{DefaultUserID: {}},
		closeCh:             make(chan bool),
		sshSigner:           signer,
		sshBackendRawConns:  make(map[net.Conn]struct{}),
		sshBackendConns:     make(map[*gossh.ServerConn]struct{}),
		sshReverseFrontends: make(map[int]*sshReverseFrontend),
		cert:                tls.Certificate{},
	}
}

func availablePortRange(t *testing.T, count int) int {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		if port+count > 65535 {
			continue
		}
		listeners := make([]net.Listener, 0, count)
		available := true
		for candidate := port; candidate < port+count; candidate++ {
			candidateListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
			if err != nil {
				available = false
				break
			}
			listeners = append(listeners, candidateListener)
		}
		for _, candidateListener := range listeners {
			_ = candidateListener.Close()
		}
		if available {
			return port
		}
	}
	t.Fatal("could not reserve a contiguous test port range")
	return 0
}

func startSSHReverseTestServer(t *testing.T, service *MuxTunnelService, username string) string {
	t.Helper()
	if err := service.StartSSHBackend(0, username); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service.mtx.Lock()
	service.sshBackendListener = listener
	service.mtx.Unlock()
	go service.acceptSSHBackends(listener, username)
	t.Cleanup(service.Close)
	return listener.Addr().String()
}

func dialSSHReverseTestClient(t *testing.T, address, username, token string) *gossh.Client {
	t.Helper()
	client, err := gossh.Dial("tcp", address, &gossh.ClientConfig{
		User:            username,
		Auth:            []gossh.AuthMethod{gossh.Password(token)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestSSHReverseAuthentication(t *testing.T) {
	service := newSSHReverseTestService(t, availablePortRange(t, 1), 1)
	address := startSSHReverseTestServer(t, service, "customuser")

	client := dialSSHReverseTestClient(t, address, "customuser", "test-token")
	_ = client.Close()

	for _, test := range []struct {
		name     string
		username string
		token    string
	}{
		{name: "wrong username", username: "reverseuser", token: "test-token"},
		{name: "wrong token", username: "customuser", token: "wrong-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := gossh.Dial("tcp", address, &gossh.ClientConfig{
				User:            test.username,
				Auth:            []gossh.AuthMethod{gossh.Password(test.token)},
				HostKeyCallback: gossh.InsecureIgnoreHostKey(),
				Timeout:         5 * time.Second,
			})
			if err == nil {
				t.Fatal("authentication unexpectedly succeeded")
			}
		})
	}
}

func TestSSHReverseForwardingAndCleanup(t *testing.T) {
	frontendPort := availablePortRange(t, 1)
	service := newSSHReverseTestService(t, frontendPort, 1)
	address := startSSHReverseTestServer(t, service, DefaultSSHBackendUser)
	client := dialSSHReverseTestClient(t, address, DefaultSSHBackendUser, "test-token")

	forward, err := client.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", frontendPort))
	if err != nil {
		t.Fatal(err)
	}
	backendDone := make(chan error, 1)
	go func() {
		conn, err := forward.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		defer conn.Close()
		_, err = io.Copy(conn, conn)
		backendDone <- err
	}()

	frontendConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", frontendPort), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("through-ssh-reverse")
	if _, err := frontendConn.Write(message); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(message))
	if _, err := io.ReadFull(frontendConn, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != string(message) {
		t.Fatalf("got %q, want %q", response, message)
	}
	_ = frontendConn.Close()

	if err := forward.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-backendDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend connection did not close")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		service.mtx.Lock()
		_, exists := service.sshReverseFrontends[frontendPort]
		service.mtx.Unlock()
		if !exists {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := service.frontendPortPool.GetElement(frontendPort); err != nil {
		t.Fatalf("frontend port was not returned to pool: %v", err)
	}
}

func TestSSHReverseDynamicPortAndSinglePortConstraint(t *testing.T) {
	lowerPort := availablePortRange(t, 2)
	service := newSSHReverseTestService(t, lowerPort, 2)
	address := startSSHReverseTestServer(t, service, DefaultSSHBackendUser)
	client := dialSSHReverseTestClient(t, address, DefaultSSHBackendUser, "test-token")

	forward, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	allocatedPort := forward.Addr().(*net.TCPAddr).Port
	if allocatedPort < lowerPort || allocatedPort >= lowerPort+2 {
		t.Fatalf("allocated port %d is outside test pool", allocatedPort)
	}

	secondPort := lowerPort
	if secondPort == allocatedPort {
		secondPort++
	}
	_, err = client.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(secondPort)))
	if err == nil {
		t.Fatal("second frontend port unexpectedly accepted on one SSH connection")
	}
	_ = forward.Close()
}

func TestSSHReverseRejectsUnsupportedBindAddress(t *testing.T) {
	frontendPort := availablePortRange(t, 1)
	service := newSSHReverseTestService(t, frontendPort, 1)
	address := startSSHReverseTestServer(t, service, DefaultSSHBackendUser)
	client := dialSSHReverseTestClient(t, address, DefaultSSHBackendUser, "test-token")

	_, err := client.Listen("tcp", fmt.Sprintf("example.com:%d", frontendPort))
	if err == nil {
		t.Fatal("unsupported bind address unexpectedly accepted")
	}
}
