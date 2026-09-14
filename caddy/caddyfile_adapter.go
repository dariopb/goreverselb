package reverselb

import (
	"encoding/json"
	"fmt"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
)

func init() {
	caddyconfig.RegisterAdapter("goreverselb-caddyfile", certificateCaddyfileAdapter{})
}

type certificateCaddyfileAdapter struct{}

func (certificateCaddyfileAdapter) Adapt(body []byte, options map[string]any) ([]byte, []caddyconfig.Warning, error) {
	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		return nil, nil, fmt.Errorf("standard Caddyfile adapter is not registered")
	}
	data, warnings, err := adapter.Adapt(body, options)
	if err != nil {
		return nil, warnings, err
	}
	result, err := mergeCaddyfileCertificates(data)
	if err != nil {
		return nil, warnings, fmt.Errorf("adapt goreverselb certificates: %w", err)
	}
	return result, warnings, nil
}

func mergeCaddyfileCertificates(data []byte) ([]byte, error) {
	var root document
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	var apps document
	if raw := root["apps"]; raw != nil {
		if err := json.Unmarshal(raw, &apps); err != nil {
			return nil, err
		}
	}
	raw, ok := apps["goreverselb"]
	if !ok {
		return data, nil
	}
	var app document
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, err
	}
	filesRaw, ok := app["certificate_files"]
	if !ok {
		return data, nil
	}
	var files caddytls.FileLoader
	if err := json.Unmarshal(filesRaw, &files); err != nil {
		return nil, err
	}
	tlsApp := make(document)
	if raw := apps["tls"]; raw != nil {
		if err := json.Unmarshal(raw, &tlsApp); err != nil {
			return nil, err
		}
	}
	if tlsApp == nil {
		tlsApp = make(document)
	}
	certificates := make(document)
	if raw := tlsApp["certificates"]; raw != nil {
		if err := json.Unmarshal(raw, &certificates); err != nil {
			return nil, err
		}
	}
	if certificates == nil {
		certificates = make(document)
	}
	var existing caddytls.FileLoader
	if raw := certificates["load_files"]; raw != nil {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, err
		}
	}
	for _, file := range files {
		found := false
		for _, old := range existing {
			if old.Certificate == file.Certificate && old.Key == file.Key {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, file)
		}
	}
	delete(app, "certificate_files")
	var err error
	if certificates["load_files"], err = json.Marshal(existing); err != nil {
		return nil, err
	}
	if tlsApp["certificates"], err = json.Marshal(certificates); err != nil {
		return nil, err
	}
	if apps["tls"], err = json.Marshal(tlsApp); err != nil {
		return nil, err
	}
	if apps["goreverselb"], err = json.Marshal(app); err != nil {
		return nil, err
	}
	if root["apps"], err = json.Marshal(apps); err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

var _ caddyconfig.Adapter = certificateCaddyfileAdapter{}
