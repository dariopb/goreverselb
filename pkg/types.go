package tunnel

import (
	"crypto/tls"
	"net"
	"sync"
	"time"

	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	log "github.com/sirupsen/logrus"
)

var DefaultUserID string = "default@none"

const (
	ProxyString       string = "PROXY->"
	HttpConnectString string = "CONNECT "
)

type ConfigData struct {
	Users map[string]*UserData `yaml:"users" json:"users"`
}
type UserData struct {
	UserID   string                  `yaml:"userId" json:"userId"`
	Token    string                  `yaml:"token" json:"token"`
	Role     string                  `yaml:"role" json:"role"`
	Services map[string]*ServiceData `yaml:"services" json:"services"`

	AllowedSources string `yaml:"allowedSources" json:"allowedSources"`
}

type ServiceData struct {
	ServiceAndInstanceName string `yaml:"serviceAndInstanceName" json:"serviceAndInstanceName"`
	Token                  string `yaml:"token" json:"token"`
	AllowedSources         string `yaml:"allowedSources" json:"allowedSources"`
	Persistent             bool   `yaml:"persistent" json:"persistent"`
}

type backendRuntimeData struct {
	conn         net.Conn
	frontendConn net.Conn
}

type frontendRuntimeData struct {
	serviceName        string
	port               int
	listener           net.Listener
	lastBackendRemoval time.Time
	backendConnMap     map[string]*backendRuntimeData
}

type FrontendData = protocol.FrontendData
type TunnelData = protocol.TunnelData
type TunnelDataResponse = protocol.TunnelDataResponse

type TunnelService struct {
	Port      int `yaml:"port" json:"port"`
	token     string
	tlsconfig *tls.Config

	frontendMap map[string]*frontendRuntimeData
	mtx         sync.Mutex
}

type TunnelConnecData = protocol.TunnelConnecData

func (frd *frontendRuntimeData) removeBackendConn(id string) {
	log.Debugf("frontend removeBackendConn for: [%s]", id)

	delete(frd.backendConnMap, id)
	frd.lastBackendRemoval = time.Now()
}

func readFrame(conn net.Conn) ([]byte, int, error) {
	b, err := protocol.ReadFrame(conn)
	return b, len(b), err
}

func sendSerializedObject(conn net.Conn, obj interface{}) error {
	return protocol.WriteObject(conn, obj)
}
