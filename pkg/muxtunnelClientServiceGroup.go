package tunnel

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sync"

	log "github.com/sirupsen/logrus"
)

type MuxTunnelClientServiceGroup struct {
	apiEndpoint string
	token       string
	options     ClientOptions
	closed      bool

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

	TargetPort int  `json:"targetPort"`
	SSHWrap    bool `json:"sshWrap"`
	TLSWrap    bool `json:"tlsWrap"`
}

// NewMuxTunnelClientServiceGroup creates a group of multiple services/clients.
func NewMuxTunnelClientServiceGroup(apiEndpoint string, token string) (*MuxTunnelClientServiceGroup, error) {
	return NewMuxTunnelClientServiceGroupWithOptions(apiEndpoint, token, LegacyClientOptions())
}

// NewMuxTunnelClientServiceGroupWithOptions applies the same control TLS policy
// to every managed client. Zero options enable certificate verification.
func NewMuxTunnelClientServiceGroupWithOptions(apiEndpoint string, token string, options ClientOptions) (*MuxTunnelClientServiceGroup, error) {
	if _, _, err := net.SplitHostPort(apiEndpoint); err != nil {
		return nil, err
	}
	options, err := normalizeClientOptions(options)
	if err != nil {
		return nil, err
	}
	log.Infof("NewMuxTunnelClientServiceGroup: on %s", apiEndpoint)
	c := &MuxTunnelClientServiceGroup{
		apiEndpoint: apiEndpoint,
		token:       token,
		options:     options,

		ServiceMap: make(map[string]*ServiceInfo),
		tunnels:    make(map[string]map[string]*MuxTunnelClient),
	}

	return c, err
}

func (c *MuxTunnelClientServiceGroup) reconcileTunnels(srv *ServiceInfo) error {
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
						Port:    p.Port,
						SSHWrap: p.SSHWrap,
						TLSWrap: p.TLSWrap,
					},
					Token:           srv.token,
					TargetPort:      p.TargetPort,
					TargetAddresses: srv.BackendIPs,
				}

				t, err = NewMuxTunnelClientWithOptions(srv.tunnelEndpointAPI, td, c.options)
				if err != nil {
					return fmt.Errorf("create tunnel for service %q: %w", srv.Name, err)
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
	return nil
}

// ReconcileServiceGroup reconciles the map of services with a new map of services
func (c *MuxTunnelClientServiceGroup) ReconcileServiceGroup(newservices map[string]*ServiceInfo) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	for name, srv := range newservices {
		if srv == nil || srv.Name == "" {
			return fmt.Errorf("service %q requires a name", name)
		}
	}

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
				Name:              svcname,
				tunnelEndpointAPI: c.apiEndpoint,
				token:             c.token,
			}

			c.ServiceMap[svcname] = srv
		} else {
			delete(srvGone, svcname)
		}
		srv.BackendIPs = append([]string(nil), newservice.BackendIPs...)
		srv.Ports = append([]PortData(nil), newservice.Ports...)
		srv.Deleted = newservice.Deleted

		if err := c.reconcileTunnels(srv); err != nil {
			return err
		}
	}

	// Removed services, delete the tunnels
	for name := range srvGone {
		srv := c.ServiceMap[name]
		srv.Deleted = true
		if err := c.reconcileTunnels(srv); err != nil {
			return err
		}
		delete(c.ServiceMap, name)
		delete(c.tunnels, name)
	}

	return nil
}

// Close stops all managed clients and prevents further reconciliation.
func (c *MuxTunnelClientServiceGroup) Close() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for _, service := range c.tunnels {
		for _, client := range service {
			client.Close()
		}
	}
	c.tunnels = make(map[string]map[string]*MuxTunnelClient)
	c.ServiceMap = make(map[string]*ServiceInfo)
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
