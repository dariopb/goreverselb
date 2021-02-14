package main

import (
	"io"
	"net"
	"os"
	"time"

	"github.com/urfave/cli/v2"

	log "github.com/sirupsen/logrus"
)

func stdinProxy(ctx *cli.Context) error {
	loglevel := log.DebugLevel
	if l, err := log.ParseLevel(loglevelstr); err == nil {
		loglevel = l
	}

	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	log.SetLevel(loglevel)
	log.SetOutput(os.Stdout)

	doProxy()

	//c := make(chan os.Signal, 2)
	//signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	//<-c

	return nil
}

func doProxy() error {
	tcpAddr, err := net.ResolveTCPAddr("tcp", serviceendpoint)
	if err != nil {
		log.Errorf("session: [%s], failed to resolve endpoint [%s]", tcpAddr.String(), err.Error())
		return err
	}

	d := net.Dialer{Timeout: 5 * time.Second}
	backConn, err := d.Dial("tcp", tcpAddr.String())
	if err != nil {
		log.Errorf("session: [%s], failed to connect to [%s]", tcpAddr.String(), err.Error())
		return err
	}

	go io.Copy(backConn, os.Stdin)
	io.Copy(os.Stdout, backConn)

	return nil
}
