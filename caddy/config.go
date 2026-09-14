// Package reverselb registers Caddy integration for dynamically published reverse tunnels.
package reverselb

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
	"github.com/dariopb/goreverselb/pkg/tunnelcore/protocol"
)

type Control struct {
	Listen []string   `json:"listen,omitempty"`
	TLS    ControlTLS `json:"tls"`
}

type ControlTLS struct {
	ServerName string `json:"server_name"`
}

type User struct {
	TokenEnv     string       `json:"token_env"`
	Registration Registration `json:"registration,omitempty"`
}

type Registration struct {
	ServicePatterns []string `json:"service_patterns,omitempty"`
	MaxEndpoints    int      `json:"max_endpoints,omitempty"`
}

type Publication struct {
	Mode            string              `json:"mode,omitempty"`
	AdminEndpoint   string              `json:"admin_endpoint,omitempty"`
	BindHost        string              `json:"bind_host,omitempty"`
	AdvertiseHost   string              `json:"advertise_host,omitempty"`
	PortStart       int                 `json:"port_start,omitempty"`
	PortCount       int                 `json:"port_count,omitempty"`
	DefaultTemplate string              `json:"default_template,omitempty"`
	Rules           []Rule              `json:"rules,omitempty"`
	Templates       map[string]Template `json:"templates,omitempty"`
	RestartGrace    caddy.Duration      `json:"restart_grace,omitempty"`
}

type Rule struct {
	UserID         string `json:"user_id"`
	ServicePattern string `json:"service_pattern"`
	Template       string `json:"template"`
}

type Template struct {
	Kind             string            `json:"kind"`
	InstanceDispatch string            `json:"instance_dispatch,omitempty"`
	FrontendTLS      bool              `json:"frontend_tls,omitempty"`
	FrontendSSH      bool              `json:"frontend_ssh,omitempty"`
	TLSServerName    string            `json:"tls_server_name,omitempty"`
	HostSuffix       string            `json:"host_suffix,omitempty"`
	AllowedSources   []string          `json:"allowed_sources,omitempty"`
	Middleware       []json.RawMessage `json:"middleware,omitempty"`
	Upstream         Upstream          `json:"upstream,omitempty"`
}

type Upstream struct {
	Versions              []string       `json:"versions,omitempty"`
	TLS                   *UpstreamTLS   `json:"tls,omitempty"`
	ResponseHeaderTimeout caddy.Duration `json:"response_header_timeout,omitempty"`
}

type UpstreamTLS struct {
	ServerName string `json:"server_name"`
	CAFile     string `json:"ca_file,omitempty"`
}

type Endpoint struct {
	ID           string                         `json:"id"`
	UserID       string                         `json:"user_id"`
	Service      string                         `json:"service"`
	Port         int                            `json:"port"`
	BindHost     string                         `json:"bind_host"`
	TemplateName string                         `json:"template_name"`
	Template     Template                       `json:"template"`
	Bindings     map[string]tunnelcore.Selector `json:"bindings"`
	Revision     string                         `json:"revision"`
}

var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func bindingID(sel tunnelcore.Selector) string {
	return "b-" + stableID(sel.UserID, sel.Service, sel.Instance)
}

func (a *App) defaults() {
	if a.RuntimeID == "" {
		a.RuntimeID = "default"
	}
	p := &a.Publication
	if p.Mode == "" {
		p.Mode = "dynamic"
	}
	if p.AdminEndpoint == "" {
		p.AdminEndpoint = "http://localhost:2019"
	}
	if p.BindHost == "" {
		p.BindHost = "0.0.0.0"
	}
	if p.AdvertiseHost == "" {
		p.AdvertiseHost = a.Control.TLS.ServerName
	}
	if p.PortStart == 0 {
		p.PortStart = 8000
	}
	if p.PortCount == 0 {
		p.PortCount = 100
	}
	if p.RestartGrace == 0 {
		p.RestartGrace = caddy.Duration(30 * time.Second)
	}
	if p.DefaultTemplate == "" {
		p.DefaultTemplate = "raw-tcp"
	}
	if p.Templates == nil {
		p.Templates = map[string]Template{"raw-tcp": {Kind: "tcp", InstanceDispatch: "direct"}}
	}
	for name, t := range p.Templates {
		if t.InstanceDispatch == "" {
			t.InstanceDispatch = "direct"
		}
		p.Templates[name] = t
	}
	if a.Generated == nil {
		a.Generated = make(map[string]Endpoint)
	}
}

func (a *App) Validate() error {
	if len(a.CertificateFiles) != 0 {
		return errors.New("certificate directives require --adapter goreverselb-caddyfile; in native JSON use apps.tls.certificates.load_files")
	}
	if !label.MatchString(a.RuntimeID) {
		return errors.New("runtime_id must be a DNS label")
	}
	if len(a.Control.Listen) == 0 || a.Control.TLS.ServerName == "" {
		return errors.New("control.listen and control.tls.server_name are required")
	}
	for _, addr := range a.Control.Listen {
		na, err := caddy.ParseNetworkAddress(addr)
		if err != nil || na.PortRangeSize() != 1 || (na.Network != "tcp" && na.Network != "tcp4" && na.Network != "tcp6") {
			return fmt.Errorf("control address %q must be a single TCP address", addr)
		}
	}
	p := a.Publication
	if p.Mode != "dynamic" && p.Mode != "bindings_only" {
		return fmt.Errorf("unsupported publication mode %q", p.Mode)
	}
	if _, err := localAdminURL(p.AdminEndpoint); err != nil {
		return err
	}
	if net.ParseIP(p.BindHost) == nil {
		return errors.New("publication.bind_host must be an IP address")
	}
	if p.PortStart < 1 || p.PortCount < 1 || p.PortStart > 65536-p.PortCount {
		return errors.New("publication port pool must fit in 1..65535")
	}
	if p.RestartGrace < 0 {
		return errors.New("restart_grace cannot be negative")
	}
	if _, ok := p.Templates[p.DefaultTemplate]; !ok && p.Mode == "dynamic" {
		return errors.New("default_template does not exist")
	}
	for name, t := range p.Templates {
		if !label.MatchString(name) {
			return fmt.Errorf("invalid template name %q", name)
		}
		if err := t.validate(); err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
	}
	for _, r := range p.Rules {
		if _, ok := a.Users[r.UserID]; !ok {
			return fmt.Errorf("rule references unknown user %q", r.UserID)
		}
		if _, err := path.Match(r.ServicePattern, ""); err != nil {
			return fmt.Errorf("invalid service pattern: %w", err)
		}
		if _, ok := p.Templates[r.Template]; !ok {
			return fmt.Errorf("rule references unknown template %q", r.Template)
		}
	}
	if len(a.Users) == 0 {
		return errors.New("at least one user is required")
	}
	for id, u := range a.Users {
		if id == "" || strings.ContainsAny(id, ":\x00/") || u.TokenEnv == "" {
			return fmt.Errorf("invalid user or missing token_env for %q", id)
		}
		if os.Getenv(u.TokenEnv) == "" {
			return fmt.Errorf("token_env for user %q is unset or empty", id)
		}
		if u.Registration.MaxEndpoints < 0 {
			return errors.New("max_endpoints cannot be negative")
		}
		for _, pattern := range u.Registration.ServicePatterns {
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("user %s: invalid service pattern: %w", id, err)
			}
		}
	}
	for id, sel := range a.Bindings {
		if !label.MatchString(id) || !validSelector(sel) {
			return fmt.Errorf("invalid binding %q", id)
		}
	}
	ports := make(map[int]bool)
	for id, ep := range a.Generated {
		if id != ep.ID || id != "e-"+stableID(a.RuntimeID, ep.UserID, ep.Service) ||
			ep.Port < p.PortStart || ep.Port >= p.PortStart+p.PortCount || ep.BindHost != p.BindHost {
			return fmt.Errorf("invalid generated endpoint %q", id)
		}
		if err := ep.Template.validate(); err != nil {
			return fmt.Errorf("generated endpoint %s: %w", id, err)
		}
		if ports[ep.Port] || len(ep.Bindings) == 0 || (ep.Template.InstanceDispatch == "direct" && len(ep.Bindings) != 1) {
			return fmt.Errorf("generated endpoint %s has duplicate port or invalid instance cardinality", id)
		}
		ports[ep.Port] = true
		for b, sel := range ep.Bindings {
			if b != bindingID(sel) || !validSelector(sel) || sel.UserID != ep.UserID || sel.Service != ep.Service {
				return fmt.Errorf("invalid generated binding %q", b)
			}
			if _, exists := a.Bindings[b]; exists {
				return fmt.Errorf("generated binding %q collides with a static binding", b)
			}
		}
	}
	return nil
}

func (t Template) validate() error {
	if t.Kind != "tcp" && t.Kind != "http" {
		return fmt.Errorf("unsupported kind %q", t.Kind)
	}
	if t.InstanceDispatch != "direct" && t.InstanceDispatch != "legacy" &&
		!(t.Kind == "tcp" && t.InstanceDispatch == "sni") &&
		!(t.Kind == "http" && t.InstanceDispatch == "host") {
		return fmt.Errorf("unsupported instance_dispatch %q", t.InstanceDispatch)
	}
	if t.Kind == "http" && t.InstanceDispatch == "legacy" {
		return errors.New("legacy dispatch requires TCP")
	}
	if t.FrontendSSH && (t.Kind != "tcp" || t.FrontendTLS) {
		return errors.New("frontend_ssh requires TCP and cannot be combined with frontend_tls")
	}
	if t.FrontendTLS && t.TLSServerName == "" {
		return errors.New("frontend TLS requires tls_server_name")
	}
	for _, cidr := range t.AllowedSources {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid source CIDR %q", cidr)
		}
	}
	return t.Upstream.validate()
}

func (u Upstream) validate() error {
	if u.ResponseHeaderTimeout < 0 {
		return errors.New("response_header_timeout cannot be negative")
	}
	for _, v := range u.Versions {
		if v != "1.1" && v != "2" && v != "h2c" {
			return fmt.Errorf("unsupported upstream HTTP version %q", v)
		}
		if v == "h2c" && (u.TLS != nil || len(u.Versions) != 1) {
			return errors.New("h2c must be the only version and cannot use TLS")
		}
	}
	if u.TLS != nil && u.TLS.ServerName == "" {
		return errors.New("upstream TLS requires server_name")
	}
	return nil
}

func validSelector(sel tunnelcore.Selector) bool {
	for _, s := range []string{sel.UserID, sel.Service, sel.Instance} {
		if len(s) > 253 || strings.ContainsAny(s, ":\x00/\\ \t\r\n") {
			return false
		}
	}
	return sel.UserID != "" && sel.Service != ""
}

func (a *App) authorize(td protocol.TunnelData) (tunnelcore.Selector, error) {
	service, instance, _ := strings.Cut(td.ServiceName, ":")
	userID := "default@none"
	if prefix, _, ok := strings.Cut(td.Token, ":"); ok {
		userID = prefix
	}
	sel := tunnelcore.Selector{UserID: userID, Service: service, Instance: instance}
	if !validSelector(sel) {
		return sel, errors.New("invalid service identity")
	}
	u, ok := a.Users[userID]
	secret := os.Getenv(u.TokenEnv)
	if !ok || secret == "" || subtle.ConstantTimeCompare([]byte(td.Token), []byte(secret)) != 1 {
		return sel, errors.New("registration denied")
	}
	if a.Publication.Mode == "bindings_only" {
		for _, b := range a.Bindings {
			if sel == b {
				return sel, nil
			}
		}
	} else {
		for _, pattern := range u.Registration.ServicePatterns {
			if match, _ := path.Match(pattern, service); match {
				return sel, nil
			}
		}
	}
	return sel, errors.New("service is not permitted by registration policy")
}

func (a *App) templateFor(sel tunnelcore.Selector) (string, Template) {
	name := a.Publication.DefaultTemplate
	for _, r := range a.Publication.Rules {
		match, _ := path.Match(r.ServicePattern, sel.Service)
		if r.UserID == sel.UserID && match {
			name = r.Template
			break
		}
	}
	return name, a.Publication.Templates[name]
}

func (a *App) selector(binding string) (tunnelcore.Selector, bool) {
	if sel, ok := a.Bindings[binding]; ok {
		return sel, true
	}
	for _, ep := range a.Generated {
		if sel, ok := ep.Bindings[binding]; ok {
			return sel, true
		}
	}
	return tunnelcore.Selector{}, false
}

func localAdminURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "unix" && u.Path != "" && u.Host == "" && u.RawQuery == "" {
		return u, nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("admin_endpoint must be loopback HTTP or unix:///absolute/socket")
	}
	return u, nil
}
