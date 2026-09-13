// Package protocol defines the control JSON and length-prefixed data metadata
// shared by standalone and embedded tunnel hosts.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const MaxFrameSize = 1000

type FrontendData struct {
	Port    int  `yaml:"port" json:"port"`
	TLSWrap bool `yaml:"tlsWrap" json:"tlsWrap"`
	SSHWrap bool `yaml:"sshWrap" json:"sshWrap"`
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
	PublicationMode string `yaml:"publicationMode,omitempty" json:"publicationMode,omitempty"`
}

type TunnelConnecData struct {
	ID            string `yaml:"id" json:"id"`
	ServiceName   string `yaml:"serviceName" json:"serviceName"`
	SourceAddress string `yaml:"sourceAddress" json:"sourceAddress"`
}

func ReadFrame(r io.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("read frame header: %w", err)
	}
	n := int(binary.LittleEndian.Uint16(header[:]))
	if n > MaxFrameSize {
		return nil, fmt.Errorf("frame payload %d exceeds maximum %d", n, MaxFrameSize)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("read frame payload: %w", err)
	}
	return payload, nil
}

func WriteObject(w io.Writer, obj any) error {
	payload, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("frame payload %d exceeds maximum %d", len(payload), MaxFrameSize)
	}
	frame := make([]byte, 2+len(payload))
	binary.LittleEndian.PutUint16(frame, uint16(len(payload)))
	copy(frame[2:], payload)
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if n < 0 || n > len(frame) {
			return io.ErrShortWrite
		}
		frame = frame[n:]
		if err != nil {
			return fmt.Errorf("write frame: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
