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
	if l, err := log.ParseLevel(cfg.LogLevel); err == nil {
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

	h, p, err := net.SplitHostPort(cfg.ServiceEndpoint)
	if err != nil {
		log.Fatalf("wrong format for endpoint: %v", err)
	}

	port, _ := strconv.Atoi(p)
	if err != nil {
		log.Fatalf("wrong format for endpoint: %v", err)
	}

	serviceName := cfg.ServiceName
	if len(cfg.InstanceName) > 0 {
		serviceName = serviceName + ":" + cfg.InstanceName
	}

	td := tunnel.TunnelData{
		ServiceName:          serviceName,
		Token:                cfg.Token,
		BackendAcceptBacklog: 1,
		FrontendData: tunnel.FrontendData{
			Port:    cfg.FrontendPort,
			TLSWrap: cfg.WrapTLS,
			SSHWrap: cfg.WrapSSH,
		},
		TargetPort:      port,
		TargetAddresses: []string{h},
	}

	tunnel.NewMuxTunnelClient(cfg.APIEndpoint, td)

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	<-c

	return nil
}
