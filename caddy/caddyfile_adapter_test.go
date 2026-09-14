package reverselb

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	tunnel "github.com/dariopb/goreverselb/pkg"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
)

func adaptCertificateCaddyfile(t *testing.T, input string) []byte {
	t.Helper()
	data, _, err := caddyconfig.GetAdapter("goreverselb-caddyfile").Adapt([]byte(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func certificateFileLoaders(t *testing.T, data []byte) caddytls.FileLoader {
	t.Helper()
	_, apps, cfg, err := decodeDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CertificateFiles) != 0 {
		t.Fatal("adapter-only certificate fields leaked into the runtime app")
	}
	var tlsApp struct {
		Certificates struct {
			Files caddytls.FileLoader `json:"load_files"`
		} `json:"certificates"`
	}
	if err := json.Unmarshal(apps["tls"], &tlsApp); err != nil {
		t.Fatal(err)
	}
	return tlsApp.Certificates.Files
}

func TestCertificateCaddyfileAdapter(t *testing.T) {
	base, err := os.ReadFile("examples/dynamic.Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Replace(string(base), "goreverselb {", `goreverselb {
		certificate "/certs/cert with spaces.pem" "/keys/key with spaces.pem"
		certificate /certs/second.pem /keys/second.pem
		certificate /certs/second.pem /keys/second.pem`, 1)
	data := adaptCertificateCaddyfile(t, input)
	files := certificateFileLoaders(t, data)
	if len(files) != 2 || files[0].Certificate != "/certs/cert with spaces.pem" ||
		files[0].Key != "/keys/key with spaces.pem" || files[1].Certificate != "/certs/second.pem" {
		t.Fatalf("incorrect certificate files: %+v", files)
	}
	_, apps, _, err := decodeDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	if apps["http"] != nil || apps["layer4"] != nil {
		t.Fatal("loading certificates created static listeners")
	}
	assertHTTPOnlySample(t, data)
	original, _, err := caddyconfig.GetAdapter("caddyfile").Adapt(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := adaptCertificateCaddyfile(t, string(base)); !bytes.Equal(got, original) {
		t.Fatal("adapter changed a Caddyfile without certificate directives")
	}
	standard, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, unconsumed, err := decodeDocument(standard)
	if err != nil {
		t.Fatal(err)
	}
	if err := unconsumed.Validate(); err == nil || !strings.Contains(err.Error(), "--adapter goreverselb-caddyfile") {
		t.Fatalf("wrong adapter must fail with actionable guidance: %v", err)
	}
}

func TestCertificateCaddyfileCoexistsWithRegularSites(t *testing.T) {
	base, err := os.ReadFile("examples/dynamic.Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	site := "\nhttps://normal.example.test:8443 {\n tls /certs/site.pem /keys/site.pem\n respond \"regular\"\n}\n"
	original, _, err := caddyconfig.GetAdapter("caddyfile").Adapt(append(base, []byte(site)...), nil)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Replace(string(base), "goreverselb {", `goreverselb {
		certificate /certs/site.pem /keys/site.pem
		certificate /certs/tunnel.pem /keys/tunnel.pem`, 1) + site
	data := adaptCertificateCaddyfile(t, input)
	files, originalFiles := certificateFileLoaders(t, data), certificateFileLoaders(t, original)
	if len(files) != 2 || len(originalFiles) != 1 || !reflect.DeepEqual(files[0], originalFiles[0]) {
		t.Fatalf("existing loader or its certificate-selection tags were lost: %+v", files)
	}
	_, oldApps, _, _ := decodeDocument(original)
	_, newApps, _, _ := decodeDocument(data)
	if !jsonEqual(oldApps["http"], newApps["http"]) || !jsonEqual(oldApps["pki"], newApps["pki"]) {
		t.Fatal("certificate adapter changed regular sites or PKI")
	}
	var oldTLS, newTLS document
	json.Unmarshal(oldApps["tls"], &oldTLS)
	json.Unmarshal(newApps["tls"], &newTLS)
	delete(oldTLS, "certificates")
	delete(newTLS, "certificates")
	oldJSON, _ := json.Marshal(oldTLS)
	newJSON, _ := json.Marshal(newTLS)
	if !jsonEqual(oldJSON, newJSON) {
		t.Fatal("certificate adapter changed existing TLS policy")
	}
}

func TestCertificateCaddyfileRejectsInvalidArguments(t *testing.T) {
	for _, directive := range []string{
		"certificate", "certificate cert.pem", "certificate cert.pem key.pem extra",
		`certificate "" key.pem`, `certificate cert.pem ""`,
	} {
		input := "{\n goreverselb {\n" + directive + "\n}\n}\n"
		if _, _, err := caddyconfig.GetAdapter("goreverselb-caddyfile").Adapt([]byte(input), nil); err == nil {
			t.Errorf("accepted invalid directive %q", directive)
		}
	}
}

func TestCertificateCaddyfileExamplePort(t *testing.T) {
	t.Setenv("REVLB_TOKEN", "certificate-example-token")
	t.Setenv("REVLB_CERT_FILE", "/certs/fullchain.pem")
	t.Setenv("REVLB_KEY_FILE", "/keys/privkey.pem")
	input, err := os.ReadFile("examples/certificates.Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	_, _, a, err := decodeDocument(adaptCertificateCaddyfile(t, string(input)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	name, template := a.templateFor(tunnelcore.Selector{UserID: "default@none", Service: "multi-1"})
	if a.Control.Listen[0] != ":9000" || name != "multi-tls" || !template.FrontendTLS ||
		template.TLSServerName != "multi-1.apps.cloudexmaquina.com" {
		t.Fatal("example does not configure the requested control listener and TLS hostname")
	}
	port, err := a.addEndpoint(tunnelcore.Selector{UserID: "default@none", Service: "multi-1"},
		protocol.TunnelData{FrontendData: protocol.FrontendData{Port: 7445, TLSWrap: true}})
	if err != nil || port != 7445 {
		t.Fatalf("example must support explicitly requested frontend port 7445: %d %v", port, err)
	}
}

func TestCertificateCaddyfileDynamicTLS(t *testing.T) {
	t.Setenv("REVLB_TOKEN", "certificate-test-token")
	// Caddy's process-wide certificate cache can outlive Stop between test runs.
	domain := fmt.Sprintf("cert-%d.example.test", time.Now().UnixNano())
	cert, key := certificateForNames(t, []string{"*." + domain})
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "full chain.pem"), filepath.Join(dir, "private key.pem")
	for path, content := range map[string]string{certFile: cert, keyFile: key} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("REVLB_CERT_FILE", certFile)
	t.Setenv("REVLB_KEY_FILE", keyFile)
	input, err := os.ReadFile("examples/certificates.Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	data := adaptCertificateCaddyfile(t, strings.ReplaceAll(string(input), "apps.cloudexmaquina.com", domain))
	if files := certificateFileLoaders(t, data); len(files) != 1 || files[0].Certificate != certFile || files[0].Key != keyFile {
		t.Fatal("environment-expanded certificate paths were not preserved")
	}
	root, apps, a, err := decodeDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	control, admin, frontend := freePort(t), freePort(t), freePort(t)
	a.Control.Listen = []string{fmt.Sprintf("127.0.0.1:%d", control)}
	a.Publication.AdminEndpoint = fmt.Sprintf("http://127.0.0.1:%d", admin)
	a.Publication.BindHost = "127.0.0.1"
	a.Publication.PortStart, a.Publication.PortCount = frontend, 1
	root["admin"], _ = json.Marshal(map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", admin), "config": map[string]any{"persist": false}})
	root["storage"], _ = json.Marshal(map[string]any{"module": "file_system", "root": filepath.Join(dir, "storage")})
	apps["goreverselb"], _ = json.Marshal(a)
	root["apps"], _ = json.Marshal(apps)
	data, _ = json.Marshal(root)
	if err := caddy.Load(data, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { caddy.Stop() })
	eventually(t, "certificate runtime activated", func() bool {
		mod, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
		return err == nil && mod.(*App).runtime.active.Load() == mod
	})
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(cert))
	tlsConfig := &tls.Config{ServerName: "multi-1." + domain, RootCAs: roots, MinVersion: tls.VersionTLS12}
	expectedPair, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "certificate-tunnel")
	}))
	defer backend.Close()
	client, err := tunnel.NewMuxTunnelClientWithOptions(a.Control.Listen[0], tunnel.TunnelData{
		ServiceName: "multi-1", Token: "certificate-test-token",
		FrontendData: tunnel.FrontendData{Port: frontend, TLSWrap: true},
		TargetPort:   backend.Listener.Addr().(*net.TCPAddr).Port, TargetAddresses: []string{"127.0.0.1"},
	}, tunnel.ClientOptions{TLSConfig: tlsConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var shared *runtime
	eventually(t, "file certificate and dynamic TLS frontend", func() bool {
		mod, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
		if err != nil || mod.(*App).runtime.active.Load() == nil {
			return false
		}
		shared = mod.(*App).runtime
		active := shared.active.Load()
		return len(active.Generated) == 1 && active.tlsApp.HasCertificateForSubject(tlsConfig.ServerName) &&
			shared.registry.Count(tunnelcore.Selector{UserID: "default@none", Service: "multi-1"}) == 1
	})
	get := func() {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp",
			fmt.Sprintf("127.0.0.1:%d", frontend), tlsConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if !bytes.Equal(conn.ConnectionState().PeerCertificates[0].Raw, expectedPair.Certificate[0]) {
			t.Fatal("frontend did not serve the configured file certificate")
		}
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", tlsConfig.ServerName); err != nil {
			t.Fatal(err)
		}
		response, err := io.ReadAll(conn)
		if err != nil || !bytes.Contains(response, []byte("certificate-tunnel")) {
			t.Fatalf("TLS-wrapped backend response: %q %v", response, err)
		}
	}
	get()
	api, err := adminClient(a.Publication.AdminEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer api.client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	activeJSON, _, status, err := api.request(ctx, http.MethodGet, nil, "")
	cancel()
	if err != nil || status != http.StatusOK {
		t.Fatalf("read active certificate configuration: %d %v", status, err)
	}
	if len(certificateFileLoaders(t, activeJSON)) != 1 || bytes.Contains(activeJSON, []byte("PRIVATE KEY")) {
		t.Fatal("active config must retain file paths, not key material")
	}
	if err := caddy.Load(activeJSON, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "certificate reload activated", func() bool {
		mod, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
		return err == nil && mod.(*App).runtime == shared && shared.active.Load() == mod
	})
	get()
	for name, contents := range map[string]string{
		"malformed certificate":  "not a PEM certificate",
		"mismatched certificate": func() string { cert, _ := certificate(t); return cert }(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(certFile, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := caddy.Load(activeJSON, true); err == nil {
				t.Fatal("invalid certificate files accepted")
			}
			get()
		})
	}
	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(activeJSON, true); err == nil {
		t.Fatal("missing certificate accepted")
	}
	get()
	if err := os.WriteFile(certFile, []byte(cert), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("invalid-key", 8)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(activeJSON, true); err == nil {
		t.Fatal("invalid private key accepted")
	}
	get()
	rotatedCert, rotatedKey := certificateForNames(t, []string{"*." + domain})
	for path, content := range map[string]string{certFile: rotatedCert, keyFile: rotatedKey} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	expectedPair, err = tls.X509KeyPair([]byte(rotatedCert), []byte(rotatedKey))
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.RootCAs = x509.NewCertPool()
	tlsConfig.RootCAs.AppendCertsFromPEM([]byte(rotatedCert))
	// Reload the adapted source policy, not an admin snapshot of generated routes.
	if err := caddy.Load(data, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rotated certificate activated", func() bool {
		mod, err := caddy.ActiveContext().AppIfConfigured("goreverselb")
		if err != nil || mod.(*App).runtime != shared || shared.active.Load() != mod {
			return false
		}
		for _, ep := range mod.(*App).Generated {
			for binding := range ep.Bindings {
				if shared.available(binding) {
					return true
				}
			}
		}
		return false
	})
	get()
}
