package reverselb

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"golang.org/x/crypto/ssh"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func certificate(t *testing.T) (string, string) {
	t.Helper()
	return certificateForNames(t, []string{"localhost"})
}

func certificateForNames(t *testing.T, names []string) (string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
}

func eventually(t *testing.T, description string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", description)
}

func TestDynamicCaddyEndToEnd(t *testing.T) {
	a := testApp(t)
	controlPort, adminPort := freePort(t), freePort(t)
	start := freePort(t)
	for start > 65000 {
		start = freePort(t)
	}
	a.Control.Listen = []string{fmt.Sprintf("127.0.0.1:%d", controlPort)}
	a.Publication.AdminEndpoint = fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	a.Publication.BindHost = "127.0.0.1"
	a.Publication.AdvertiseHost = "127.0.0.1"
	a.Publication.PortStart, a.Publication.PortCount = start, 20
	a.Publication.Templates["plain-http"] = Template{Kind: "http", InstanceDispatch: "direct"}
	a.Publication.Rules = []Rule{{UserID: "default@none", ServicePattern: "web-*", Template: "plain-http"}}
	a.Publication.Templates["wrapped-ssh"] = Template{Kind: "tcp", InstanceDispatch: "direct", FrontendSSH: true}
	a.Publication.Rules = append(a.Publication.Rules, Rule{UserID: "default@none", ServicePattern: "ssh-*", Template: "wrapped-ssh"})
	for _, mode := range []string{"sni", "legacy"} {
		name := "wrapped-" + mode
		a.Publication.Templates[name] = Template{Kind: "tcp", InstanceDispatch: mode, FrontendSSH: true,
			AllowedSources: []string{"127.0.0.0/8"}}
		a.Publication.Rules = append(a.Publication.Rules, Rule{UserID: "default@none", ServicePattern: "wrapped-" + mode, Template: name})
	}
	cert, key := certificate(t)
	config := map[string]any{
		"admin":   map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", adminPort), "config": map[string]any{"persist": false}},
		"storage": map[string]any{"module": "file_system", "root": t.TempDir()},
		"apps": map[string]any{
			"goreverselb": a,
			"tls":         map[string]any{"certificates": map[string]any{"load_pem": []any{map[string]any{"certificate": cert, "key": key}}}},
		},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(data, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { caddy.Stop() })
	eventually(t, "committed runtime", func() bool {
		mod, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
		return err == nil && mod.(*App).runtime.active.Load() != nil
	})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Backend", "tunnel")
		fmt.Fprintf(w, "%s %s", req.Method, req.URL.Path)
	}))
	defer backend.Close()
	_, portStr, _ := net.SplitHostPort(backend.Listener.Addr().String())
	backendPort, _ := strconv.Atoi(portStr)
	web, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "web-demo", Token: "test-only-token",
		TargetPort: backendPort, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 15*time.Second)
	webStatus, err := web.WaitReady(readyCtx)
	cancelReady()
	if err != nil || webStatus.PublicationMode != "dynamic" || webStatus.FrontendPort == 0 ||
		webStatus.FrontendAddress != fmt.Sprintf("127.0.0.1:%d", webStatus.FrontendPort) {
		t.Fatalf("dynamic client readiness: %+v %v", webStatus, err)
	}
	webPort := webStatus.FrontendPort
	httpClient := &http.Client{Timeout: 3 * time.Second}
	getWeb := func() {
		t.Helper()
		resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/example", webPort))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != "tunnel" || string(body) != "GET /example" {
			t.Fatalf("unexpected tunneled response: %d %s", resp.StatusCode, body)
		}
	}
	getWeb()
	mod, _ := caddy.ActiveContext().AppIfConfigured("goreverselb")
	shared := mod.(*App).runtime
	before := shared.registry.Count(tunnelcore.Selector{UserID: "default@none", Service: "web-demo"})
	if before != 1 {
		t.Fatalf("HTTP session count=%d", before)
	}

	tcpBackend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpBackend.Close()
	go func() {
		for {
			conn, err := tcpBackend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				body, err := io.ReadAll(conn)
				if err == nil {
					conn.Write(append([]byte("reply:"), body...))
				}
			}()
		}
	}()
	requested := start + 10
	tcpClient, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "shell", Token: "test-only-token",
		FrontendData: tunnel.FrontendData{Port: requested},
		TargetPort:   tcpBackend.Addr().(*net.TCPAddr).Port, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tcpClient.Close()
	api, err := adminClient(a.Publication.AdminEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer api.client.CloseIdleConnections()
	eventually(t, "TCP native Caddy server visible", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		data, _, status, err := api.request(ctx, http.MethodGet, nil, "")
		if err != nil || status != 200 {
			return false
		}
		_, _, cfg, err := decodeDocument(data)
		if err != nil {
			return false
		}
		for _, ep := range cfg.Generated {
			if ep.Service == "shell" && ep.Port == requested {
				return shared.registry.Count(tunnelcore.Selector{UserID: "default@none", Service: "shell"}) == 1
			}
		}
		return false
	})
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", requested), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("half-close"))
	wrapped, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "ssh-wrapped", Token: "test-only-token",
		FrontendData: tunnel.FrontendData{SSHWrap: true},
		TargetPort:   tcpBackend.Addr().(*net.TCPAddr).Port, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wrapped.Close()
	eventually(t, "SSH dynamic registration", func() bool { return wrapped.FrontendPort() != 0 })
	sshPort := wrapped.FrontendPort()
	signer, err := loadSSHHostKey(context.Background(), shared.storage, "goreverselb/"+a.RuntimeID+"/ssh_host_key")
	if err != nil {
		t.Fatal(err)
	}
	dialSSH := func(port int) *ssh.Client {
		t.Helper()
		client, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
			User: "any-user", Auth: []ssh.AuthMethod{ssh.Password("any-password")},
			HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: 3 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		return client
	}
	sshClient := dialSSH(sshPort)
	openSSH := sshOpenForward(t, sshClient)
	if _, err := io.WriteString(openSSH, "before-"); err != nil {
		t.Fatal(err)
	}
	other, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "web-other", Token: "test-only-token",
		TargetPort: backendPort, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	eventually(t, "publication while TCP stream is open", func() bool { return other.FrontendPort() != 0 })
	if _, err := io.WriteString(openSSH, "reload"); err != nil {
		t.Fatal(err)
	}
	if err := openSSH.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	sshResponse, err := io.ReadAll(openSSH)
	if err != nil || string(sshResponse) != "reply:before-reload" {
		t.Fatalf("SSH channel lost across publication reload: %q %v", sshResponse, err)
	}
	sshReply(t, sshOpenForward(t, sshClient), "new-channel-after-reload")
	sshReply(t, sshOpenForward(t, dialSSH(sshPort)), "same-host-key-after-reload")
	adminCtx, cancelAdmin := context.WithTimeout(context.Background(), 3*time.Second)
	activeJSON, _, status, err := api.request(adminCtx, http.MethodGet, nil, "")
	cancelAdmin()
	if err != nil || status != 200 {
		t.Fatalf("query published SSH route: %d %v", status, err)
	}
	_, activeApps, _, err := decodeDocument(activeJSON)
	if err != nil || !bytes.Contains(activeApps["layer4"], []byte(`"goreverselb_ssh"`)) {
		t.Fatalf("SSH wrapping is not visible in native Caddy configuration: %v", err)
	}
	conn.(*net.TCPConn).CloseWrite()
	reply, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || !bytes.Equal(reply, []byte("reply:half-close")) {
		t.Fatalf("TCP half-close: %q %v", reply, err)
	}
	getWeb()
	mod, _ = caddy.ActiveContext().AppIfConfigured("goreverselb")
	if mod.(*App).runtime != shared {
		t.Fatal("publication reload replaced live registry")
	}
	if count := shared.registry.Count(tunnelcore.Selector{UserID: "default@none", Service: "web-demo"}); count != before {
		t.Fatalf("publication reload disrupted existing session: %d", count)
	}
	tcpClient.Close()
	eventually(t, "last-session native config removal", func() bool {
		current := shared.active.Load()
		for _, ep := range current.Generated {
			if ep.Service == "shell" {
				return false
			}
		}
		return true
	})
	getWeb()
	reused, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "reused", Token: "test-only-token",
		FrontendData: tunnel.FrontendData{Port: requested},
		TargetPort:   tcpBackend.Addr().(*net.TCPAddr).Port, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Close()
	eventually(t, "released port reused by a different endpoint", func() bool {
		return shared.registry.Count(tunnelcore.Selector{UserID: "default@none", Service: "reused"}) == 1
	})
	wrapped.Close()
	eventually(t, "SSH last-session native config removal", func() bool {
		for _, ep := range shared.active.Load().Generated {
			if ep.Service == "ssh-wrapped" {
				return false
			}
		}
		return true
	})
	sshReused, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "ssh-reused", Token: "test-only-token",
		FrontendData: tunnel.FrontendData{SSHWrap: true, Port: sshPort},
		TargetPort:   tcpBackend.Addr().(*net.TCPAddr).Port, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sshReused.Close()
	eventually(t, "SSH requested port reused", func() bool {
		return sshReused.FrontendPort() == sshPort &&
			shared.registry.Count(tunnelcore.Selector{UserID: "default@none", Service: "ssh-reused"}) == 1
	})
	sshReply(t, sshOpenForward(t, dialSSH(sshPort)), "reused-ssh-port")

	legacy, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "wrapped-legacy:blue", Token: "test-only-token",
		FrontendData: tunnel.FrontendData{SSHWrap: true},
		TargetPort:   tcpBackend.Addr().(*net.TCPAddr).Port, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	eventually(t, "SSH legacy instance registration", func() bool { return legacy.FrontendPort() != 0 })
	legacySSH := dialSSH(legacy.FrontendPort())
	legacyChannel := sshOpenForward(t, legacySSH)
	if _, err := io.WriteString(legacyChannel, "PROXY->\x04blue"); err != nil {
		t.Fatal(err)
	}
	sshReply(t, legacyChannel, "legacy-instance")

	backendCertificate, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	tlsBackend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "sni-instance")
	}))
	tlsBackend.TLS = &tls.Config{Certificates: []tls.Certificate{backendCertificate}, MinVersion: tls.VersionTLS12}
	tlsBackend.StartTLS()
	defer tlsBackend.Close()
	sni, err := tunnel.NewMuxTunnelClient(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "wrapped-sni:localhost", Token: "test-only-token",
		FrontendData: tunnel.FrontendData{SSHWrap: true},
		TargetPort:   tlsBackend.Listener.Addr().(*net.TCPAddr).Port, TargetAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sni.Close()
	eventually(t, "SSH SNI instance registration", func() bool { return sni.FrontendPort() != 0 })
	sniSSH := dialSSH(sni.FrontendPort())
	channel, err := sniSSH.Dial("tcp", "ignored-destination.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(cert))
	tlsChannel := tls.Client(channel, &tls.Config{ServerName: "localhost", RootCAs: roots, MinVersion: tls.VersionTLS12})
	defer tlsChannel.Close()
	if _, err := io.WriteString(tlsChannel, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	result, err := io.ReadAll(tlsChannel)
	if err != nil || !bytes.Contains(result, []byte("sni-instance")) {
		t.Fatalf("TLS ClientHello inside SSH channel was not routed to its instance: %q %v", result, err)
	}
}
