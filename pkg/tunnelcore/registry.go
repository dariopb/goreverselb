// Package tunnelcore provides framework-independent session selection and
// stream dialing. Hosts own authentication and registration policy.
package tunnelcore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	"github.com/hashicorp/yamux"
)

var (
	ErrUnavailable       = errors.New("no available tunnel session")
	ErrResourceExhausted = errors.New("tunnel stream limit reached")
	ErrClosed            = errors.New("tunnel registry closed")
	ErrOpenTimeout       = errors.New("tunnel stream open timeout")
)

type Selector struct {
	UserID   string `json:"user_id"`
	Service  string `json:"service"`
	Instance string `json:"instance"`
}

type ConnectionInfo struct {
	SourceAddress string
}

type Dialer interface {
	DialContext(context.Context, Selector, ConnectionInfo) (net.Conn, error)
}

type Options struct {
	// Limits include pending opens, including canceled opens still inside yamux.
	// Nonpositive values use bounded defaults: 4096 total, 1024 per user, 10s.
	MaxStreams        int
	MaxStreamsPerUser int
	OpenTimeout       time.Duration
}

type registration struct {
	id       string
	selector Selector
	session  *yamux.Session
	done     chan struct{}
	streams  map[*streamConn]struct{}
}

type Registry struct {
	mu      sync.Mutex
	options Options
	entries map[string]*registration
	users   map[string]int
	active  int
	closed  bool
	wg      sync.WaitGroup
}

func NewRegistry(options Options) *Registry {
	if options.MaxStreams <= 0 {
		options.MaxStreams = 4096
	}
	if options.MaxStreamsPerUser <= 0 {
		options.MaxStreamsPerUser = 1024
	}
	if options.OpenTimeout <= 0 {
		options.OpenTimeout = 10 * time.Second
	}
	return &Registry{options: options, entries: make(map[string]*registration), users: make(map[string]int)}
}

func (r *Registry) Register(id string, selector Selector, session *yamux.Session) error {
	if id == "" || session == nil {
		return fmt.Errorf("registration requires an ID and session")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if session.IsClosed() {
		return ErrUnavailable
	}
	if _, exists := r.entries[id]; exists {
		return fmt.Errorf("duplicate tunnel session ID %q", id)
	}
	for _, entry := range r.entries {
		if entry.session == session {
			return fmt.Errorf("tunnel session is already registered")
		}
	}
	entry := &registration{id: id, selector: selector, session: session, done: make(chan struct{}), streams: make(map[*streamConn]struct{})}
	r.entries[id] = entry
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		select {
		case <-session.CloseChan():
			r.remove(entry)
		case <-entry.done:
		}
	}()
	return nil
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	entry := r.entries[id]
	r.mu.Unlock()
	if entry != nil {
		r.remove(entry)
	}
}

func (r *Registry) remove(entry *registration) {
	r.mu.Lock()
	if r.entries[entry.id] != entry {
		r.mu.Unlock()
		return
	}
	delete(r.entries, entry.id)
	r.wg.Add(1)
	close(entry.done)
	streams := make([]*streamConn, 0, len(entry.streams))
	for stream := range entry.streams {
		streams = append(streams, stream)
	}
	r.mu.Unlock()
	defer r.wg.Done()
	_ = entry.session.Close()
	for _, stream := range streams {
		_ = stream.Close()
	}
}

func (r *Registry) Available(selector Selector) bool { return r.Count(selector) > 0 }

func (r *Registry) Count(selector Selector) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, entry := range r.entries {
		if entry.selector == selector && !entry.session.IsClosed() {
			count++
		}
	}
	return count
}

func (r *Registry) DialContext(ctx context.Context, selector Selector, info ConnectionInfo) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	var entries []*registration
	for _, entry := range r.entries {
		if entry.selector == selector && !entry.session.IsClosed() {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		r.mu.Unlock()
		return nil, ErrUnavailable
	}
	if r.active >= r.options.MaxStreams || r.users[selector.UserID] >= r.options.MaxStreamsPerUser {
		r.mu.Unlock()
		return nil, ErrResourceExhausted
	}
	entry := entries[rand.Intn(len(entries))]
	r.active++
	r.users[selector.UserID]++
	r.wg.Add(1)
	r.mu.Unlock()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			r.mu.Lock()
			r.active--
			r.users[selector.UserID]--
			if r.users[selector.UserID] == 0 {
				delete(r.users, selector.UserID)
			}
			r.mu.Unlock()
		})
	}
	openCtx, cancel := context.WithTimeout(ctx, r.options.OpenTimeout)
	defer cancel()
	type result struct {
		conn *streamConn
		err  error
	}
	results := make(chan result)
	go func() {
		defer r.wg.Done()
		stream, err := entry.session.OpenStream()
		var conn *streamConn
		if err == nil {
			conn = &streamConn{Stream: stream, done: make(chan struct{})}
			conn.release = func() {
				r.mu.Lock()
				delete(entry.streams, conn)
				r.mu.Unlock()
				release()
			}
			r.mu.Lock()
			if r.entries[entry.id] != entry {
				err = ErrUnavailable
			} else {
				entry.streams[conn] = struct{}{}
			}
			r.mu.Unlock()
			if err == nil {
				err = openCtx.Err()
			}
			if err == nil {
				deadline, _ := openCtx.Deadline()
				err = conn.SetWriteDeadline(deadline)
				if err == nil {
					name := selector.Service
					if selector.Instance != "" {
						name += ":" + selector.Instance
					}
					err = protocol.WriteObject(conn, protocol.TunnelConnecData{
						ServiceName: name, SourceAddress: info.SourceAddress,
					})
				}
				if err == nil {
					err = conn.SetWriteDeadline(time.Time{})
				}
			}
		}
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			} else {
				release()
			}
		}
		select {
		case results <- result{conn, err}:
			return
		case <-openCtx.Done():
		case <-entry.done:
		}
		if conn != nil {
			_ = conn.Close()
		}
	}()
	select {
	case result := <-results:
		if err := openCtx.Err(); err != nil {
			if result.conn != nil {
				_ = result.conn.Close()
			}
			return nil, openError(ctx, err)
		}
		if result.err != nil {
			if errors.Is(result.err, yamux.ErrTimeout) {
				return nil, errors.Join(ErrOpenTimeout, result.err)
			}
			if entry.session.IsClosed() {
				return nil, errors.Join(ErrUnavailable, result.err)
			}
			return nil, fmt.Errorf("open tunnel stream: %w", result.err)
		}
		select {
		case <-entry.done:
			_ = result.conn.Close()
			return nil, ErrUnavailable
		default:
			return result.conn, nil
		}
	case <-openCtx.Done():
		return nil, openError(ctx, openCtx.Err())
	case <-entry.done:
		return nil, ErrUnavailable
	}
}

func openError(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	return errors.Join(ErrOpenTimeout, err)
}

func (r *Registry) Close() error {
	r.mu.Lock()
	r.closed = true
	entries := make([]*registration, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
	}
	r.mu.Unlock()
	for _, entry := range entries {
		r.remove(entry)
	}
	r.wg.Wait()
	return nil
}

type streamConn struct {
	*yamux.Stream
	done       chan struct{}
	once       sync.Once
	deadlineMu sync.Mutex
	release    func()
}

// Done signals full connection closure or session invalidation, not a FIN.
func (c *streamConn) Done() <-chan struct{} { return c.done }

func (c *streamConn) CloseWrite() error {
	select {
	case <-c.done:
		return net.ErrClosed
	default:
		return c.Stream.Close()
	}
}

func (c *streamConn) Close() error {
	c.once.Do(func() {
		c.deadlineMu.Lock()
		close(c.done)
		_ = c.Stream.SetDeadline(time.Now())
		c.deadlineMu.Unlock()
		_ = c.Stream.Close()
		c.release()
	})
	return nil
}

func (c *streamConn) SetDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	select {
	case <-c.done:
		return net.ErrClosed
	default:
		return c.Stream.SetDeadline(t)
	}
}

func (c *streamConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	select {
	case <-c.done:
		return net.ErrClosed
	default:
		return c.Stream.SetReadDeadline(t)
	}
}

func (c *streamConn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	select {
	case <-c.done:
		return net.ErrClosed
	default:
		return c.Stream.SetWriteDeadline(t)
	}
}

func (c *streamConn) Read(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
		return c.Stream.Read(p)
	}
}

func (c *streamConn) Write(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
		return c.Stream.Write(p)
	}
}
