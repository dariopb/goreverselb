//go:generate $GOPATH/bin/statik -f -src=./web

package restapi

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"runtime"

	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/labstack/echo/v4"
	echolog "github.com/onrik/logrus/echo"
	log "github.com/sirupsen/logrus"

	_ "github.com/dariopb/goreverselb/pkg/restapi/statik"
	"github.com/rakyll/statik/fs"
)

type RestAPI struct {
	echo *echo.Echo
	ts   *tunnel.MuxTunnelService
}

func NewRestApi(cert tls.Certificate, port int, token string, ts *tunnel.MuxTunnelService) (RestAPI, error) {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	api := RestAPI{
		echo: e,
		ts:   ts,
	}

	// Routes
	e.GET("/help", help)
	e.GET("/services", api.getServices)
	e.GET("/services/:name", api.getServices)
	//e.Static("/", "web")

	statikFS, err := fs.New()
	if err != nil {
		log.Fatal(err)
	}
	assetHandler := http.FileServer(statikFS) //http.StripPrefix("/", http.FileServer(statikFS))
	e.GET("/:rrr", echo.WrapHandler(assetHandler))
	e.GET("/", echo.WrapHandler(assetHandler))

	e.Logger = echolog.NewLogger(log.StandardLogger(), "")
	//e.Use(middleware.Logger())

	log.Infof("Starting echo REST API on port %d", port)

	go func() {
		//err := e.StartTLS(fmt.Sprintf(":%d", port), cert.Certificate, xxxx)
		err := e.Start(fmt.Sprintf(":%d", port))
		if err != nil {
			log.Errorf("Start echo failed with [%s]", err.Error())
			panic(err.Error())
		}
	}()

	return api, nil
}

func (r RestAPI) Close() {
	if r.echo != nil {
		r.echo.Close()
		r.echo = nil
	}
}

func help(c echo.Context) error {
	hostname, _ := os.Hostname()
	banner := fmt.Sprintf("reverselb alive on [%s] (%s on %s/%s). Available routes: ",
		hostname, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	routes := c.Echo().Routes()
	for _, route := range routes {
		banner = banner + "<li>" + route.Path + "</li> "
	}

	return c.HTML(http.StatusOK, banner)
}

func (api *RestAPI) getServices(c echo.Context) error {
	name := c.Param("name")
	var ret interface{}

	services := api.ts.GetServices()
	ret = services
	if name != "" {
		for k, v := range services {
			if name == k {
				ret = v
				break
			}
		}
	}

	if ret == nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Service [%s] not found", name))
	}

	return c.JSON(http.StatusOK, ret)
}
