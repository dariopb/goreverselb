package reverselb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

func TestDecodeDocumentErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"missing apps", `{}`, "no apps object"},
		{"null apps", `{"apps":null}`, "no apps object"},
		{"invalid apps", `{"apps":[]}`, "decode Caddy apps"},
		{"different Caddy", `{"apps":{"http":{}}}`, "no goreverselb app"},
		{"null app", `{"apps":{"goreverselb":null}}`, "no goreverselb app"},
		{"invalid app", `{"apps":{"goreverselb":{"control":{"listen":42}}}}`, "decode goreverselb config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := decodeDocument([]byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if tc.name == "invalid app" && strings.Contains(err.Error(), "no goreverselb app") {
				t.Fatal("decoding error was misreported as a missing app")
			}
		})
	}
}

func TestControllerRejectsDifferentCaddyInstance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Errorf("controller attempted to mutate another Caddy instance: %s", req.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("ETag", `"different-instance"`)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"apps":{"http":{"servers":{}}}}`))
	}))
	defer server.Close()
	a := testApp(t)
	a.Publication.AdminEndpoint = server.URL
	r, err := newRuntime(a, zap.NewNop(), &certmagic.FileStorage{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Destruct()
	err = r.update(context.Background(), func(*App) error {
		t.Error("mutation callback must not run against a different Caddy instance")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), server.URL) ||
		!strings.Contains(err.Error(), "publication.admin_endpoint") {
		t.Fatalf("expected error identifying the incorrect endpoint, got %v", err)
	}
}
