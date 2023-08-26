package tunnel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	log "github.com/sirupsen/logrus"
)

type MuxTunnelClientServiceGroup struct {
	apiEndpoint string
	token       string

	ServiceMap map[string]*ServiceInfo
	tunnels    map[string]map[string]*MuxTunnelClient

	mtx sync.Mutex
}

type ServiceInfo struct {
	Name       string     `json:"name"`
	Ports      []PortData `json:"ports"`
	BackendIPs []string   `json:"backendIPs"`

	Deleted bool `json:"deleted"`

	tunnelEndpointAPI string
	token             string
}

type PortData struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`

	TargetPort int `json:"targetPort"`
}

// NewMuxTunnelClientServiceGroup creates a group of multiple services/clients.
func NewMuxTunnelClientServiceGroup(apiEndpoint string, token string) (*MuxTunnelClientServiceGroup, error) {
	var err error

	log.Infof("NewMuxTunnelClientServiceGroup: on %s", apiEndpoint)
	c := &MuxTunnelClientServiceGroup{
		apiEndpoint: apiEndpoint,
		token:       token,

		ServiceMap: make(map[string]*ServiceInfo),
		tunnels:    make(map[string]map[string]*MuxTunnelClient),
	}

	return c, err
}

func (c *MuxTunnelClientServiceGroup) reconcileTunnels(srv *ServiceInfo) {
	var svcTunnelMap map[string]*MuxTunnelClient
	ok := false

	if srv.tunnelEndpointAPI != "" {
		if svcTunnelMap, ok = c.tunnels[srv.Name]; !ok {
			svcTunnelMap = make(map[string]*MuxTunnelClient)
			c.tunnels[srv.Name] = svcTunnelMap
		}
	}

	portsGone := make(map[string]bool)
	for portKey := range svcTunnelMap {
		portsGone[portKey] = true
	}

	if !srv.Deleted {
		for _, p := range srv.Ports {
			k := fmt.Sprintf("%s-%d", p.Protocol, p.Port)

			var err error
			var t *MuxTunnelClient
			if t, ok = svcTunnelMap[k]; !ok {
				td := TunnelData{
					ServiceName:          srv.Name,
					BackendAcceptBacklog: 1,
					FrontendData: FrontendData{
						Port: p.Port,
					},
					Token:           srv.token,
					TargetPort:      p.TargetPort,
					TargetAddresses: []string{},
				}

				t, err = NewMuxTunnelClient(srv.tunnelEndpointAPI, td)
				if err != nil {
					continue
				}

				svcTunnelMap[k] = t
			}

			delete(portsGone, k)
			belist := t.TargetAddresses()
			if !reflect.DeepEqual(srv.BackendIPs, belist) {
				log.Info("Updating backend addresses on tunnel", "Service", srv.Name, "IPs", srv.BackendIPs)
				t.UpdateTargetAddresses(srv.BackendIPs)
			}

			beport := t.TargetPort()
			if beport != p.TargetPort {
				log.Info("Updating backend port on tunnel", "Service", srv.Name, "port", p.TargetPort)
				t.UpdateTargetPort(p.TargetPort)
			}
		}
	}

	// Removed ports, delete the tunnels
	for portKey := range portsGone {
		t := svcTunnelMap[portKey]
		t.Close()
		delete(svcTunnelMap, portKey)
	}
}

// ReconcileServiceGroup reconciles the map of services with a new map of services
func (c *MuxTunnelClientServiceGroup) ReconcileServiceGroup(newservices map[string]*ServiceInfo) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	srvGone := make(map[string]bool)
	for name := range c.ServiceMap {
		srvGone[name] = true
	}

	for _, newservice := range newservices {
		svcname := newservice.Name

		var srv *ServiceInfo
		var ok bool
		if srv, ok = c.ServiceMap[svcname]; !ok {
			srv = &ServiceInfo{
				Name:       svcname,
				BackendIPs: newservice.BackendIPs,
				Ports:      newservice.Ports,

				tunnelEndpointAPI: c.apiEndpoint,
				token:             c.token,
			}

			c.ServiceMap[svcname] = srv
		} else {
			delete(srvGone, svcname)
		}

		c.reconcileTunnels(srv)
	}

	// Removed services, delete the tunnels
	for name := range srvGone {
		srv := c.ServiceMap[name]
		srv.Deleted = true
		c.reconcileTunnels(srv)
		delete(c.ServiceMap, name)
	}

	return nil
}

// ReconcileServiceGroupFromJSON reconciles the map of services with a new map of services
// {"name":"srv-1234","ports":[{"port":8000,"protocol":"tcp"}],"backendIPs":["127.0.0.1"],"deleted":false}
func (c *MuxTunnelClientServiceGroup) ReconcileServiceGroupFromJSON(jsonstr string) error {
	var newservices map[string]*ServiceInfo
	err := json.Unmarshal([]byte(jsonstr), &newservices)
	if err != nil {
		return err
	}

	return c.ReconcileServiceGroup(newservices)
}
