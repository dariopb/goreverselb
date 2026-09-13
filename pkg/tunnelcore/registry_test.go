package tunnelcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	"github.com/hashicorp/yamux"
)

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := listener.Accept()
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func sessionPair(t *testing.T, backlog int) (*yamux.Session, *yamux.Session) {
	t.Helper()
	a, b := tcpPair(t)
	config := yamux.DefaultConfig()
	config.EnableKeepAlive = false
	config.AcceptBacklog = backlog
	config.StreamOpenTimeout = 0
	config.StreamCloseTimeout = time.Second
	config.ConnectionWriteTimeout = time.Second
	server, err := yamux.Server(a, config)
	if err != nil {
		t.Fatal(err)
	}
	client, err := yamux.Client(b, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close(); client.Close() })
	return server, client
}

func acceptMetadata(session *yamux.Session) (*yamux.Stream, protocol.TunnelConnecData, error) {
	stream, err := session.AcceptStream()
	if err != nil {
		return nil, protocol.TunnelConnecData{}, err
	}
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := protocol.ReadFrame(stream)
	var metadata protocol.TunnelConnecData
	if err == nil {
		err = json.Unmarshal(b, &metadata)
	}
	return stream, metadata, err
}

func TestRegistryHalfCloseMetadataAndInvalidation(t *testing.T) {
	server, client := sessionPair(t, 8)
	r := NewRegistry(Options{})
	t.Cleanup(func() { r.Close() })
	selector := Selector{UserID: "user", Service: "echo", Instance: "one"}
	if err := r.Register("session", selector, server); err != nil {
		t.Fatal(err)
	}
	peerResult := make(chan error, 1)
	go func() {
		stream, metadata, err := acceptMetadata(client)
		if err != nil {
			peerResult <- err
			return
		}
		defer stream.Close()
		if metadata.ServiceName != "echo:one" || metadata.SourceAddress != "127.0.0.1:123" {
			peerResult <- fmt.Errorf("metadata: %+v", metadata)
			return
		}
		request, err := io.ReadAll(stream)
		if err != nil || string(request) != "request" {
			peerResult <- fmt.Errorf("read request %q: %v", request, err)
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, err = stream.Write([]byte("delayed response"))
		peerResult <- err
	}()
	conn, err := r.DialContext(context.Background(), selector, ConnectionInfo{SourceAddress: "127.0.0.1:123"})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(conn)
	if err != nil || string(response) != "delayed response" {
		t.Fatalf("half-close response %q: %v", response, err)
	}
	if err := <-peerResult; err != nil {
		t.Fatal(err)
	}
	done := conn.(interface{ Done() <-chan struct{} }).Done()
	select {
	case <-done:
		t.Fatal("FIN must not invalidate pooled connection before full close")
	default:
	}
	r.Remove("session")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("removal did not invalidate connection")
	}
	if r.Available(selector) || !server.IsClosed() {
		t.Fatal("removed session remains live")
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read removed connection: %v", err)
	}
}

func TestRegistryAdmissionAndPendingCancellation(t *testing.T) {
	server, _ := sessionPair(t, 1)
	r := NewRegistry(Options{MaxStreams: 2, MaxStreamsPerUser: 2, OpenTimeout: time.Second})
	t.Cleanup(func() { r.Close() })
	selector := Selector{UserID: "user", Service: "service"}
	if err := r.Register("id", selector, server); err != nil {
		t.Fatal(err)
	}
	first, err := r.DialContext(context.Background(), selector, ConnectionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := r.DialContext(ctx, selector, ConnectionInfo{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending open not canceled: %v", err)
	}
	if _, err := r.DialContext(context.Background(), selector, ConnectionInfo{}); !errors.Is(err, ErrResourceExhausted) {
		t.Fatalf("canceled pending open escaped admission count: %v", err)
	}
	r.Remove("id")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 || len(r.users) != 0 {
		t.Fatalf("reservations leaked: %d, %v", r.active, r.users)
	}
}

func TestRegistrySelectionLimitsAndFailures(t *testing.T) {
	r := NewRegistry(Options{MaxStreams: 3, MaxStreamsPerUser: 1})
	t.Cleanup(func() { r.Close() })
	selector := Selector{UserID: "user", Service: "service"}
	if _, err := r.DialContext(context.Background(), selector, ConnectionInfo{}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	seen := make(chan int, 100)
	for i := 0; i < 2; i++ {
		server, client := sessionPair(t, 64)
		if err := r.Register(fmt.Sprint(i), selector, server); err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				stream, _, err := acceptMetadata(client)
				if err != nil {
					return
				}
				seen <- i
				stream.Close()
			}
		}()
	}
	if r.Count(selector) != 2 {
		t.Fatal("wrong session count")
	}
	counts := [2]int{}
	for i := 0; i < 50; i++ {
		conn, err := r.DialContext(context.Background(), selector, ConnectionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.DialContext(context.Background(), selector, ConnectionInfo{}); !errors.Is(err, ErrResourceExhausted) {
			t.Fatalf("per-user limit: %v", err)
		}
		conn.Close()
		select {
		case index := <-seen:
			counts[index]++
		case <-time.After(time.Second):
			t.Fatal("missing metadata")
		}
	}
	if counts[0] == 0 || counts[1] == 0 {
		t.Fatalf("session selection not distributed: %v", counts)
	}
	if _, err := r.DialContext(context.Background(), selector, ConnectionInfo{SourceAddress: string(make([]byte, 1001))}); err == nil {
		t.Fatal("oversize metadata accepted")
	}
	r.Close()
	r.Close()
	if _, err := r.DialContext(context.Background(), selector, ConnectionInfo{}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestRegistryOpenTimeout(t *testing.T) {
	server, _ := sessionPair(t, 1)
	r := NewRegistry(Options{OpenTimeout: 20 * time.Millisecond})
	t.Cleanup(func() { r.Close() })
	selector := Selector{}
	if err := r.Register("id", selector, server); err != nil {
		t.Fatal(err)
	}
	conn, err := r.DialContext(context.Background(), selector, ConnectionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := r.DialContext(context.Background(), selector, ConnectionInfo{}); !errors.Is(err, ErrOpenTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("open timeout classification: %v", err)
	}
}

func TestCanceledOpenClosesLateStream(t *testing.T) {
	server, client := sessionPair(t, 1)
	r := NewRegistry(Options{MaxStreams: 2, OpenTimeout: time.Second})
	defer r.Close()
	if err := r.Register("id", Selector{}, server); err != nil {
		t.Fatal(err)
	}
	first, err := r.DialContext(context.Background(), Selector{}, ConnectionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.DialContext(ctx, Selector{}, ConnectionInfo{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending open: %v", err)
	}
	stream, _, err := acceptMetadata(client)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	acceptCtx, acceptCancel := context.WithTimeout(context.Background(), time.Second)
	defer acceptCancel()
	late, err := client.AcceptStreamWithContext(acceptCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	_ = late.SetReadDeadline(time.Now().Add(time.Second))
	b, err := io.ReadAll(late)
	if err != nil || len(b) != 0 {
		t.Fatalf("late stream wasn't closed before metadata: %q %v", b, err)
	}
	first.Close()
	r.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		t.Fatalf("late stream leaked a reservation: %d", r.active)
	}
}

func TestRegistrySessionLossAndGlobalLimit(t *testing.T) {
	r := NewRegistry(Options{MaxStreams: 2, MaxStreamsPerUser: 2})
	defer r.Close()
	var conns []net.Conn
	var clients []*yamux.Session
	for i := 0; i < 3; i++ {
		selector := Selector{UserID: fmt.Sprint(i), Service: "service"}
		server, client := sessionPair(t, 8)
		clients = append(clients, client)
		if err := r.Register(fmt.Sprint(i), selector, server); err != nil {
			t.Fatal(err)
		}
		conn, err := r.DialContext(context.Background(), selector, ConnectionInfo{})
		if i == 2 {
			if !errors.Is(err, ErrResourceExhausted) {
				t.Fatalf("global stream limit: %v", err)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		defer conn.Close()
	}
	done := conns[0].(interface{ Done() <-chan struct{} }).Done()
	clients[0].Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("transport loss did not invalidate pooled connection")
	}
	if r.Available(Selector{UserID: "0", Service: "service"}) {
		t.Fatal("dead session remains selectable")
	}
}

func TestRegistryConcurrentLifecycle(t *testing.T) {
	r := NewRegistry(Options{OpenTimeout: 50 * time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		server, _ := sessionPair(t, 8)
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint(i)
			if err := r.Register(id, Selector{}, server); err != nil {
				return
			}
			conn, _ := r.DialContext(context.Background(), Selector{}, ConnectionInfo{})
			if conn != nil {
				conn.Close()
			}
			r.Remove(id)
		}()
	}
	r.Close()
	wg.Wait()
}
