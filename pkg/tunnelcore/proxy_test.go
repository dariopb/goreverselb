package tunnelcore

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestProxyTCPAndYamuxHalfClose(t *testing.T) {
	server, client := sessionPair(t, 8)
	stream, err := server.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := client.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	consumer, frontend := tcpPair(t)
	_ = consumer.SetDeadline(time.Now().Add(5 * time.Second))
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	result := make(chan error, 1)
	copies := make(chan CopyResult, 2)
	go func() {
		result <- ProxyWithObserver(context.Background(), frontend, stream, func(result CopyResult) {
			copies <- result
		})
	}()
	if _, err := consumer.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	consumer.(*net.TCPConn).CloseWrite()
	request, err := io.ReadAll(peer)
	if err != nil || string(request) != "request" {
		t.Fatalf("request: %q %v", request, err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := peer.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	peer.Close()
	response, err := io.ReadAll(consumer)
	if err != nil || string(response) != "response" {
		t.Fatalf("response: %q %v", response, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not complete")
	}
	want := map[CopyDirection]int64{AToB: int64(len("request")), BToA: int64(len("response"))}
	for range 2 {
		result := <-copies
		if n, ok := want[result.Direction]; !ok || result.Bytes != n || result.Err != nil {
			t.Fatalf("unexpected copy diagnostic: %+v", result)
		}
		delete(want, result.Direction)
	}
}

func TestProxyCancellationUnblocksYamuxReads(t *testing.T) {
	server, client := sessionPair(t, 8)
	stream, err := server.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AcceptStream(); err != nil {
		t.Fatal(err)
	}
	_, frontend := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Proxy(ctx, frontend, stream) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled proxy leaked")
	}
}
