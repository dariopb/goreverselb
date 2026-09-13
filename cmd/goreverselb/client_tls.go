package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/urfave/cli/v2"
)

func clientTLSFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name: "insecuretls", Aliases: []string{"i"}, Value: true,
			Usage:   "legacy control TLS compatibility: skip certificate verification (set false for verified TLS)",
			EnvVars: []string{"REVLB_INSECURE_TLS"}, Destination: &cfg.InsecureTLS,
		},
		&cli.StringFlag{
			Name: "tlscafile", Usage: "PEM CA bundle for control TLS (enables verification; conflicts with explicit insecuretls=true)",
			EnvVars: []string{"REVLB_TLS_CA_FILE"}, Destination: &cfg.TLSCAFile,
		},
		&cli.StringFlag{
			Name: "tlsservername", Usage: "control TLS certificate name (enables verification; conflicts with explicit insecuretls=true)",
			EnvVars: []string{"REVLB_TLS_SERVER_NAME"}, Destination: &cfg.TLSServerName,
		},
	}
}

func clientOptions(ctx *cli.Context) (tunnel.ClientOptions, error) {
	insecure := cfg.InsecureTLS
	if cfg.TLSCAFile != "" || cfg.TLSServerName != "" {
		if ctx.IsSet("insecuretls") && insecure {
			return tunnel.ClientOptions{}, fmt.Errorf("insecuretls=true cannot be combined with tlscafile or tlsservername")
		}
		insecure = false
	}
	config := &tls.Config{InsecureSkipVerify: insecure, ServerName: cfg.TLSServerName}
	if cfg.TLSCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return tunnel.ClientOptions{}, fmt.Errorf("read TLS CA file: %w", err)
		}
		config.RootCAs = x509.NewCertPool()
		if !config.RootCAs.AppendCertsFromPEM(pem) {
			return tunnel.ClientOptions{}, fmt.Errorf("TLS CA file contains no certificates")
		}
	}
	return tunnel.ClientOptions{TLSConfig: config}, nil
}
