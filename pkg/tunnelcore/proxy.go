package tunnelcore

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

func closeWrite(conn net.Conn) error {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	// In yamux v0.1.2 Close sends FIN and prohibits writes, but leaves reads
	// usable until the peer sends its FIN. There is no CloseWrite method.
	if stream, ok := conn.(*yamux.Stream); ok {
		return stream.Close()
	}
	return conn.Close()
}

// Proxy copies both directions, preserving half-closes and delayed responses.
// It owns both connections until completion and closes them on cancellation.
func Proxy(ctx context.Context, a, b net.Conn) error {
	return ProxyWithObserver(ctx, a, b, nil)
}

type CopyDirection string

const (
	AToB CopyDirection = "a_to_b"
	BToA CopyDirection = "b_to_a"
)

type CopyResult struct {
	Direction CopyDirection
	Bytes     int64
	Err       error
}

// ProxyWithObserver reports each completed copy, including bytes transferred
// before an error. The observer may be called concurrently by both directions.
func ProxyWithObserver(ctx context.Context, a, b net.Conn, observe func(CopyResult)) error {
	var once sync.Once
	abort := func() {
		once.Do(func() {
			// Raw yamux Close is only a half-close; deadlines interrupt reads too.
			_ = a.SetDeadline(time.Now())
			_ = b.SetDeadline(time.Now())
			_ = a.Close()
			_ = b.Close()
		})
	}
	stop := context.AfterFunc(ctx, abort)
	defer stop()
	defer abort()
	results := make(chan error, 2)
	copyDirection := func(dst, src net.Conn, direction CopyDirection) {
		n, err := io.Copy(dst, src)
		if err == nil {
			err = closeWrite(dst)
		}
		if err != nil {
			abort()
		}
		if observe != nil {
			observe(CopyResult{Direction: direction, Bytes: n, Err: err})
		}
		results <- err
	}
	go copyDirection(a, b, BToA)
	go copyDirection(b, a, AToB)
	err := errors.Join(<-results, <-results)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
