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

	tsg, err := tunnel.NewMuxTunnelClientServiceGroup(lbapiendpoint, token)
	if err != nil {
		log.Fatalf("failed to start new tunnel service group: ", err)
	}

	err = tsg.ReconcileServiceGroupFromJSON(servicemapjson)
	if err != nil {
		log.Fatalf("failed to reconcile service group: ", err)
	}

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	<-c

	return nil
}
