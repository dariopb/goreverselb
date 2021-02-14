package main

import (
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/urfave/cli/v2"

	log "github.com/sirupsen/logrus"
)

func client(ctx *cli.Context) error {
	printVersion()

	loglevel := log.DebugLevel
	if l, err := log.ParseLevel(loglevelstr); err == nil {
		loglevel = l
	}

	//log.AddHook(ProcessCounter)
	//log.SetFormatter(&log.TextFormatter{ForceColors: true})
	log.SetFormatter(&log.TextFormatter{
		//DisableColors: true,
		FullTimestamp: true,
	})
	log.SetLevel(loglevel)
	log.SetOutput(os.Stdout)

	h, p, err := net.SplitHostPort(serviceendpoint)
	if err != nil {
		log.Fatalf("wrong format for endpoint: ", err)
	}

	port, _ := strconv.Atoi(p)
	if err != nil {
		log.Fatalf("wrong format for endpoint: ", err)
	}

	if len(instancename) > 0 {
		servicename = servicename + ":" + instancename
	}

	td := tunnel.TunnelData{
		ServiceName:          servicename,
		Token:                token,
		BackendAcceptBacklog: 1,
		FrontendData: tunnel.FrontendData{
			Port: frontendport,
		},
		TargetPort:      port,
		TargetAddresses: []string{h},
	}

	tunnel.NewMuxTunnelClient(lbapiendpoint, td)

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	<-c

	return nil
}
