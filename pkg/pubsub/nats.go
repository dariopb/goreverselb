package pubsub

import (
	"crypto/tls"
	"time"

	natsd "github.com/nats-io/nats-server/server"
)

func NewNatsServer(cert tls.Certificate, servicePort int, token string) {
	tlsconfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	// This configure the NATS Server using natsd package
	nopts := &natsd.Options{}
	nopts.Port = 8223
	//nopts.HTTPSPort = 8443
	nopts.TLSConfig = tlsconfig

	// Setting a customer client authentication requires the NATS Server Authentication interface.
	//nopts.CustomClientAuthentication = &myCustomClientAuth{}

	// Create the NATS Server
	ns := natsd.New(nopts)
	//ns.SetLogger(log.StandardLogger())
	ns.ConfigureLogger()

	// Start it as a go routine
	go func() {
		ns.Start()
	}()

	// Wait for it to be able to accept connections
	if !ns.ReadyForConnections(10 * time.Second) {
		//	panic("not able to start")
	}

}
