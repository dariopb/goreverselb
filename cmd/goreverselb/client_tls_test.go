package main

import (
	"testing"

	"github.com/urfave/cli/v2"
)

func TestClientTLSFlags(t *testing.T) {
	for _, command := range []string{"tunnel", "tunnelgroup"} {
		for _, test := range []struct {
			name     string
			args     []string
			insecure bool
			wantErr  bool
		}{
			{"legacy default", nil, true, false},
			{"verified system roots", []string{"--insecuretls=false"}, false, false},
			{"server name enables verification", []string{"--tlsservername=tunnel.test"}, false, false},
			{"conflicting settings", []string{"--tlsservername=tunnel.test", "--insecuretls=true"}, false, true},
			{"missing CA", []string{"--tlscafile=/does/not/exist"}, false, true},
		} {
			t.Run(command+"/"+test.name, func(t *testing.T) {
				cfg = Config{}
				app := &cli.App{Commands: []*cli.Command{{
					Name: command, Flags: clientTLSFlags(),
					Action: func(ctx *cli.Context) error {
						options, err := clientOptions(ctx)
						if (err != nil) != test.wantErr {
							t.Fatalf("options error: %v", err)
						}
						if err == nil && options.TLSConfig.InsecureSkipVerify != test.insecure {
							t.Fatalf("insecure=%v", options.TLSConfig.InsecureSkipVerify)
						}
						return nil
					},
				}}}
				args := append([]string{"goreverselb", command}, test.args...)
				if err := app.Run(args); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
