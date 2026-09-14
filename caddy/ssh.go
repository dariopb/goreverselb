package reverselb

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/certmagic"
	"github.com/mholt/caddy-l4/layer4"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

func init() { caddy.RegisterModule(SSHHandler{}) }

// SSHHandler unwraps SSH local forwarding before running the remaining L4 chain.
// Password acceptance intentionally matches the standalone encryption-only wrapper.
type SSHHandler struct {
	signer ssh.Signer
	ctx    context.Context
	logger *zap.Logger
}

func (SSHHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "layer4.handlers.goreverselb_ssh", New: func() caddy.Module { return new(SSHHandler) }}
}

func (h *SSHHandler) Provision(ctx caddy.Context) error {
	r, err := bindingRuntime(ctx)
	if err != nil {
		return err
	}
	h.ctx, h.logger = r.ctx, ctx.Logger()
	h.signer, err = loadSSHHostKey(ctx, r.storage, "goreverselb/"+r.id+"/ssh_host_key")
	if err != nil {
		return fmt.Errorf("SSH frontend host key: %w", err)
	}
	h.logger.Info("Loaded SSH frontend host key",
		zap.String("fingerprint", ssh.FingerprintSHA256(h.signer.PublicKey())))
	return nil
}

func loadSSHHostKey(ctx context.Context, storage certmagic.Storage, key string) (signer ssh.Signer, err error) {
	if err = storage.Lock(ctx, key); err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, storage.Unlock(ctx, key)) }()
	data, err := storage.Load(ctx, key)
	if err == nil {
		return ssh.ParsePrivateKey(data)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return nil, err
	}
	if err := storage.Store(ctx, key, pem.EncodeToMemory(block)); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(private)
}

func (h *SSHHandler) Handle(cx *layer4.Connection, next layer4.Handler) error {
	if h.signer == nil || h.ctx == nil || next == nil {
		return errors.New("SSH frontend handler is not provisioned")
	}
	defer cx.Close()
	logger := cx.Logger
	if logger == nil {
		logger = h.logger
	}
	logger = logger.With(zap.String("source_address", cx.RemoteAddr().String()),
		zap.String("frontend_address", cx.LocalAddr().String()))
	logger.Info("Upgrading frontend connection to SSH")
	ctx, cancel := context.WithCancel(cx.Context)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	stopConn := context.AfterFunc(ctx, func() { _ = cx.Close() })
	defer stopConn()
	if err := cx.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, _ []byte) (*ssh.Permissions, error) {
			logger.Info("SSH wrap password auth attempt", zap.String("ssh_user", meta.User()))
			return nil, nil
		},
		BannerCallback: func(ssh.ConnMetadata) string {
			return "##########################\n# reverselb ssh endpoint #\n##########################\n"
		},
	}
	config.AddHostKey(h.signer)
	conn, channels, requests, err := ssh.NewServerConn(cx, config)
	if err != nil {
		logger.Debug("SSH handshake failed", zap.Error(err))
		return fmt.Errorf("SSH frontend handshake: %w", err)
	}
	defer conn.Close()
	if err := cx.SetDeadline(time.Time{}); err != nil {
		return err
	}
	logger = logger.With(zap.String("ssh_user", conn.User()))
	logger.Debug("SSH frontend connection established")
	go ssh.DiscardRequests(requests)
	var wg sync.WaitGroup
	// Bound forwarding/session goroutines without blocking SSH channel rejection.
	slots := make(chan struct{}, 32)
	var channelID uint64
	for incoming := range channels {
		channelID++
		entry := logger.With(zap.Uint64("ssh_channel_id", channelID),
			zap.String("ssh_channel_type", incoming.ChannelType()))
		if incoming.ChannelType() != "direct-tcpip" && incoming.ChannelType() != "session" {
			entry.Debug("SSH channel rejected")
			if err := incoming.Reject(ssh.UnknownChannelType, "only local TCP forwarding is supported"); err != nil {
				entry.Debug("SSH channel rejection failed", zap.Error(err))
			}
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			entry.Debug("SSH channel limit reached")
			if err := incoming.Reject(ssh.ResourceShortage, "SSH channel limit reached"); err != nil {
				entry.Debug("SSH channel rejection failed", zap.Error(err))
			}
			continue
		}
		wg.Add(1)
		go func(incoming ssh.NewChannel, entry *zap.Logger) {
			defer wg.Done()
			defer func() { <-slots }()
			if err := h.handleChannel(ctx, cx, incoming, entry, next); err != nil {
				entry.Error("SSH channel failed", zap.Error(err))
			}
		}(incoming, entry)
	}
	cancel()
	_ = conn.Close()
	wg.Wait()
	logger.Debug("SSH frontend connection closed")
	return nil
}

type sshForwardData struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

func (h *SSHHandler) handleChannel(ctx context.Context, parent *layer4.Connection, incoming ssh.NewChannel, logger *zap.Logger, next layer4.Handler) error {
	if incoming.ChannelType() == "direct-tcpip" {
		var data sshForwardData
		if err := ssh.Unmarshal(incoming.ExtraData(), &data); err != nil || data.DestPort > 65535 || data.OriginPort > 65535 {
			return errors.Join(errors.New("invalid SSH forward metadata"),
				incoming.Reject(ssh.ConnectionFailed, "invalid forward metadata"))
		}
		logger = logger.With(
			zap.String("ssh_origin", net.JoinHostPort(data.OriginAddr, strconv.Itoa(int(data.OriginPort)))),
			zap.String("ssh_destination", net.JoinHostPort(data.DestAddr, strconv.Itoa(int(data.DestPort)))))
		logger.Info("DirectTCPIPHandler request")
	}
	channel, requests, err := incoming.Accept()
	if err != nil {
		return fmt.Errorf("accept SSH channel: %w", err)
	}
	wrapped := newSSHChannelConn(channel, parent.LocalAddr(), parent.RemoteAddr())
	defer func() {
		_ = wrapped.Close()
		wrapped.workers.Wait()
	}()
	stop := context.AfterFunc(ctx, func() { _ = wrapped.Close() })
	defer stop()
	if incoming.ChannelType() == "session" {
		for request := range requests {
			shell := request.Type == "shell"
			if request.WantReply {
				if err := request.Reply(shell, nil); err != nil {
					return err
				}
			}
			if shell {
				_, err := io.WriteString(channel, "Only port forwarding available...\nUse '-N' flag to not start a terminal session\n")
				return err
			}
		}
		return nil
	}
	go ssh.DiscardRequests(requests)
	// Fresh per-channel matcher state; keep the actual SSH peer as the source.
	front := layer4.WrapConnection(wrapped, nil, logger)
	channelCtx, cancel := context.WithCancel(front.Context)
	defer cancel()
	stopContext := context.AfterFunc(ctx, cancel)
	defer stopContext()
	front.Context = channelCtx
	logger.Debug("SSH forwarding channel opened")
	err = next.Handle(front)
	logger.Debug("SSH forwarding channel closed", zap.Error(err))
	return err
}

// SSH has no per-channel deadlines, and Channel.Close only sends a close packet.
// Local pipes interrupt I/O even if the peer does not acknowledge that packet.
// The caller retains its channel slot until the pumps finish or SSH disconnects.
type sshChannelConn struct {
	ssh.Channel
	local, remote             net.Addr
	reader, writer            net.Conn
	done, readDone, writeDone chan struct{}
	workers                   sync.WaitGroup
	closeOnce, writeOnce      sync.Once
	readErr, writeErr         error
}

func newSSHChannelConn(channel ssh.Channel, local, remote net.Addr) *sshChannelConn {
	reader, incoming := net.Pipe()
	writer, outgoing := net.Pipe()
	c := &sshChannelConn{Channel: channel, local: local, remote: remote, reader: reader, writer: writer,
		done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{})}
	c.workers.Add(3)
	go func() {
		defer c.workers.Done()
		defer incoming.Close()
		_, c.readErr = io.Copy(incoming, channel)
		close(c.readDone)
	}()
	go func() {
		defer c.workers.Done()
		defer close(c.writeDone)
		defer outgoing.Close()
		_, c.writeErr = io.Copy(channel, outgoing)
		if c.writeErr == nil {
			c.writeErr = channel.CloseWrite()
		}
	}()
	go func() {
		defer c.workers.Done()
		<-c.done
		_ = channel.Close()
	}()
	return c
}

func (c *sshChannelConn) LocalAddr() net.Addr  { return c.local }
func (c *sshChannelConn) RemoteAddr() net.Addr { return c.remote }
func (c *sshChannelConn) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if err == io.EOF {
		<-c.readDone
		if c.readErr != nil {
			err = c.readErr
		}
	}
	return n, err
}

func (c *sshChannelConn) Write(p []byte) (int, error) {
	n, err := c.writer.Write(p)
	if err != nil {
		select {
		case <-c.writeDone:
			if c.writeErr != nil {
				err = c.writeErr
			}
		default:
		}
	}
	return n, err
}

func (c *sshChannelConn) CloseWrite() error {
	c.writeOnce.Do(func() { _ = c.writer.Close() })
	select {
	case <-c.writeDone:
		return c.writeErr
	case <-c.done:
		return net.ErrClosed
	}
}

func (c *sshChannelConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.reader.Close()
		_ = c.writer.Close()
	})
	return nil
}

func (c *sshChannelConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *sshChannelConn) SetReadDeadline(t time.Time) error  { return c.reader.SetReadDeadline(t) }
func (c *sshChannelConn) SetWriteDeadline(t time.Time) error { return c.writer.SetWriteDeadline(t) }

var (
	_ caddy.Provisioner  = (*SSHHandler)(nil)
	_ layer4.NextHandler = (*SSHHandler)(nil)
	_ net.Conn           = (*sshChannelConn)(nil)
)
