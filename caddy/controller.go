package reverselb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
	"go.uber.org/zap"
)

type adminAPI struct {
	client *http.Client
	base   string
}

func adminClient(endpoint string) (*adminAPI, error) {
	u, err := localAdminURL(endpoint)
	if err != nil {
		return nil, err
	}
	t := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	base := strings.TrimRight(endpoint, "/")
	if u.Scheme == "unix" {
		socket := u.Path
		t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}
		base = "http://localhost"
	}
	return &adminAPI{
		client: &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("admin redirects are not allowed")
		}},
		base: base,
	}, nil
}

func (a *adminAPI) request(ctx context.Context, method string, data []byte, etag string) ([]byte, string, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+"/config/", bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, err
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", etag)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return nil, "", resp.StatusCode, err
	}
	if len(body) > 16<<20 {
		return nil, "", resp.StatusCode, errors.New("Caddy configuration exceeds 16 MiB limit")
	}
	return body, resp.Header.Get("ETag"), resp.StatusCode, nil
}

type document map[string]json.RawMessage

func decodeDocument(data []byte) (document, document, *App, error) {
	var root document
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, nil, fmt.Errorf("decode Caddy config: %w", err)
	}
	appsRaw, ok := root["apps"]
	if !ok || bytes.Equal(bytes.TrimSpace(appsRaw), []byte("null")) {
		return nil, nil, nil, errors.New("Caddy config has no apps object")
	}
	var apps document
	if err := json.Unmarshal(appsRaw, &apps); err != nil {
		return nil, nil, nil, fmt.Errorf("decode Caddy apps: %w", err)
	}
	appRaw, ok := apps["goreverselb"]
	if !ok || bytes.Equal(bytes.TrimSpace(appRaw), []byte("null")) {
		return nil, nil, nil, errors.New("active Caddy config has no goreverselb app; check that publication.admin_endpoint reaches this Caddy instance")
	}
	var a App
	if err := json.Unmarshal(appRaw, &a); err != nil {
		return nil, nil, nil, fmt.Errorf("decode goreverselb config: %w", err)
	}
	a.defaults()
	return root, apps, &a, nil
}

func (r *runtime) update(ctx context.Context, mutate func(*App) error) error {
	if err := r.loadJournal(ctx); err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		data, etag, status, err := r.admin.request(ctx, http.MethodGet, nil, "")
		if err != nil {
			return fmt.Errorf("read Caddy configuration: %w", err)
		}
		if status != http.StatusOK || etag == "" {
			return fmt.Errorf("Caddy config read returned %d or no ETag; enable the local admin API", status)
		}
		root, apps, current, err := decodeDocument(data)
		if err != nil {
			return fmt.Errorf("admin endpoint %s: %w", r.admin.base, err)
		}
		if current.runtimeKey() != r.key || current.Publication.Mode != "dynamic" {
			return errors.New("runtime publication policy has been replaced")
		}
		if err := current.Validate(); err != nil {
			return fmt.Errorf("active policy: %w", err)
		}
		current.unavailablePorts = configuredPorts(root, apps, current)
		current.recoverableLeases = r.intents
		for _, ep := range r.intents {
			current.unavailablePorts[ep.Port] = true
		}
		old := make(map[string]Endpoint, len(current.Generated))
		for id, ep := range current.Generated {
			ep.Bindings = cloneBindings(ep.Bindings)
			old[id] = ep
		}
		if err := mutate(current); err != nil {
			return err
		}
		if err := mergeGenerated(apps, current, old); err != nil {
			return err
		}
		if reflect.DeepEqual(old, current.Generated) {
			return nil
		}
		apps["goreverselb"], err = json.Marshal(current)
		if err != nil {
			return err
		}
		root["apps"], err = json.Marshal(apps)
		if err != nil {
			return err
		}
		candidate, err := json.Marshal(root)
		if err != nil {
			return err
		}
		// An intent survives a lost response. Native config remains authoritative.
		previousIntents := r.intents
		intent := make(map[string]Endpoint)
		for id, ep := range r.intents {
			intent[id] = ep
		}
		for id := range old {
			delete(intent, id)
		}
		for id, ep := range current.Generated {
			intent[id] = ep
		}
		if err := r.writeJournal(ctx, intent); err != nil {
			return err
		}
		_, _, status, err = r.admin.request(ctx, http.MethodPost, candidate, etag)
		if err != nil {
			return fmt.Errorf("publication outcome unknown; lease retained for reconciliation: %w", err)
		}
		if status == http.StatusPreconditionFailed {
			if err := r.writeJournal(ctx, previousIntents); err != nil {
				return err
			}
			continue
		}
		if status != http.StatusOK {
			if err := r.writeJournal(ctx, previousIntents); err != nil {
				return err
			}
			return fmt.Errorf("Caddy rejected publication (HTTP %d); previous configuration retained", status)
		}
		r.logger.Info("dynamic Caddy configuration committed",
			zap.String("runtime_id", r.id), zap.Int("endpoints", len(current.Generated)))
		return nil
	}
	return errors.New("Caddy configuration changed concurrently; publication retry limit reached")
}

func (r *runtime) writeJournal(ctx context.Context, endpoints map[string]Endpoint) error {
	data, err := json.Marshal(struct {
		Version   int                 `json:"version"`
		Endpoints map[string]Endpoint `json:"endpoints"`
	}{Version: 1, Endpoints: endpoints})
	if err != nil {
		return err
	}
	if err := r.storage.Store(ctx, "goreverselb/"+r.id+"/leases.json", data); err != nil {
		return fmt.Errorf("persist publication intent: %w", err)
	}
	r.intents = endpoints
	return nil
}

func (r *runtime) loadJournal(ctx context.Context) error {
	if r.journalLoaded {
		return nil
	}
	data, err := r.storage.Load(ctx, "goreverselb/"+r.id+"/leases.json")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("load publication journal: %w", err)
	}
	r.intents = make(map[string]Endpoint)
	if err == nil {
		var journal struct {
			Version   int                 `json:"version"`
			Endpoints map[string]Endpoint `json:"endpoints"`
		}
		if err := json.Unmarshal(data, &journal); err != nil || journal.Version != 1 {
			return errors.New("invalid or unsupported publication journal")
		}
		for id, ep := range journal.Endpoints {
			if id != ep.ID || ep.Port < 1 || ep.Port > 65535 {
				return errors.New("invalid endpoint in publication journal")
			}
			r.intents[id] = ep
		}
	}
	r.journalLoaded = true
	return nil
}

func (a *App) addEndpoint(sel tunnelcore.Selector, td protocol.TunnelData) (int, error) {
	p := a.Publication
	name, template := a.templateFor(sel)
	if td.FrontendData.TLSWrap != template.FrontendTLS {
		return 0, errors.New("requested TLSWrap does not match the selected template")
	}
	if td.FrontendData.SSHWrap != template.FrontendSSH {
		return 0, errors.New("requested SSHWrap does not match the selected template")
	}
	id := "e-" + stableID(a.RuntimeID, sel.UserID, sel.Service)
	binding := bindingID(sel)
	ep, exists := a.Generated[id]
	if exists {
		if td.FrontendData.Port != 0 && td.FrontendData.Port != ep.Port {
			return 0, fmt.Errorf("service already owns frontend port %d", ep.Port)
		}
		if ep.TemplateName != name || !reflect.DeepEqual(ep.Template, template) {
			return 0, errors.New("template differs from the active endpoint; remove its sessions before changing the template")
		}
		if _, ok := ep.Bindings[binding]; ok {
			return ep.Port, nil
		}
		if template.InstanceDispatch == "direct" && len(ep.Bindings) != 0 {
			return 0, errors.New("direct template permits only one distinct instance")
		}
	} else {
		used := make(map[int]bool)
		for port := range a.unavailablePorts {
			used[port] = true
		}
		count := 0
		for _, other := range a.Generated {
			used[other.Port] = true
			if other.UserID == sel.UserID {
				count++
			}
		}
		for leaseID, lease := range a.recoverableLeases {
			if _, exists := a.Generated[leaseID]; !exists && leaseID != id && lease.UserID == sel.UserID {
				count++
			}
		}
		limit := a.Users[sel.UserID].Registration.MaxEndpoints
		if limit == 0 {
			limit = 32
		}
		if count >= limit {
			return 0, errors.New("user endpoint quota reached")
		}
		port := td.FrontendData.Port
		if lease, ok := a.recoverableLeases[id]; ok {
			if lease.UserID != sel.UserID || lease.Service != sel.Service ||
				lease.BindHost != p.BindHost || !reflect.DeepEqual(lease.Template, template) {
				return 0, errors.New("recovered lease does not match the requested publication")
			}
			if port == 0 {
				port = lease.Port
			}
			if port != lease.Port {
				return 0, fmt.Errorf("unresolved previous publication reserves port %d", lease.Port)
			}
			// Only the same authenticated identity can recover an uncertain lease.
			delete(used, port)
			for _, other := range a.Generated {
				if other.Port == port {
					used[port] = true
				}
			}
		}
		if port == 0 {
			for candidate := p.PortStart; candidate < p.PortStart+p.PortCount; candidate++ {
				if !used[candidate] {
					port = candidate
					break
				}
			}
		}
		if port < p.PortStart || port >= p.PortStart+p.PortCount {
			return 0, errors.New("no port available or requested port is outside the pool")
		}
		if used[port] {
			return 0, errors.New("requested port belongs to another endpoint")
		}
		ep = Endpoint{ID: id, UserID: sel.UserID, Service: sel.Service, Port: port,
			BindHost: p.BindHost, TemplateName: name, Template: template,
			Bindings: make(map[string]tunnelcore.Selector)}
	}
	ep.Bindings = cloneBindings(ep.Bindings)
	ep.Bindings[binding] = sel
	refreshRevision(&ep)
	a.Generated[id] = ep
	return ep.Port, nil
}

func cloneBindings(src map[string]tunnelcore.Selector) map[string]tunnelcore.Selector {
	dst := make(map[string]tunnelcore.Selector, len(src))
	for id, sel := range src {
		dst[id] = sel
	}
	return dst
}

func refreshRevision(ep *Endpoint) {
	ep.Revision = ""
	data, _ := json.Marshal(ep)
	ep.Revision = stableID(string(data))
}

func serverName(runtimeID, endpointID string) string { return "revlb-" + runtimeID + "-" + endpointID }

func renderEndpoint(runtimeID string, ep Endpoint) (string, json.RawMessage, error) {
	t := ep.Template
	var routes []any
	ids := make([]string, 0, len(ep.Bindings))
	for id := range ep.Bindings {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		// The empty instance is an explicit last-resort route.
		a, b := ep.Bindings[ids[i]].Instance, ep.Bindings[ids[j]].Instance
		if a == "" {
			return false
		}
		if b == "" {
			return true
		}
		return a < b
	})
	if t.InstanceDispatch == "legacy" {
		bindings := make(map[string]string)
		for id, sel := range ep.Bindings {
			bindings[sel.Instance] = id
		}
		handles := make([]any, 0)
		if t.FrontendTLS {
			handles = append(handles, tlsHandler(t.TLSServerName))
		}
		handles = append(handles, map[string]any{"handler": "goreverselb_route", "bindings": bindings})
		route := map[string]any{"handle": handles}
		if len(t.AllowedSources) > 0 {
			route["match"] = []any{map[string]any{"remote_ip": map[string]any{"ranges": t.AllowedSources}}}
		}
		routes = append(routes, route)
	} else {
		for _, id := range ids {
			sel := ep.Bindings[id]
			match := make(map[string]any)
			if len(t.AllowedSources) > 0 {
				match["remote_ip"] = map[string]any{"ranges": t.AllowedSources}
			}
			if sel.Instance != "" && (t.InstanceDispatch == "sni" || t.InstanceDispatch == "host") {
				host := sel.Instance
				if t.HostSuffix != "" {
					host += "." + t.HostSuffix
				}
				if t.InstanceDispatch == "host" {
					match["host"] = []string{host}
				} else {
					match["tls"] = map[string]any{"sni": []string{host}}
				}
			}
			handles := make([]any, 0)
			for _, h := range t.Middleware {
				handles = append(handles, h)
			}
			if t.Kind == "http" {
				transport := map[string]any{"protocol": "goreverselb", "binding": id}
				if len(t.Upstream.Versions) != 0 {
					transport["versions"] = t.Upstream.Versions
				}
				if t.Upstream.TLS != nil {
					transport["tls"] = t.Upstream.TLS
				}
				if t.Upstream.ResponseHeaderTimeout != 0 {
					transport["response_header_timeout"] = t.Upstream.ResponseHeaderTimeout
				}
				handles = append(handles, map[string]any{
					"handler": "reverse_proxy", "transport": transport,
					"upstreams":          []any{map[string]any{"dial": id + ".revlb.invalid:80"}},
					"stream_close_delay": int64(30 * time.Second),
				})
			} else {
				if t.FrontendTLS {
					handles = append(handles, tlsHandler(t.TLSServerName))
				}
				handles = append(handles, map[string]any{"handler": "goreverselb", "binding": id})
			}
			route := map[string]any{"handle": handles}
			if len(match) > 0 {
				route["match"] = []any{match}
			}
			routes = append(routes, route)
		}
	}
	if t.FrontendSSH {
		route := map[string]any{"handle": []any{
			map[string]any{"handler": "goreverselb_ssh"},
			map[string]any{"handler": "subroute", "routes": routes},
		}}
		if len(t.AllowedSources) > 0 {
			route["match"] = []any{map[string]any{"remote_ip": map[string]any{"ranges": t.AllowedSources}}}
		}
		routes = []any{route}
	}
	server := map[string]any{
		"@id":    serverName(runtimeID, ep.ID) + "-server",
		"listen": []string{net.JoinHostPort(ep.BindHost, strconv.Itoa(ep.Port))},
		"routes": routes,
	}
	app := "layer4"
	if t.Kind == "http" {
		app = "http"
		server["automatic_https"] = map[string]any{"disable": true}
		if t.FrontendTLS {
			server["tls_connection_policies"] = []any{map[string]any{"default_sni": t.TLSServerName}}
		}
	}
	data, err := json.Marshal(server)
	return app, data, err
}

func tlsHandler(name string) any {
	return map[string]any{
		"handler":             "tls",
		"connection_policies": []any{map[string]any{"default_sni": name}},
	}
}

func mergeGenerated(apps document, a *App, old map[string]Endpoint) error {
	configs := make(map[string]document)
	servers := make(map[string]document)
	for _, app := range []string{"http", "layer4"} {
		configs[app] = make(document)
		servers[app] = make(document)
		if raw, ok := apps[app]; ok {
			config := configs[app]
			if err := json.Unmarshal(raw, &config); err != nil {
				return err
			}
			configs[app] = config
			if raw, ok := configs[app]["servers"]; ok {
				serverMap := servers[app]
				if err := json.Unmarshal(raw, &serverMap); err != nil {
					return err
				}
				servers[app] = serverMap
			}
		}
	}
	for id, ep := range old {
		app, expected, err := renderEndpoint(a.RuntimeID, ep)
		if err != nil {
			return err
		}
		name := serverName(a.RuntimeID, id)
		actual, ok := servers[app][name]
		if !ok || !jsonEqual(actual, expected) {
			return fmt.Errorf("ownership conflict for generated server %s; restore it or remove the endpoint manifest explicitly", name)
		}
		delete(servers[app], name)
	}
	for id, ep := range a.Generated {
		app, data, err := renderEndpoint(a.RuntimeID, ep)
		if err != nil {
			return err
		}
		name := serverName(a.RuntimeID, id)
		if _, exists := servers[app][name]; exists {
			return fmt.Errorf("generated server name %s is already in use", name)
		}
		for otherApp, all := range servers {
			for otherName, raw := range all {
				var s struct {
					Listen []string `json:"listen"`
				}
				if err := json.Unmarshal(raw, &s); err != nil {
					return err
				}
				for _, addr := range s.Listen {
					if overlaps(addr, ep.BindHost, ep.Port) {
						return fmt.Errorf("frontend port %d conflicts with %s server %s", ep.Port, otherApp, otherName)
					}
				}
			}
		}
		for _, addr := range a.Control.Listen {
			if overlaps(addr, ep.BindHost, ep.Port) {
				return fmt.Errorf("frontend port %d conflicts with the control listener", ep.Port)
			}
		}
		servers[app][name] = data
	}
	for _, app := range []string{"http", "layer4"} {
		if len(servers[app]) == 0 && len(configs[app]) == 0 {
			continue
		}
		data, err := json.Marshal(servers[app])
		if err != nil {
			return err
		}
		configs[app]["servers"] = data
		apps[app], err = json.Marshal(configs[app])
		if err != nil {
			return err
		}
	}
	return nil
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func overlaps(addr, host string, port int) bool {
	na, err := caddy.ParseNetworkAddress(addr)
	if err != nil || !strings.HasPrefix(na.Network, "tcp") || port < int(na.StartPort) || port > int(na.EndPort) {
		return false
	}
	return na.Host == host || na.Host == "" || na.Host == "0.0.0.0" || na.Host == "::" ||
		host == "0.0.0.0" || host == "::" || net.ParseIP(na.Host) == nil
}

func configuredPorts(root, apps document, a *App) map[int]bool {
	reserved := make(map[int]bool)
	reserve := func(addr string) {
		for port := a.Publication.PortStart; port < a.Publication.PortStart+a.Publication.PortCount; port++ {
			if overlaps(addr, a.Publication.BindHost, port) {
				reserved[port] = true
			}
		}
	}
	for _, app := range []string{"http", "layer4"} {
		var cfg struct {
			Servers map[string]struct {
				Listen []string `json:"listen"`
			} `json:"servers"`
		}
		if raw := apps[app]; len(raw) != 0 && json.Unmarshal(raw, &cfg) == nil {
			for _, s := range cfg.Servers {
				for _, addr := range s.Listen {
					reserve(addr)
				}
			}
		}
	}
	for _, addr := range a.Control.Listen {
		reserve(addr)
	}
	var admin struct {
		Listen string `json:"listen"`
	}
	if raw := root["admin"]; len(raw) != 0 && json.Unmarshal(raw, &admin) == nil {
		if admin.Listen != "" {
			reserve(admin.Listen)
		}
	}
	return reserved
}

func (r *runtime) reconcile() {
	a := r.active.Load()
	if a == nil || a.Publication.Mode != "dynamic" || r.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	err := r.update(ctx, func(current *App) error {
		r.mu.Lock()
		live := make(map[tunnelcore.Selector]protocol.TunnelData)
		recovering := make(map[string]bool)
		for id, until := range r.recovered {
			if time.Now().Before(until) {
				recovering[id] = true
			} else {
				delete(r.recovered, id)
			}
		}
		for _, reg := range r.entries {
			if !reg.session.IsClosed() {
				if _, err := current.authorize(reg.data); err == nil {
					live[reg.selector] = reg.data
				}
			}
		}
		r.mu.Unlock()
		for id, ep := range current.Generated {
			if recovering[id] {
				continue
			}
			ep.Bindings = cloneBindings(ep.Bindings)
			for b, sel := range ep.Bindings {
				if _, ok := live[sel]; !ok {
					delete(ep.Bindings, b)
				}
			}
			if len(ep.Bindings) == 0 {
				delete(current.Generated, id)
			} else {
				refreshRevision(&ep)
				current.Generated[id] = ep
			}
		}
		// Reloading source policy omits generated routes. Restore only known
		// leases, preserving the port already acknowledged to each live client.
		for sel, td := range live {
			id := "e-" + stableID(current.RuntimeID, sel.UserID, sel.Service)
			if ep, ok := current.Generated[id]; ok {
				if _, ok := ep.Bindings[bindingID(sel)]; ok {
					continue
				}
			}
			if _, ok := current.recoverableLeases[id]; !ok {
				return fmt.Errorf("cannot restore live endpoint %s without its publication lease", id)
			}
			if _, err := current.addEndpoint(sel, td); err != nil {
				return fmt.Errorf("restore live endpoint %s: %w", id, err)
			}
		}
		return nil
	})
	if err != nil && r.ctx.Err() == nil {
		r.logger.Error("endpoint reconciliation failed", zap.Error(err))
	}
}
