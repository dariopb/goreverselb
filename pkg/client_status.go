package tunnel

import (
	"context"
	"net"
	"strings"
	"time"
)

// ClientState describes a control connection, or the aggregate client state.
type ClientState string

const (
	ClientStateConnecting   ClientState = "connecting"
	ClientStateRegistering  ClientState = "registering"
	ClientStateReady        ClientState = "ready"
	ClientStateReconnecting ClientState = "reconnecting"
	ClientStateClosed       ClientState = "closed"
)

// ClientConnectionStatus describes one reconnecting control-connection worker.
// ID is a stable, zero-based index, not a yamux stream ID.
// Frontend fields describe an accepted registration and are empty unless ready.
type ClientConnectionStatus struct {
	ID              int         `json:"id"`
	State           ClientState `json:"state"`
	FrontendPort    int         `json:"frontendPort"`
	FrontendAddress string      `json:"frontendAddress,omitempty"`
	PublicationMode string      `json:"publicationMode,omitempty"`
	LastError       string      `json:"lastError,omitempty"`
	LastErrorAt     time.Time   `json:"lastErrorAt"`
	UpdatedAt       time.Time   `json:"updatedAt"`
}

// ClientStatus is a point-in-time snapshot, not a guarantee of backend health.
// State is ready when at least one connection has an accepted registration.
// Frontend fields come from the lowest-ID ready connection; see Connections for
// every response. They are zero/empty when no connection is ready.
type ClientStatus struct {
	State                ClientState              `json:"state"`
	Endpoint             string                   `json:"endpoint"`
	ServiceName          string                   `json:"serviceName"`
	DesiredConnections   int                      `json:"desiredConnections"`
	ConnectedConnections int                      `json:"connectedConnections"`
	ReadyConnections     int                      `json:"readyConnections"`
	FrontendPort         int                      `json:"frontendPort"`
	FrontendAddress      string                   `json:"frontendAddress,omitempty"`
	PublicationMode      string                   `json:"publicationMode,omitempty"`
	LastError            string                   `json:"lastError,omitempty"`
	LastErrorAt          time.Time                `json:"lastErrorAt"`
	UpdatedAt            time.Time                `json:"updatedAt"`
	Connections          []ClientConnectionStatus `json:"connections"`
}

// Status returns an independent snapshot safe to read or modify concurrently
// with reconnects, target updates, Close, and other Status/WaitReady calls.
// ConnectedConnections counts completed TLS handshakes, including connections
// awaiting registration. LastError is the most recent unresolved attempt error.
func (tc *MuxTunnelClient) Status() ClientStatus {
	tc.mtx.Lock()
	defer tc.mtx.Unlock()
	return tc.statusLocked()
}

func (tc *MuxTunnelClient) statusLocked() ClientStatus {
	status := ClientStatus{
		State: ClientStateReconnecting, Endpoint: tc.apiEndpoint,
		ServiceName: tc.tunnelData.ServiceName, DesiredConnections: len(tc.connections),
		Connections: append([]ClientConnectionStatus(nil), tc.connections...),
	}
	for _, conn := range status.Connections {
		switch conn.State {
		case ClientStateReady:
			if status.ReadyConnections == 0 {
				status.FrontendPort = conn.FrontendPort
				status.FrontendAddress = conn.FrontendAddress
				status.PublicationMode = conn.PublicationMode
			}
			status.ReadyConnections++
			status.ConnectedConnections++
			status.State = ClientStateReady
		case ClientStateRegistering:
			status.ConnectedConnections++
			if status.State != ClientStateReady {
				status.State = ClientStateRegistering
			}
		case ClientStateConnecting:
			if status.State == ClientStateReconnecting {
				status.State = ClientStateConnecting
			}
		}
		if conn.LastError != "" && (status.LastError == "" || conn.LastErrorAt.After(status.LastErrorAt)) {
			status.LastError, status.LastErrorAt = conn.LastError, conn.LastErrorAt
		}
		if conn.UpdatedAt.After(status.UpdatedAt) {
			status.UpdatedAt = conn.UpdatedAt
		}
	}
	if tc.statusClosed {
		status.State = ClientStateClosed
	}
	return status
}

// WaitReady waits for an accepted registration, including bindings-only mode
// where no frontend port is allocated. Transient failures continue to retry.
// Cancellation returns ctx.Err(); closing the client returns net.ErrClosed.
// The returned snapshot is populated on both success and failure.
func (tc *MuxTunnelClient) WaitReady(ctx context.Context) (ClientStatus, error) {
	for {
		tc.mtx.Lock()
		status, changed := tc.statusLocked(), tc.statusChanged
		tc.mtx.Unlock()
		if err := ctx.Err(); err != nil {
			return status, err
		}
		if status.State == ClientStateClosed {
			return status, net.ErrClosed
		}
		if status.State == ClientStateReady {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return tc.Status(), ctx.Err()
		case <-changed:
		}
	}
}

func (tc *MuxTunnelClient) setConnectionState(id int, state ClientState, err error) {
	tc.mtx.Lock()
	defer tc.mtx.Unlock()
	if tc.statusClosed {
		return
	}
	conn := &tc.connections[id]
	conn.State, conn.UpdatedAt = state, time.Now().UTC()
	conn.FrontendPort, conn.FrontendAddress, conn.PublicationMode = 0, "", ""
	if err != nil {
		conn.LastError, conn.LastErrorAt = redactClientError(err, tc.tunnelData.Token), conn.UpdatedAt
	}
	tc.notifyStatusLocked()
}

func (tc *MuxTunnelClient) notifyStatusLocked() {
	close(tc.statusChanged)
	tc.statusChanged = make(chan struct{})
}

func redactClientError(err error, token string) string {
	message := err.Error()
	if token != "" {
		message = strings.ReplaceAll(message, token, "[redacted]")
	}
	return message
}
