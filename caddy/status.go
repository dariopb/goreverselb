package reverselb

import (
	"encoding/json"
	"net/http"

	"github.com/caddyserver/caddy/v2"
)

func init() { caddy.RegisterModule(adminStatus{}) }

type adminStatus struct{}

func (adminStatus) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "admin.api.goreverselb", New: func() caddy.Module { return new(adminStatus) }}
}

func (adminStatus) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{{
		Pattern: "/goreverselb/status",
		Handler: caddy.AdminHandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
			if req.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return nil
			}
			type state struct {
				RuntimeID string `json:"runtime_id"`
				Sessions  int    `json:"sessions"`
				Endpoints int    `json:"endpoints"`
				Pending   int    `json:"pending_publications"`
			}
			states := make([]state, 0)
			runtimes.Range(func(_, value any) bool {
				r := value.(*runtime)
				a := r.active.Load()
				if a != nil {
					r.mu.Lock()
					sessions := len(r.entries)
					r.mu.Unlock()
					states = append(states, state{a.RuntimeID, sessions, len(a.Generated), len(r.jobs)})
				}
				return true
			})
			w.Header().Set("Content-Type", "application/json")
			return json.NewEncoder(w).Encode(states)
		}),
	}}
}

var _ caddy.AdminRouter = (*adminStatus)(nil)
