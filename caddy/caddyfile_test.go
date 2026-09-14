package reverselb

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
)

func TestDynamicCaddyfile(t *testing.T) {
	input, err := os.ReadFile("examples/dynamic.Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		t.Fatal("standard Caddyfile adapter not registered")
	}
	data, _, err := adapter.Adapt(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, cfg, err := decodeDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Generated) != 0 || len(cfg.Bindings) != 0 {
		t.Fatal("dynamic configuration must not need static endpoints")
	}
	if cfg.Publication.PortStart != 8000 || cfg.Publication.PortCount != 100 ||
		cfg.Publication.Templates["plain-http"].Kind != "http" {
		t.Fatalf("unexpected publication configuration: %+v", cfg.Publication)
	}
	raw, err := os.ReadFile("examples/dynamic.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _, expected, err := decodeDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(expected)
	got, _ := json.Marshal(cfg)
	if !jsonEqual(want, got) {
		t.Fatalf("Caddyfile/JSON policy mismatch:\n%s\n%s", want, got)
	}
	t.Run("Caddyfile HTTP-only policy", func(t *testing.T) { assertHTTPOnlySample(t, data) })
	t.Run("JSON HTTP-only policy", func(t *testing.T) { assertHTTPOnlySample(t, raw) })
}

func assertHTTPOnlySample(t *testing.T, data []byte) {
	t.Helper()
	root, apps, cfg, err := decodeDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	var admin struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(root["admin"], &admin); err != nil {
		t.Fatal(err)
	}
	if admin.Listen != "0.0.0.0:2020" || cfg.Publication.AdminEndpoint != "http://127.0.0.1:2020" {
		t.Fatal("sample must expose admin on all IPv4 interfaces while keeping controller requests local")
	}
	if cfg.Publication.DefaultTemplate != "plain-http" || len(cfg.Publication.Rules) != 0 || len(cfg.Publication.Templates) != 1 {
		t.Fatal("sample must default to a single HTTP template")
	}
	template := cfg.Publication.Templates["plain-http"]
	if template.Kind != "http" || template.FrontendTLS {
		t.Fatal("sample frontend must be plaintext HTTP")
	}
	var tlsPolicy struct {
		Automation struct {
			Policies []struct {
				Issuers []struct {
					Module string `json:"module"`
				} `json:"issuers"`
			} `json:"policies"`
		} `json:"automation"`
	}
	if err := json.Unmarshal(apps["tls"], &tlsPolicy); err != nil {
		t.Fatal(err)
	}
	if len(tlsPolicy.Automation.Policies) != 1 || len(tlsPolicy.Automation.Policies[0].Issuers) != 1 ||
		tlsPolicy.Automation.Policies[0].Issuers[0].Module != "internal" {
		t.Fatal("sample control TLS must use only the local issuer, not ACME")
	}
	var pki struct {
		CAs map[string]struct {
			InstallTrust *bool `json:"install_trust"`
		} `json:"certificate_authorities"`
	}
	if err := json.Unmarshal(apps["pki"], &pki); err != nil {
		t.Fatal(err)
	}
	if trust := pki.CAs["local"].InstallTrust; trust == nil || *trust {
		t.Fatal("sample must not install the CA into system trust stores")
	}
	sel := tunnelcore.Selector{UserID: "default@none", Service: "example"}
	if _, err := cfg.addEndpoint(sel, protocol.TunnelData{}); err != nil {
		t.Fatal(err)
	}
	for _, ep := range cfg.Generated {
		app, raw, err := renderEndpoint(cfg.RuntimeID, ep)
		if err != nil {
			t.Fatal(err)
		}
		var server struct {
			AutomaticHTTPS struct {
				Disable bool `json:"disable"`
			} `json:"automatic_https"`
			TLSConnectionPolicies []json.RawMessage `json:"tls_connection_policies"`
		}
		if err := json.Unmarshal(raw, &server); err != nil {
			t.Fatal(err)
		}
		if app != "http" || !server.AutomaticHTTPS.Disable || len(server.TLSConnectionPolicies) != 0 {
			t.Fatal("sample publication must generate HTTP without HTTPS automation")
		}
	}
}

func TestCaddyfileRejectsUnknownOptions(t *testing.T) {
	_, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte("{\n goreverselb {\n typo value\n }\n}\n"), nil)
	if err == nil {
		t.Fatal("unknown global option accepted")
	}
}
