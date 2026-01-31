package main

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"os"
	"time"

	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/urfave/cli/v2"

	log "github.com/sirupsen/logrus"
)

func stdinProxy(ctx *cli.Context) error {
	loglevel := log.DebugLevel
	if l, err := log.ParseLevel(cfg.LogLevel); err == nil {
		loglevel = l
	}

	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	log.SetLevel(loglevel)
	log.SetOutput(os.Stdout)

	instanceName := cfg.InstanceName
	if instanceName == "" {
		instanceName = cfg.ServiceEndpoint
	}

	doProxy(instanceName)

	//c := make(chan os.Signal, 2)
	//signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	//<-c

	return nil
}

func doProxy(instanceName string) error {
	tcpAddr, err := net.ResolveTCPAddr("tcp", cfg.ServiceEndpoint)
	if err != nil {
		log.Errorf("session: [%s], failed to resolve endpoint [%s]", tcpAddr.String(), err.Error())
		return err
	}

	var backConn net.Conn

	d := &net.Dialer{Timeout: 5 * time.Second}

	if cfg.WrapTLS {
		tlsconfig := &tls.Config{
			InsecureSkipVerify: cfg.InsecureTLS,
			ServerName:         instanceName,
		}

		backConn, err = tls.DialWithDialer(d, "tcp", tcpAddr.String(), tlsconfig)
	} else {
		backConn, err = d.Dial("tcp", tcpAddr.String())

		// if it is not tls and an instancename was provided, then use my PROXY protocol
		if instanceName != "" {
			// send my PROXY preamble
			var buffer bytes.Buffer

			buffer.WriteString(tunnel.ProxyString)
			// Write the length of the instancename to the buffer
			buffer.WriteByte(byte(len(instanceName)))
			buffer.WriteString(instanceName)
			buffer.WriteByte('\n')

			_, err = backConn.Write(buffer.Bytes())
			if err != nil {
				log.Errorf("session: [%s], failed to write PROXY header [%s]", tcpAddr.String(), err.Error())
				return err
			}
		}
	}
	if err != nil {
		log.Errorf("session: [%s], failed to connect to [%s]", tcpAddr.String(), err.Error())
		return err
	}

	go io.Copy(backConn, os.Stdin)
	io.Copy(os.Stdout, backConn)

	return nil
}
