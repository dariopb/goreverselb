package reverselb

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/dariopb/goreverselb/pkg/tunnelcore"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("goreverselb", parseGlobal)
}

func parseGlobal(d *caddyfile.Dispenser, existing any) (any, error) {
	if existing != nil {
		return nil, d.Err("goreverselb may only be configured once")
	}
	a := new(App)
	if err := a.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	return httpcaddyfile.App{Name: "goreverselb", Value: data}, nil
}

func oneArg(d *caddyfile.Dispenser) (string, error) {
	args := d.RemainingArgs()
	if len(args) != 1 {
		return "", d.ArgErr()
	}
	return args[0], nil
}

func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	a.Users = make(map[string]User)
	a.Bindings = make(map[string]tunnelcore.Selector)
	for d.Next() {
		if len(d.RemainingArgs()) != 0 {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "certificate":
				args := d.RemainingArgs()
				if len(args) != 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
					return d.Err("certificate requires a certificate-chain file and a private-key file")
				}
				a.CertificateFiles = append(a.CertificateFiles, caddytls.CertKeyFilePair{Certificate: args[0], Key: args[1]})
			case "runtime_id":
				value, err := oneArg(d)
				if err != nil {
					return err
				}
				a.RuntimeID = value
			case "control":
				a.Control.Listen = d.RemainingArgs()
				if len(a.Control.Listen) == 0 {
					return d.ArgErr()
				}
				for nesting := d.Nesting(); d.NextBlock(nesting); {
					if d.Val() != "tls" {
						return d.Errf("unknown control option %s", d.Val())
					}
					value, err := oneArg(d)
					if err != nil {
						return err
					}
					a.Control.TLS.ServerName = value
				}
			case "user":
				id, err := oneArg(d)
				if err != nil {
					return err
				}
				if _, exists := a.Users[id]; exists {
					return d.Errf("duplicate user %s", id)
				}
				var u User
				for nesting := d.Nesting(); d.NextBlock(nesting); {
					switch d.Val() {
					case "token_env":
						u.TokenEnv, err = oneArg(d)
					case "register_services":
						u.Registration.ServicePatterns = d.RemainingArgs()
						if len(u.Registration.ServicePatterns) == 0 {
							err = d.ArgErr()
						}
					case "max_endpoints":
						var value string
						value, err = oneArg(d)
						if err == nil {
							u.Registration.MaxEndpoints, err = strconv.Atoi(value)
						}
					default:
						return d.Errf("unknown user option %s", d.Val())
					}
					if err != nil {
						return err
					}
				}
				a.Users[id] = u
			case "binding":
				id, err := oneArg(d)
				if err != nil {
					return err
				}
				if _, exists := a.Bindings[id]; exists {
					return d.Errf("duplicate binding %s", id)
				}
				var sel tunnelcore.Selector
				for nesting := d.Nesting(); d.NextBlock(nesting); {
					key := d.Val()
					value, err := oneArg(d)
					if err != nil {
						return err
					}
					switch key {
					case "user_id":
						sel.UserID = value
					case "service":
						sel.Service = value
					case "instance":
						sel.Instance = value
					default:
						return d.Errf("unknown binding option %s", key)
					}
				}
				a.Bindings[id] = sel
			case "publication":
				if err := a.Publication.unmarshal(d); err != nil {
					return err
				}
			default:
				return d.Errf("unknown goreverselb option %s", d.Val())
			}
		}
	}
	return nil
}

func (p *Publication) unmarshal(d *caddyfile.Dispenser) error {
	mode, err := oneArg(d)
	if err != nil {
		return err
	}
	p.Mode = mode
	p.Templates = make(map[string]Template)
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		key := d.Val()
		switch key {
		case "template":
			name, err := oneArg(d)
			if err != nil {
				return err
			}
			if _, ok := p.Templates[name]; ok {
				return d.Errf("duplicate template %s", name)
			}
			var t Template
			if err := t.unmarshal(d); err != nil {
				return err
			}
			p.Templates[name] = t
		case "ports":
			args := d.RemainingArgs()
			if len(args) != 2 {
				return d.ArgErr()
			}
			p.PortStart, err = strconv.Atoi(args[0])
			if err == nil {
				p.PortCount, err = strconv.Atoi(args[1])
			}
		case "rule":
			args := d.RemainingArgs()
			if len(args) != 3 {
				return d.ArgErr()
			}
			p.Rules = append(p.Rules, Rule{UserID: args[0], ServicePattern: args[1], Template: args[2]})
		default:
			var value string
			value, err = oneArg(d)
			if err != nil {
				return err
			}
			switch key {
			case "admin_endpoint":
				p.AdminEndpoint = value
			case "bind_host":
				p.BindHost = value
			case "advertise_host":
				p.AdvertiseHost = value
			case "default_template":
				p.DefaultTemplate = value
			case "restart_grace":
				var duration time.Duration
				duration, err = caddy.ParseDuration(value)
				p.RestartGrace = caddy.Duration(duration)
			default:
				return d.Errf("unknown publication option %s", key)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *Template) unmarshal(d *caddyfile.Dispenser) error {
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		key := d.Val()
		if key == "allowed_sources" {
			t.AllowedSources = d.RemainingArgs()
			if len(t.AllowedSources) == 0 {
				return d.ArgErr()
			}
			continue
		}
		value, err := oneArg(d)
		if err != nil {
			return err
		}
		switch key {
		case "kind":
			t.Kind = value
		case "instance_dispatch":
			t.InstanceDispatch = value
		case "frontend_tls", "frontend_ssh":
			var enabled bool
			switch value {
			case "on", "true":
				enabled = true
			case "off", "false":
			default:
				return fmt.Errorf("%s must be on or off", key)
			}
			if key == "frontend_ssh" {
				t.FrontendSSH = enabled
			} else {
				t.FrontendTLS = enabled
			}
		case "tls_server_name":
			t.TLSServerName = value
		case "host_suffix":
			t.HostSuffix = value
		default:
			return d.Errf("unknown template option %s (use native JSON for middleware and upstream settings)", key)
		}
	}
	return nil
}

var _ caddyfile.Unmarshaler = (*App)(nil)
