package tunnel

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var DefaultUserID string = ""

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

type FrontendData struct {
	Port int `yaml:"port" json:"port"`
	auto bool
}

type TunnelData struct {
	ID             string       `yaml:"id" json:"id"`
	ServiceName    string       `yaml:"serviceName" json:"serviceName"`
	Token          string       `yaml:"token" json:"token"`
	AllowedSources string       `yaml:"allowedSources" json:"allowedSources"`
	FrontendData   FrontendData `yaml:"frontendData" json:"frontendData"`

	BackendAcceptBacklog int
	TargetPort           int
	TargetAddresses      []string
}

type TunnelDataResponse struct {
	ID              string `yaml:"id" json:"id"`
	ServiceName     string `yaml:"serviceName" json:"serviceName"`
	FrontendPort    int    `yaml:"frontendPort" json:"frontendPort"`
	FrontendAddress string `yaml:"frontendAddress" json:"frontendAddress"`
	Error           string `yaml:"error" json:"error"`
}

type TunnelService struct {
	Port      int `yaml:"port" json:"port"`
	token     string
	tlsconfig *tls.Config

	frontendMap map[string]*frontendRuntimeData
	mtx         sync.Mutex
}

type TunnelConnecData struct {
	ID            string `yaml:"id" json:"id"`
	ServiceName   string `yaml:"serviceName" json:"serviceName"`
	SourceAddress string `yaml:"sourceAddress" json:"sourceAddress"`
}

func (frd *frontendRuntimeData) removeBackendConn(id string) {
	log.Debugf("frontend removeBackendConn for: [%s]", id)

	delete(frd.backendConnMap, id)
	frd.lastBackendRemoval = time.Now()
}

func readFrame(conn net.Conn) ([]byte, int, error) {
	b := make([]byte, 1000)
	l, err := conn.Read(b[0:2])
	if err != nil || l != 2 {
		return nil, 0, fmt.Errorf("Bad frame format")
	}
	payloadLen := binary.LittleEndian.Uint16(b[0:2])
	if payloadLen > uint16(len(b)) {
		return nil, 0, fmt.Errorf("payload len too long: [%d]", payloadLen)
	}

	l, err = conn.Read(b[0:payloadLen])
	if err != nil || uint16(l) != payloadLen {
		return nil, 0, fmt.Errorf("Not enought contiguous data")
	}

	return b[:payloadLen], int(payloadLen), nil
}

func sendSerializedObject(conn net.Conn, obj interface{}) error {
	b, err := json.Marshal(obj)
	if err != nil {
		log.Errorf("failed to serialize data [%s]", err.Error())
		return err
	}

	payloadLen := uint16(len(b))
	l := make([]byte, 2)
	binary.LittleEndian.PutUint16(l, payloadLen)

	ll, err := conn.Write(l)
	if uint16(ll) != 2 {
		return err
	}
	ll, err = conn.Write(b)
	if uint16(ll) != payloadLen {
		return err
	}

	return nil
}
