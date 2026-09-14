package reverselb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/certmagic"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	"github.com/mholt/caddy-l4/layer4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/ssh"
)

func TestSSHHostKeyPersistence(t *testing.T) {
	storage := &certmagic.FileStorage{Path: t.TempDir()}
	const name = "goreverselb/test/ssh_host_key"
	first, err := loadSSHHostKey(context.Background(), storage, name)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadSSHHostKey(context.Background(), storage, name)
	if err != nil || !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatalf("host identity changed: %v", err)
	}
	info, err := os.Stat(filepath.Join(storage.Path, name))
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("SSH private key is not private: %v", err)
	}
	if err := storage.Store(context.Background(), name, []byte("invalid key")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSSHHostKey(context.Background(), storage, name); err == nil {
		t.Fatal("silently replaced corrupt persisted key")
	}
}

func TestSSHTemplatePolicyAndRendering(t *testing.T) {
	for _, mode := range []string{"direct", "sni", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			var template Template
			d := caddyfile.NewTestDispenser("template wrapped {\nkind tcp\ninstance_dispatch " + mode +
				"\nfrontend_ssh on\nallowed_sources 127.0.0.0/8\n}")
			d.Next()
			d.RemainingArgs()
			if err := template.unmarshal(d); err != nil {
				t.Fatal(err)
			}
			if err := template.validate(); err != nil || !template.FrontendSSH {
				t.Fatalf("SSH template: %+v %v", template, err)
			}
			a := testApp(t)
			a.Publication.Templates[a.Publication.DefaultTemplate] = template
			sel := tunnelcore.Selector{UserID: "default@none", Service: "wrapped", Instance: "blue"}
			if _, err := a.addEndpoint(sel, protocol.TunnelData{}); err == nil {
				t.Fatal("accepted plaintext request on SSH template")
			}
			td := protocol.TunnelData{FrontendData: protocol.FrontendData{SSHWrap: true}}
			if port, err := a.addEndpoint(sel, td); err != nil || port != 8000 {
				t.Fatalf("SSH allocation: %d %v", port, err)
			}
			for _, ep := range a.Generated {
				app, raw, err := renderEndpoint(a.RuntimeID, ep)
				if err != nil || app != "layer4" {
					t.Fatalf("render: %s %v", app, err)
				}
				var server struct {
					Routes []struct {
						Match  json.RawMessage `json:"match"`
						Handle []struct {
							Handler string          `json:"handler"`
							Routes  json.RawMessage `json:"routes"`
						} `json:"handle"`
					} `json:"routes"`
				}
				if err := json.Unmarshal(raw, &server); err != nil {
					t.Fatal(err)
				}
				if len(server.Routes) != 1 || len(server.Routes[0].Handle) != 2 {
					t.Fatalf("wrong SSH route: %s", raw)
				}
				route := server.Routes[0]
				if route.Handle[0].Handler != "goreverselb_ssh" ||
					route.Handle[1].Handler != "subroute" ||
					!bytes.Contains(route.Handle[1].Routes, []byte("goreverselb")) ||
					!bytes.Contains(route.Match, []byte("127.0.0.0/8")) {
					t.Fatalf("SSH must wrap instance routes and enforce sources before handshake: %s", raw)
				}
			}
		})
	}
	for _, template := range []Template{
		{Kind: "http", InstanceDispatch: "direct", FrontendSSH: true},
		{Kind: "tcp", InstanceDispatch: "direct", FrontendSSH: true, FrontendTLS: true, TLSServerName: "localhost"},
	} {
		if err := template.validate(); err == nil {
			t.Fatalf("accepted incompatible wrapping: %+v", template)
		}
	}
	a := testApp(t)
	if _, err := a.addEndpoint(tunnelcore.Selector{UserID: "default@none", Service: "wrapped"},
		protocol.TunnelData{FrontendData: protocol.FrontendData{SSHWrap: true}}); err == nil {
		t.Fatal("silently omitted requested SSH wrapping on plain template")
	}
}

func sshTestClient(t *testing.T, ctx context.Context, next layer4.Handler) (*ssh.Client, net.Conn, *observer.ObservedLogs, <-chan error) {
	t.Helper()
	signer, err := loadSSHHostKey(context.Background(), &certmagic.FileStorage{Path: t.TempDir()}, "key")
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	h := SSHHandler{signer: signer, ctx: ctx, logger: zap.New(core)}
	client, front := adapterTCPPair(t)
	done := make(chan error, 1)
	go func() { done <- h.Handle(layer4.WrapConnection(front, nil, h.logger), next) }()
	conn, channels, requests, err := ssh.NewClientConn(client, client.RemoteAddr().String(), &ssh.ClientConfig{
		User: "any-user", Auth: []ssh.AuthMethod{ssh.Password("never-log-this-password")},
		HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	sshClient := ssh.NewClient(conn, channels, requests)
	t.Cleanup(func() { sshClient.Close() })
	return sshClient, client, logs, done
}

func sshOpenForward(t *testing.T, client *ssh.Client) ssh.Channel {
	t.Helper()
	ch, requests, err := client.OpenChannel("direct-tcpip", ssh.Marshal(sshForwardData{
		DestAddr: "not-a-real-backend.invalid", DestPort: 4321,
		OriginAddr: "203.0.113.7", OriginPort: 54321,
	}))
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(requests)
	t.Cleanup(func() { ch.Close() })
	return ch
}

func sshReply(t *testing.T, ch ssh.Channel, payload string) {
	t.Helper()
	if _, err := io.WriteString(ch, payload); err != nil {
		t.Fatal(err)
	}
	if err := ch.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(ch)
	if err != nil || string(reply) != "reply:"+payload {
		t.Fatalf("SSH half-close response: %q %v", reply, err)
	}
}

func TestSSHForwardingHalfCloseAndDiagnostics(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				body, err := io.ReadAll(conn)
				if err == nil {
					conn.Write(append([]byte("reply:"), body...))
				}
			}()
		}
	}()
	r := &adapterRegistryRuntime{registry: tunnelcore.NewRegistry(tunnelcore.Options{}),
		selector: tunnelcore.Selector{UserID: "test", Service: "wrapped"}}
	t.Cleanup(func() { r.registry.Close() })
	metadata := adapterRegisterYamux(t, r, "ssh", backend.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, raw, logs, done := sshTestClient(t, ctx, layer4.Handlers{&L4Handler{Binding: "web", runtime: r}}.Compile())
	// Multiple simultaneously open channels use independent tunnel streams.
	first, second := sshOpenForward(t, client), sshOpenForward(t, client)
	sshReply(t, first, "one")
	sshReply(t, second, "two")
	for range 2 {
		select {
		case info := <-metadata:
			if info.SourceAddress != raw.LocalAddr().String() {
				t.Fatalf("lost actual SSH peer: %+v", info)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing tunnel metadata")
		}
	}
	if _, _, err := client.OpenChannel("direct-tcpip", []byte("malformed")); err == nil {
		t.Fatal("accepted malformed channel")
	}
	if _, _, err := client.OpenChannel("forwarded-tcpip", nil); err == nil {
		t.Fatal("accepted remote forwarding channel")
	}
	if ok, _, err := client.SendRequest("tcpip-forward", true, nil); err != nil || ok {
		t.Fatalf("remote forwarding must be rejected: %v %v", ok, err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run("not-a-real-command"); err == nil {
		t.Fatal("accepted command execution")
	}
	session.Close()
	client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSH handler did not drain")
	}
	for _, message := range []string{"Upgrading frontend connection to SSH", "SSH wrap password auth attempt",
		"SSH frontend connection established", "SSH frontend connection closed"} {
		if logs.FilterMessage(message).Len() != 1 {
			t.Errorf("missing diagnostic %q", message)
		}
	}
	forwarding := logs.FilterMessage("Forwarding L4 connection through tunnel").All()
	if len(forwarding) != 2 {
		t.Fatalf("expected two forwarding diagnostics, got %d", len(forwarding))
	}
	for _, entry := range forwarding {
		fields := entry.ContextMap()
		for _, name := range []string{"source_address", "frontend_address", "ssh_channel_id",
			"ssh_origin", "ssh_destination", "ssh_user", "binding", "tunnel_local", "tunnel_remote", "stream_id"} {
			if fields[name] == nil {
				t.Errorf("lost hop field %s", name)
			}
		}
		if fields["ssh_origin"] != "203.0.113.7:54321" || fields["ssh_destination"] != "not-a-real-backend.invalid:4321" {
			t.Fatal("lost original forwarding metadata")
		}
	}
	completed := logs.FilterMessage("L4 copy finished").All()
	if len(completed) != 4 {
		t.Fatalf("expected four directional copy records: %d", len(completed))
	}
	for _, entry := range completed {
		fields := entry.ContextMap()
		want := int64(3)
		if fields["direction"] == "tunnel_to_frontend" {
			want += 6
		}
		if fields["bytes"] != want || fields["duration"] == nil || fields["error"] != nil {
			t.Fatalf("incorrect copy diagnostic: %v", fields)
		}
	}
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message+fmt.Sprint(entry.ContextMap()), "never-log-this-password") {
			t.Fatal("SSH password leaked to logs")
		}
	}
}

func TestSSHChannelDeadlineAndRuntimeShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 2)
	next := layer4.HandlerFunc(func(cx *layer4.Connection) error {
		var first [1]byte
		if _, err := io.ReadFull(cx, first[:]); err != nil {
			return err
		}

		if first[0] == 'd' {
			if err := cx.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
				return err
			}
		} else {
			if err := cx.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
				return err
			}
			if err := cx.SetReadDeadline(time.Time{}); err != nil {
				return err
			}
		}
		_, err := io.ReadAll(cx)
		results <- err
		return nil
	})
	client, _, _, done := sshTestClient(t, ctx, next)
	expiring, surviving := sshOpenForward(t, client), sshOpenForward(t, client)
	io.WriteString(expiring, "d")
	io.WriteString(surviving, "s")
	select {
	case err := <-results:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline did not interrupt channel read: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel deadline hung")
	}
	if _, err := io.WriteString(surviving, "still-open"); err != nil {
		t.Fatalf("deadline closed sibling channel: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime cancellation did not close SSH connection and channels")
	}
}

type unresponsiveSSHChannel struct {
	stopped chan struct{}
	once    sync.Once
}

func (c *unresponsiveSSHChannel) Read([]byte) (int, error) {
	<-c.stopped
	return 0, io.EOF
}

func (c *unresponsiveSSHChannel) Write([]byte) (int, error) {
	<-c.stopped
	return 0, io.ErrClosedPipe
}

// Sending SSH CLOSE does not imply that a peer acknowledges it.
func (c *unresponsiveSSHChannel) Close() error      { return nil }
func (c *unresponsiveSSHChannel) CloseWrite() error { return nil }
func (c *unresponsiveSSHChannel) SendRequest(string, bool, []byte) (bool, error) {
	return false, nil
}
func (c *unresponsiveSSHChannel) Stderr() io.ReadWriter { return c }
func (c *unresponsiveSSHChannel) stop() {
	c.once.Do(func() { close(c.stopped) })
}

func TestSSHChannelDeadlinesDoNotRequirePeerAcknowledgment(t *testing.T) {
	channel := &unresponsiveSSHChannel{stopped: make(chan struct{})}
	defer channel.stop()
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
	conn := newSSHChannelConn(channel, addr, addr)
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		results <- err
	}()
	go func() {
		_, err := conn.Write(make([]byte, 128*1024))
		results <- err
	}()
	for range 2 {
		select {
		case err := <-results:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("I/O did not report its deadline: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("channel I/O waited for unresponsive SSH peer")
		}
	}
	conn.Close()
	channel.stop()
	done := make(chan struct{})
	go func() { conn.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SSH pumps did not exit after transport shutdown")
	}
}
