# Reverse tunnel Load Balancer
```
                                  _     ____  
 _ __ _____   _____ _ __ ___  ___| |   | __ ) 
| '__/ _ \ \ / / _ \ '__/ __|/ _ \ |   |  _ \ 
| | |  __/\ V /  __/ |  \__ \  __/ |___| |_) )
|_|  \___| \_/ \___|_|  |___/\___|_____|____/ 
```


reverselb is a L4 reverse tunnel and load balancer: it creates an encrypted TLS tunnel to an external ingress that in turns receives requests on the specified port and forwards the traffic to the client via multiplexed sessions. Since it operates at L4 (tcp only for now), it allows to tunnel almost every protocol that runs on top of TCP (plain TCP, HTTP, HTTPS, WS, MQTT, etc). It is inspired on services such as [ngrok](http://ngrok.com) and [inlets](https://github.com/inlets/inlets): those are great services but they either have limits or don't support TCP tunnels (and k8s LoadBalance for them) in their free tiers.

There is a client and a server components. The server is intended to be run on a machine/container that has a publicly accessible endpoint (there is an Azure ACI sample template for quick deployment) while the client runs on the private network and configures the service that needs to be accesible externally.
The client (either via the cmd line application or library) makes an outbound connection to the reverselb server and configures the tunnel properties. The server now starts listening on the port the client instructed it to and when connections are received on this port, it relays the data back and forth between it and the backend connection. As many connections as desired can be established.

# Console application

```
NAME:
   goreverselb - create tunnel proxies and load balance traffic between them

USAGE:
   goreverselb [global options] command [command options] [arguments...]

COMMANDS:
   server   runs as a server
   tunnel   creates an ingress tunnel
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --loglevel value, -l value  debug level, one of: info, debug (default: "info") [$LOGLEVEL]
   --token value, -t value     shared secret for authorization (default: "info") [$TOKEN]
   --help, -h                  show help (default: false)

```

# Server

```
NAME:
   goreverselb [global options] server - runs as a server

USAGE:
   goreverselb server [command options] [arguments...]

OPTIONS:
   --port value, -p value                 port for the API endpoint (default: 9999) [$PORT]
   --autocertsubjectname value, -s value  subject name for the autogenerated certificate (default: "localhost") [$AUTO_CERT_SUBJECT_NAME]
   --help, -h                             show help (default: false)
```

# Client

```
NAME:
   goreverselb tunnel - creates an ingress tunnel

USAGE:
   goreverselb tunnel [command options] [arguments...]

OPTIONS:
   --apiendpoint value, -e value      API endpoint in the form: hostname:port [$LB_API_ENDPOINT]
   --frontendport value, -p value     frontend port where the service is going to be exposed (endpoint will be apiendpoint:serviceport (default: 8888) [$PORT]
   --serviceendpoint value, -b value  backend service endpoint (the local target for the lb: hostname:port) [$SERVICE_ENDPOINT]
   --servicename value, -s value      service name string (default: "xxxx") [$SERVICE_NAME]
   --insecuretls, -i                  allow skip checking server CA/hostname (default: false) [$INSECURE_TLS]
   --help, -h                         show help (default: false)
```


# Run it!

Linux 

````
cd cmd/goreverselb
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build

# create a tunnel server on the local machine
./goreverselb -t "0000" server -p 9999 -s "localhost"

# register a forwarder to some random endpoint
./goreverselb -t "0000" tunnel --apiendpoint localhost:9999 --servicename "myendpoint-8888" --serviceendpoint ip.jsontest.com:80 --frontendport 8888 --insecuretls=true

# now hit the frontend on the port defined

curl --header "Host: ip.jsontest.com" http://localhost:8888
{"ip": "173.69.143.190"}

[notice that we need to override the host header since the tunnel is a plain TCP one so hitting servers that rely on the host header to find the target won't work without doing so]
````

Docker (either linux, mac if you really want to try this way)

````
# server
docker run -it --rm --net host -e "PORT=9999" -e "TOKEN=0000" -e "AUTO_CERT_SUBJECT_NAME=localhost" dariob/reverselb-alpine:latest ./goreverselb server

INFO[0000] goreverseLB version: latest
INFO[0000] Go Version: go1.13.5
INFO[0000] Go OS/Arch: linux/amd64
INFO[0000] CreateDynamicTlsCertWithKey: creating new tls cert for SN: [localhost]
INFO[0001] tunnel service listening on: tcp => [::]:9999


# client
docker run -it --rm --net host -e "LB_API_ENDPOINT=localhost:9999" -e "TOKEN=0000" -e "SERVICE_NAME=my service" -e "PORT=8888" -e "SERVICE_ENDPOINT=ip.jsontest.com:80" -e "INSECURE_TLS=true" dariob/reverselb-alpine:latest ./goreverselb tunnel

INFO[0000] goreverseLB version: latest
INFO[0000] Go Version: go1.13.5
INFO[0000] Go OS/Arch: linux/amd64
INFO[2020-01-12T18:21:40Z] NewTunnelClient to apiEndpoint [localhost:9999] with tunnel info: [{ my service 0000 {8888} 1 80 [ip.jsontest.com]}]


````

Raspberry PI (comming...) 

# Kubernetes LoadBalancer Operator

You can expose you kubernetes pods externally via a LoadBalancer operator available here: 

# Docker

Create docker image:

````
cd cmd/goreverselb
env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -v -o goreverselb
cd ../..
sudo docker build -f docker/Dockerfile-alpine.txt -t dariob/reverselb-alpine .
sudo docker tag dariob/reverselb-alpine dariob/reverselb-alpine:0.1
sudo docker tag dariob/reverselb-alpine dariob/reverselb-alpine:latest
sudo docker push dariob/reverselb-alpine:latest
sudo docker push dariob/reverselb-alpine:0.1
````


# Raspberry PI 

````
cd cmd/goreverselb
env CGO_ENABLED=0 GOARCH=arm GOARM=5 GOOS=linux go build -o goreverselb

sudo docker build -f docker/Dockerfile-pi.txt -t dariob/reverselb-pi .
sudo docker tag dariob/reverselb-pi dariob/reverselb-pi:0.1
sudo docker tag dariob/reverselb-pi dariob/reverselb-pi:latest
sudo docker push dariob/reverselb-pi:latest
sudo docker push dariob/reverselb-pi:0.1

````

# Azure ACI ingress sample

You can deploy a cheap entry point using Azure ACI container services (create a [free account](https://azure.microsoft.com/en-us/free/) if you don't have one to evaluate).
Create a new template deployment using the template below (make sure that dnsNameLabel and the autocertsubjectname strings match the name and region where you are deploying).

```json
{
    "$schema": "https://schema.management.azure.com/schemas/2015-01-01/deploymentTemplate.json#",
    "contentVersion": "1.0.0.0",
    "parameters": {
        "containerGroupName": {
          "type": "string",
          "defaultValue": "reverselbdefaultname",
          "metadata": {
            "description": "reverseLB server"
          }
        },
        "containerImageName": {
            "type": "string",
            "defaultValue": "dariob/reverselb-alpine:latest",
            "metadata": {
              "description": "reverseLB image"
            }
          }
  
          ,"port": {
            "type": "string",
            "defaultValue": "9999",
            "metadata": {
              "description": "API endpoint port"
            }
          }
          ,"token": {
            "type": "string",
            "defaultValue": "",
            "metadata": {
                "description": "shared secret for authorization"
            }
        }
        ,"autocertsubjectname": {
            "type": "string",
            "defaultValue": "reverselb-123.westus2.azurecontainer.io",
            "metadata": {
                "description": "subject name for the autogenerated certificate"
            }
        }
        ,"dnsNameLabel": {
            "type": "string",
            "defaultValue": "reverselb-123",
            "metadata": {
                "description": "Dns name prefix for pod"
            }
        }
        ,"loglevel": {
            "type": "string",
            "defaultValue": "debug",
            "metadata": {
                "description": "loglevel"
            }
        }
        ,"portserv1": {
            "type": "string",
            "defaultValue": "8888",
            "metadata": {
              "description": "service port 1"
            }
          }
          ,"portserv2": {
            "type": "string",
            "defaultValue": "8889",
            "metadata": {
              "description": "service port 1"
            }
          }
    },
    "variables": {
        "reverselbimage": "dariob/reverselb-alpine:latest"
    },
    "resources": [
        {
            "name": "[parameters('containerGroupName')]",
            "type": "Microsoft.ContainerInstance/containerGroups",
            "apiVersion": "2018-10-01",
            "location": "[resourceGroup().location]",
            "properties": {
                "containers": [
                    {
                        "name": "reverselbdefaultname",
                        "properties": {
                            "image": "[parameters('containerImageName')]",
                            "environmentVariables": [
                                {
                                    "name": "PORT",
                                    "value": "[parameters('port')]"
                                },
                                {
                                    "name": "LOGLEVEL",
                                    "value": "[parameters('loglevel')]"
                                },
                                {
                                    "name": "TOKEN",
                                    "value": "[parameters('token')]"
                                },
                                {
                                    "name": "AUTO_CERT_SUBJECT_NAME",
                                    "value": "[parameters('autocertsubjectname')]"
                                },
                                {
                                    "name": "OWN_CONTAINER_ID",
                                    "value": "[resourceId('Microsoft.ContainerInstance/containerGroups', parameters('containerGroupName'))]"
                                }
                            ],
                            "resources": {
                                "requests": {
                                    "cpu": 1,
                                    "memoryInGb": 1
                                }
                            },
                            "ports": [
                                {
                                    "port": "[parameters('port')]"
                                }
                                ,{
                                    "port": "[parameters('portserv1')]"
                                }
                                ,{
                                    "port": "[parameters('portserv2')]"
                                }
                            ]
                        }
                    }

                ],
                "osType": "Linux",
                "ipAddress": {
                    "type": "Public",
                    "ports": [
                      {
                        "protocol": "tcp",
                        "port": "[parameters('port')]"
                      }
                      ,{
                        "protocol": "tcp",
                        "port": "[parameters('portserv1')]"
                      }
                      ,{
                        "protocol": "tcp",
                        "port": "[parameters('portserv2')]"
                      }
                    ],
                    "dnsNameLabel": "[parameters('dnsNameLabel')]"
                }
            }
        }
    ]
}
```

# Embedding the client

The client can be embedded in your app if the backend mappings are dynamic like in the case of a load balancer controller (see the kubernetes LoadBalancer repo for more):

```go
   ...
	td := tunnel.TunnelData{
		ServiceName:          "web8888",
		Token:                "1234",
		BackendAcceptBacklog: 1,
		FrontendData: tunnel.FrontendData{
			Port: 8888,
		},
		TargetPort:      80,
		TargetAddresses: []string{"www.google.com"},
	}

   tc, err := tunnel.NewMuxTunnelClient("localhost:9999", td)
   ...

   tc.Close()

```

