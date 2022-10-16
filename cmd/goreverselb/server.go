package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/dariopb/goreverselb/pkg/certHelper"
	"github.com/dariopb/goreverselb/pkg/helpers"
	pubsub "github.com/dariopb/goreverselb/pkg/pubsub"
	restapi "github.com/dariopb/goreverselb/pkg/restapi"
	version "github.com/dariopb/goreverselb/version"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

func printVersion() {
	log.Info(fmt.Sprintf("goreverseLB version: %v", version.Version))
	log.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
}

// server
var port int
var loglevelstr string
var autocertsubjectname string
var httpport int
var natsport int

// client
var lbapiendpoint string
var serviceendpoint string
var insecuretls bool
var servicename string
var instancename string
var token string
var frontendport int
var dynport, dynportcount int

var banner = `
                                  _     ____  
 _ __ _____   _____ _ __ ___  ___| |   | __ ) 
| '__/ _ \ \ / / _ \ '__/ __|/ _ \ |   |  _ \ 
| | |  __/\ V /  __/ |  \__ \  __/ |___| |_) )
|_|  \___| \_/ \___|_|  |___/\___|_____|____/ 
`

func main() {
	banner = strings.ReplaceAll(banner, "#", "`")
	fmt.Println(banner)

	app := &cli.App{
		EnableBashCompletion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "loglevel",
				Aliases:     []string{"l"},
				Value:       "info",
				Usage:       "debug level, one of: info, debug",
				EnvVars:     []string{"LOGLEVEL"},
				Destination: &loglevelstr,
			},
			&cli.StringFlag{
				Name:        "token",
				Aliases:     []string{"t"},
				Usage:       "shared secret for authorization",
				EnvVars:     []string{"TOKEN"},
				Destination: &token,
				Required:    true,
			},
		},
		Commands: []*cli.Command{
			{
				Name: "server",
				//Aliases: []string{"server"},
				Usage:  "runs as a server",
				Action: server,

				Flags: []cli.Flag{
					&cli.IntFlag{
						Name:        "port",
						Aliases:     []string{"p"},
						Value:       0,
						Usage:       "port for the API endpoint",
						EnvVars:     []string{"PORT"},
						Destination: &port,
						Required:    true,
					},

					&cli.StringFlag{
						Name:        "autocertsubjectname",
						Aliases:     []string{"s"},
						Value:       "",
						Usage:       "subject name for the autogenerated certificate",
						EnvVars:     []string{"AUTO_CERT_SUBJECT_NAME"},
						Destination: &autocertsubjectname,
						Required:    true,
					},
					&cli.IntFlag{
						Name:        "httpport",
						Value:       0,
						Usage:       "port for the HTTP rest endpoint (server will be disabled if not provided)",
						EnvVars:     []string{"HTTP_PORT"},
						Destination: &httpport,
						Required:    false,
					},
					&cli.IntFlag{
						Name:        "natsport",
						Value:       0,
						Usage:       "port for the secure NATS endpoint (server will be disabled if not provided)",
						EnvVars:     []string{"NATS_PORT"},
						Destination: &natsport,
						Required:    false,
					},
					&cli.IntFlag{
						Name:        "dynport",
						Value:       8000,
						Usage:       "dynamic frontend port base",
						EnvVars:     []string{"DYN_FRONTEND_PORT"},
						Destination: &dynport,
						Required:    false,
					},
					&cli.IntFlag{
						Name:        "dynportcount",
						Value:       100,
						Usage:       "number of dynamic frontend ports",
						EnvVars:     []string{"DYN_FRONTEND_PORT_COUNT"},
						Destination: &dynportcount,
						Required:    false,
					},
				},
			},
			{
				Name: "tunnel",
				//Aliases: []string{"tunnel"},
				Usage:  "creates an ingress tunnel",
				Action: client,

				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:        "apiendpoint",
						Aliases:     []string{"e"},
						Value:       "",
						Usage:       "API endpoint in the form: hostname:port",
						EnvVars:     []string{"LB_API_ENDPOINT"},
						Destination: &lbapiendpoint,
						Required:    true,
					},
					&cli.IntFlag{
						Name:        "frontendport",
						Aliases:     []string{"p"},
						Value:       0,
						DefaultText: "auto",
						Usage:       "frontend port where the service is going to be exposed (endpoint will be apiendpoint:serviceport)",
						EnvVars:     []string{"PORT"},
						Destination: &frontendport,
						Required:    false,
					},

					&cli.StringFlag{
						Name:        "serviceendpoint",
						Aliases:     []string{"b"},
						Value:       "",
						Usage:       "backend service address (the local target for the lb: hostname:port)",
						EnvVars:     []string{"SERVICE_ENDPOINT"},
						Destination: &serviceendpoint,
						Required:    true,
					},
					&cli.StringFlag{
						Name:        "servicename",
						Aliases:     []string{"s"},
						Usage:       "service name string",
						EnvVars:     []string{"SERVICE_NAME"},
						Destination: &servicename,
						Required:    true,
					},
					&cli.StringFlag{
						Name:        "instancename",
						Value:       "",
						DefaultText: "empty",
						Usage:       "instance name string (for SNI/Host functionality)",
						EnvVars:     []string{"INSTANCE_NAME"},
						Destination: &instancename,
						Required:    false,
					},
					&cli.BoolFlag{
						Name:        "insecuretls",
						Aliases:     []string{"i"},
						Value:       false,
						Usage:       "allow skip checking server CA/hostname",
						EnvVars:     []string{"INSECURE_TLS"},
						Destination: &insecuretls,
						Required:    false,
					},
				},
			},
			{
				Name: "stdinproxy",
				//Aliases: []string{"tunnel"},
				Usage:  "creates an stdin/stdout proxy to the endpoint",
				Action: stdinProxy,

				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:        "serviceendpoint",
						Aliases:     []string{"e"},
						Value:       "",
						Usage:       "backend service address (hostname:port)",
						EnvVars:     []string{"SERVICE_ENDPOINT"},
						Destination: &serviceendpoint,
						Required:    true,
					},
				},
			},
		},

		Name:  "goreverselb",
		Usage: "create tunnel proxies and load balance traffic between them",
		//Action: goreverselbserver,
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func server(ctx *cli.Context) error {
	printVersion()

	loglevel := log.InfoLevel
	if l, err := log.ParseLevel(loglevelstr); err == nil {
		loglevel = l
	}

	//log.AddHook(ProcessCounter)
	//log.SetFormatter(&log.TextFormatter{ForceColors: true})
	log.SetLevel(loglevel)
	log.SetOutput(os.Stdout)

	// Initialize/retrieve the persistent runtime data
	configData := &tunnel.ConfigData{}
	store, err := helpers.NewAppData("user_store.yaml")
	store.LoadConfig(configData, false, func() (interface{}, error) {
		configData.Users = map[string]*tunnel.UserData{
			tunnel.DefaultUserID: {UserID: "default@none",
				AllowedSources: "*"},
		}

		return configData, nil
	})
	if err != nil {
		log.Fatal("failed to load user configuration: ", err)
	}

	configData.Users[tunnel.DefaultUserID].Token = token
	store.Save()

	cert, err := certHelper.CreateDynamicTlsCertWithKey(autocertsubjectname)
	if err != nil {
		log.Fatalf("failed to create tls certificate: ", err)
	}

	ts, err := tunnel.NewMuxTunnelService(configData, *cert, port, token, dynport, dynportcount)
	if err != nil {
		log.Fatalf("failed to start new tunnel service: ", err)
	}

	if natsport != 0 {
		pubsub.NewNatsServer(*cert, natsport, token)
	} else {
		log.Debug("natsport not provided, not starting NATS server")
	}

	if httpport != 0 {
		restapi.NewRestApi(*cert, httpport, token, ts)
	} else {
		log.Debug("httpport not provided, not starting HTTP server")
	}

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	<-c

	ts.Close()

	return err
}
