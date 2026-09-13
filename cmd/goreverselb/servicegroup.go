package main

import (
	"os"
	"os/signal"
	"syscall"

	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/urfave/cli/v2"

	log "github.com/sirupsen/logrus"
)

func servicegroup(ctx *cli.Context) error {
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

	options, err := clientOptions(ctx)
	if err != nil {
		return err
	}
	tsg, err := tunnel.NewMuxTunnelClientServiceGroupWithOptions(cfg.APIEndpoint, cfg.Token, options)
	if err != nil {
		return err
	}
	defer tsg.Close()

	err = tsg.ReconcileServiceGroupFromJSON(cfg.ServiceGroupJSON)
	if err != nil {
		return err
	}

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(c)

	<-c

	return nil
}
