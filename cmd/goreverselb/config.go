package main

// Config holds all CLI configuration parameters
type Config struct {
	// Global options
	LogLevel string
	Token    string

	// Server options
	Port                int
	AutoCertSubjectName string
	HTTPPort            int
	NATSPort            int
	DynPort             int
	DynPortCount        int

	// Client/Tunnel options
	APIEndpoint     string
	FrontendPort    int
	WrapTLS         bool
	WrapSSH         bool
	ServiceEndpoint string
	ServiceName     string
	InstanceName    string
	InsecureTLS     bool

	// TunnelGroup options
	ServiceGroupJSON string
}

// Global config instance populated by CLI flags
var cfg Config
