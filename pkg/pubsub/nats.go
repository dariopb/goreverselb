package pubsub

import (
	"crypto/tls"
	"crypto/x509"

	stand "github.com/nats-io/nats-streaming-server/server"
	stores "github.com/nats-io/nats-streaming-server/stores"

	"github.com/nats-io/nats.go"
)

func NewNatsServer(cert tls.Certificate, port int, token string) (*stand.StanServer, error) {
	tlsconfig := &tls.Config{Certificates: []tls.Certificate{cert}}
	roots := x509.NewCertPool()

	//roots.AddCert()
	tlsconfig.RootCAs = roots
	tlsconfig.InsecureSkipVerify = true

	// Get NATS Streaming Server default options
	opts := stand.GetDefaultOptions()
	opts.EnableLogging = true
	opts.StoreLimits = stores.DefaultStoreLimits
	opts.StoreLimits.MsgStoreLimits.MaxMsgs = 2
	opts.ID = "pubsub-server"
	opts.CustomLogger = LrusLogger{}
	//opts.Secure = true

	oo := []nats.Option{nats.Name("nats_stream_server")}
	oo = append(oo, nats.Secure(tlsconfig))
	opts.NATSClientOpts = oo

	snopts := stand.NewNATSOptions()
	//snopts.HTTPPort = 8223
	snopts.TLSConfig = tlsconfig
	snopts.Authorization = token
	snopts.Port = port

	// Run the server with the streaming and streaming/nats options.
	s, err := stand.RunServerWithOpts(opts, snopts)
	if err != nil {
		return nil, err
	}

	return s, nil
}
