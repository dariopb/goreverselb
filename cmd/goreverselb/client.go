package main

import (
	"fmt"
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
		return fmt.Errorf("wrong format for endpoint: %w", err)
	}

	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid backend port %q", p)
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

	options, err := clientOptions(ctx)
	if err != nil {
		return err
	}
	tc, err := tunnel.NewMuxTunnelClientWithOptions(cfg.APIEndpoint, td, options)
	if err != nil {
		return err
	}
	defer tc.Close()

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(c)

	<-c

	return nil
}
