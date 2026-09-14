package reverselb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/caddyserver/certmagic"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	"go.uber.org/zap"
)

func testApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("REVLB_TEST_TOKEN", "test-only-token")
	a := &App{
		RuntimeID: "test",
		Control:   Control{Listen: []string{"127.0.0.1:9999"}, TLS: ControlTLS{ServerName: "localhost"}},
		Users: map[string]User{"default@none": {
			TokenEnv: "REVLB_TEST_TOKEN", Registration: Registration{ServicePatterns: []string{"*"}},
		}},
	}
	a.defaults()
	return a
}

func TestDynamicAllocation(t *testing.T) {
	a := testApp(t)
	sel := tunnelcore.Selector{UserID: "default@none", Service: "ssh"}
	td := protocol.TunnelData{ServiceName: "ssh", Token: "test-only-token"}
	port, err := a.addEndpoint(sel, td)
	if err != nil || port != 8000 {
		t.Fatalf("allocate: port=%d err=%v", port, err)
	}
	again, err := a.addEndpoint(sel, td)
	if err != nil || again != port || len(a.Generated) != 1 {
		t.Fatalf("reuse: port=%d err=%v endpoints=%d", again, err, len(a.Generated))
	}
	td.FrontendData.Port = 8001
	if _, err := a.addEndpoint(sel, td); err == nil {
		t.Fatal("accepted conflicting port for same service")
	}
	sel.Service = "web"
	td.ServiceName = "web"
	if p, err := a.addEndpoint(sel, td); err != nil || p != 8001 {
		t.Fatalf("explicit port: %d %v", p, err)
	}
	sel.Service = "other"
	if _, err := a.addEndpoint(sel, td); err == nil {
		t.Fatal("accepted occupied port")
	}
	td.FrontendData.Port = 9000
	if _, err := a.addEndpoint(sel, td); err == nil {
		t.Fatal("accepted port outside pool")
	}
}

func TestAuthorizationAndTemplates(t *testing.T) {
	a := testApp(t)
	for _, td := range []protocol.TunnelData{
		{ServiceName: "test", Token: "wrong"},
		{ServiceName: "test:instance:extra", Token: "test-only-token"},
		{ServiceName: "", Token: "test-only-token"},
	} {
		if _, err := a.authorize(td); err == nil {
			t.Fatalf("accepted invalid identity %q", td.ServiceName)
		}
	}
	sel := tunnelcore.Selector{UserID: "default@none", Service: "web", Instance: "first"}
	if _, err := a.addEndpoint(sel, protocol.TunnelData{}); err != nil {
		t.Fatal(err)
	}
	sel.Instance = "second"
	if _, err := a.addEndpoint(sel, protocol.TunnelData{}); err == nil {
		t.Fatal("direct template accepted multiple instances")
	}
}

func TestRenderAndOwnership(t *testing.T) {
	a := testApp(t)
	sel := tunnelcore.Selector{UserID: "default@none", Service: "web"}
	a.Publication.Templates["raw-tcp"] = Template{Kind: "http", InstanceDispatch: "direct"}
	if _, err := a.addEndpoint(sel, protocol.TunnelData{}); err != nil {
		t.Fatal(err)
	}
	apps := document{"http": json.RawMessage(`{"servers":{"operator":{"listen":["127.0.0.1:1234"],"routes":[]}}}`)}
	if err := mergeGenerated(apps, a, nil); err != nil {
		t.Fatal(err)
	}
	var generated struct {
		Servers map[string]json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(apps["http"], &generated); err != nil {
		t.Fatal(err)
	}
	if _, ok := generated.Servers["operator"]; !ok {
		t.Fatal("operator server removed")
	}
	var ep Endpoint
	for _, ep = range a.Generated {
	}
	var server map[string]any
	if err := json.Unmarshal(generated.Servers[serverName(a.RuntimeID, ep.ID)], &server); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(server["listen"], []any{"0.0.0.0:8000"}) {
		t.Fatalf("wrong listener: %v", server["listen"])
	}
	if err := mergeGenerated(apps, a, a.Generated); err != nil {
		t.Fatalf("idempotent render: %v", err)
	}
	generated.Servers[serverName(a.RuntimeID, ep.ID)] = json.RawMessage(`{"listen":[":8001"]}`)
	changed, _ := json.Marshal(generated)
	apps["http"] = changed
	if err := mergeGenerated(apps, a, a.Generated); err == nil {
		t.Fatal("overwrote externally modified server")
	}
}

func TestPublicationRebasesETagConflict(t *testing.T) {
	a := testApp(t)
	var config []byte
	gets, posts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/config/" {
			t.Errorf("unexpected API path %s", req.URL.Path)
		}
		if req.Method == http.MethodGet {
			gets++
			w.Header().Set("ETag", `"revision"`)
			w.Write(config)
			return
		}
		posts++
		if req.Header.Get("If-Match") != `"revision"` {
			t.Error("publication did not use If-Match")
		}
		if posts == 1 {
			var root map[string]any
			json.Unmarshal(config, &root)
			root["logging"] = map[string]any{"logs": map[string]any{"default": map[string]any{"level": "DEBUG"}}}
			config, _ = json.Marshal(root)
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		var root map[string]any
		if err := json.NewDecoder(req.Body).Decode(&root); err != nil {
			t.Error(err)
		}
		if root["logging"] == nil {
			t.Error("lost concurrent operator configuration")
		}
		config, _ = json.Marshal(root)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	a.Publication.AdminEndpoint = server.URL
	config, _ = json.Marshal(map[string]any{"apps": map[string]any{"goreverselb": a}})
	r, err := newRuntime(a, zap.NewNop(), &certmagic.FileStorage{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Destruct()
	err = r.update(context.Background(), func(current *App) error {
		_, err := current.addEndpoint(tunnelcore.Selector{UserID: "default@none", Service: "web"}, protocol.TunnelData{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if gets != 2 || posts != 2 {
		t.Fatalf("expected rebase; GET=%d POST=%d", gets, posts)
	}
}

func TestAdminEndpointRestrictions(t *testing.T) {
	for _, endpoint := range []string{"http://example.com:2019", "https://localhost:2019", "http://user:pass@localhost:2019", "http://localhost:2019/other"} {
		if _, err := adminClient(endpoint); err == nil {
			t.Errorf("accepted unsafe admin endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:2019", "http://[::1]:2019", "unix:///tmp/caddy-admin.sock"} {
		client, err := adminClient(endpoint)
		if err != nil {
			t.Errorf("%s: %v", endpoint, err)
		} else {
			client.client.CloseIdleConnections()
		}
	}
}

func TestFailedPublicationReservations(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown=%v", unknown), func(t *testing.T) {
			a := testApp(t)
			var config []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					w.Header().Set("ETag", `"revision"`)
					w.Write(config)
					return
				}
				if unknown {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			a.Publication.AdminEndpoint = server.URL
			config, _ = json.Marshal(map[string]any{"apps": map[string]any{"goreverselb": a}})
			r, err := newRuntime(a, zap.NewNop(), &certmagic.FileStorage{Path: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Destruct()
			err = r.update(context.Background(), func(current *App) error {
				_, err := current.addEndpoint(tunnelcore.Selector{UserID: "default@none", Service: "example"}, protocol.TunnelData{})
				return err
			})
			if err == nil {
				t.Fatal("reported a failed publication as successful")
			}
			if unknown && len(r.intents) != 1 {
				t.Fatal("lost reservation after unknown publication outcome")
			}
			if !unknown && len(r.intents) != 0 {
				t.Fatal("definitively rejected publication retained reservation")
			}
		})
	}
}

func TestAutomaticAllocationSkipsConfiguredPorts(t *testing.T) {
	a := testApp(t)
	apps := document{"http": json.RawMessage(`{"servers":{"existing":{"listen":["127.0.0.1:8000"]}}}`)}
	a.unavailablePorts = configuredPorts(nil, apps, a)
	port, err := a.addEndpoint(tunnelcore.Selector{UserID: "default@none", Service: "new"}, protocol.TunnelData{})
	if err != nil || port != 8001 {
		t.Fatalf("allocated occupied port: %d %v", port, err)
	}
}
