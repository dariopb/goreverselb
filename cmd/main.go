package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/dariopb/goreverselb/pkg/certHelper"
	"github.com/dariopb/goreverselb/pkg/restapi"

	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

func main() {
	fmt.Println("goreverselb starting...")

	port := 9999
	if p, err := strconv.Atoi(os.Getenv("PORT")); err == nil {
		port = p
	}

	loglevel := log.InfoLevel
	if l, err := log.ParseLevel(os.Getenv("LOGLEVEL")); err == nil {
		loglevel = l
	}

	//log.AddHook(ProcessCounter)
	log.SetFormatter(&logrus.TextFormatter{ForceColors: true})
	log.SetLevel(loglevel)
	log.SetOutput(os.Stdout)

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	cert, err := certHelper.CreateDynamicTlsCertWithKey("localhost")
	if err != nil {
		log.Fatalf("failed to create tls certificate: ", err)
	}

	//pubsub.NewNatsServer(*cert, port, "1234")
	//<-c
	//tunnel.NewTunnelService(*cert, port, "1234")
	ts, _ := tunnel.NewMuxTunnelService(*cert, port, "1234", 8000, 2)

	restapi.NewRestApi(*cert, 7777, "1234", ts)

	time.Sleep(1 * time.Second)

	td := tunnel.TunnelData{
		ServiceName:          "web8888",
		Token:                "1234",
		BackendAcceptBacklog: 1,
		FrontendData: tunnel.FrontendData{
			Port: 8000,
		},
		TargetPort:      80,
		TargetAddresses: []string{"www.google.com"},
	}

	td1 := tunnel.TunnelData{
		ServiceName: "web888899:",
		Token:       "1234",
		FrontendData: tunnel.FrontendData{
			Port: 0,
		},
		TargetPort:      80,
		TargetAddresses: []string{"www.microsoft.com"},
	}

	tc, _ := tunnel.NewMuxTunnelClient("localhost:9999", td)
	time.Sleep(1 * time.Second)
	_, _ = tunnel.NewMuxTunnelClient("localhost:9999", td1)

	time.Sleep(10000000 * time.Second)
	tc.Close()

	time.Sleep(10 * time.Second)
	tunnel.NewMuxTunnelClient("localhost:9999", td1)

	time.Sleep(10000 * time.Second)

	td.FrontendData.Port = 7777
	tc, _ = tunnel.NewMuxTunnelClient("localhost:9999", td)

	<-c
}
