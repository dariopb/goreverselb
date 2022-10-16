module github.com/dariopb/goreverselb

go 1.13

//replace github.com/dariopb/goreverselb => ../goreverselb

require (
	github.com/hashicorp/yamux v0.0.0-20190923154419-df201c70410d
	github.com/labstack/echo/v4 v4.1.13
	github.com/nats-io/gnatsd v1.4.1 // indirect
	github.com/nats-io/nats-server v1.4.1 // indirect
	github.com/nats-io/nats-streaming-server v0.16.2
	github.com/nats-io/nats.go v1.8.1
	github.com/onrik/logrus v0.4.1
	github.com/pkg/errors v0.8.1
	github.com/rakyll/statik v0.1.6
	github.com/sirupsen/logrus v1.4.2
	github.com/urfave/cli/v2 v2.1.1
	golang.org/x/crypto v0.0.0-20191227163750-53104e6ec876
	gopkg.in/yaml.v2 v2.3.0
)
