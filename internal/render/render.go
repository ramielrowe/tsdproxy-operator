/*
Copyright 2026 Andrew Melton.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package render turns TSDProxy and Proxy resources into the two files a
// tsdproxy v3 process reads: the main configuration, which tsdproxy loads once
// at startup, and the proxy list, which tsdproxy watches and reloads live.
package render

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/attach"
)

// Paths and names shared between the rendered configuration and the pod that
// mounts it. The configuration refers to these paths, so the StatefulSet must
// mount the matching volumes there.
const (
	// ConfigDir is where the ConfigMap is mounted. It must be mounted as a
	// whole directory: tsdproxy detects ConfigMap updates through the
	// directory's symlink swap, which a subPath mount never sees.
	ConfigDir      = "/config"
	MainConfigFile = "tsdproxy.yaml"
	ProxyListFile  = "proxies.yaml"

	// DataDir holds the Tailscale state of every device.
	DataDir = "/data"

	// TailscaleSecretDir is where the Tailscale credentials are mounted.
	TailscaleSecretDir   = "/secrets/tailscale"
	AuthKeyFileName      = "authkey"
	ClientSecretFileName = "client_secret"

	// APIKeyDir is where the operator-generated API key is mounted.
	APIKeyDir      = "/secrets/api"
	APIKeyFileName = "api-key"

	// HTTPPort serves the tsdproxy dashboard, REST API and health endpoint.
	HTTPPort = 8080

	// ProviderName is the single Tailscale provider of an instance.
	ProviderName = "default"
	// ListName is the single list target provider of an instance.
	ListName = "kubernetes"

	// ClientIDEnv is the environment variable tsdproxy reads the OAuth client
	// ID of ProviderName from. tsdproxy has no file-based option for it.
	ClientIDEnv = "TSDPROXY_TAILSCALE_DEFAULT_CLIENTID"

	defaultLogLevel = "info"
)

// mainConfig mirrors the subset of tsdproxy's config.Data the operator sets.
// Field names follow tsdproxy's canonical YAML keys.
type mainConfig struct {
	DefaultProxyProvider string                `json:"defaultProxyProvider"`
	Tailscale            tailscaleConfig       `json:"tailscale"`
	Lists                map[string]listConfig `json:"lists"`
	HTTP                 httpConfig            `json:"http"`
	Log                  logConfig             `json:"log"`
	APIKeyFile           string                `json:"apiKeyFile"`
	Admins               []string              `json:"admins,omitempty"`
	AdminAllowLocalhost  bool                  `json:"adminAllowLocalhost"`
}

type tailscaleConfig struct {
	DataDir   string                    `json:"dataDir"`
	Providers map[string]providerConfig `json:"providers"`
}

type providerConfig struct {
	AuthKeyFile         string `json:"authKeyFile,omitempty"`
	ClientSecretFile    string `json:"clientSecretFile,omitempty"`
	Tags                string `json:"tags,omitempty"`
	ControlURL          string `json:"controlUrl,omitempty"`
	Hostname            string `json:"hostname,omitempty"`
	Services            bool   `json:"services,omitempty"`
	PreventDuplicates   bool   `json:"preventDuplicates,omitempty"`
	AutoApproveDevices  bool   `json:"autoApproveDevices,omitempty"`
	AutoRemoveConflicts bool   `json:"autoRemoveConflicts,omitempty"`
	AutoProvisionACL    bool   `json:"autoProvisionAcl,omitempty"`
}

type listConfig struct {
	Filename             string `json:"filename"`
	DefaultProxyProvider string `json:"defaultProxyProvider"`
}

type httpConfig struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
}

type logConfig struct {
	Level string `json:"level"`
	JSON  bool   `json:"json"`
}

// MainConfig renders tsdproxy.yaml for an instance. tsdproxy reads this file
// once at startup, so any change to the result requires restarting the pod.
func MainConfig(inst *v1alpha1.TSDProxy) ([]byte, error) {
	ts := inst.Spec.Tailscale

	provider := providerConfig{
		Tags:              strings.Join(ts.Tags, ","),
		ControlURL:        ts.ControlURL,
		PreventDuplicates: ts.PreventDuplicates,
		AutoProvisionACL:  ts.AutoProvisionACL,
	}
	switch {
	case ts.OAuth != nil:
		provider.ClientSecretFile = TailscaleSecretDir + "/" + ClientSecretFileName
	case ts.AuthKey != nil:
		provider.AuthKeyFile = TailscaleSecretDir + "/" + AuthKeyFileName
	default:
		return nil, fmt.Errorf("tailscale: one of authKey or oauth is required")
	}
	if ts.Mode == v1alpha1.TailscaleModeServices {
		provider.Services = true
		provider.Hostname = ts.Hostname
		provider.AutoApproveDevices = ts.AutoApproveDevices
		provider.AutoRemoveConflicts = ts.AutoRemoveConflicts
	}

	log := logConfig{Level: defaultLogLevel}
	if inst.Spec.Log != nil {
		if inst.Spec.Log.Level != "" {
			log.Level = inst.Spec.Log.Level
		}
		log.JSON = inst.Spec.Log.JSON
	}

	cfg := mainConfig{
		DefaultProxyProvider: ProviderName,
		Tailscale: tailscaleConfig{
			DataDir:   DataDir,
			Providers: map[string]providerConfig{ProviderName: provider},
		},
		Lists: map[string]listConfig{
			ListName: {
				Filename:             ConfigDir + "/" + ProxyListFile,
				DefaultProxyProvider: ProviderName,
			},
		},
		// tsdproxy binds 127.0.0.1 unless it detects Docker through
		// /.dockerenv, which does not exist under Kubernetes. Probes and the
		// operator's status polling need the pod IP.
		HTTP:       httpConfig{Hostname: "0.0.0.0", Port: HTTPPort},
		Log:        log,
		APIKeyFile: APIKeyDir + "/" + APIKeyFileName,
		Admins:     inst.Spec.Admins,
		// adminAllowLocalhost also trusts every RFC 1918 source address,
		// which covers the whole pod network. Keep it off; the operator
		// authenticates with the API key.
		AdminAllowLocalhost: false,
	}

	return yaml.Marshal(cfg)
}

// listEntry mirrors one proxy in a tsdproxy list file.
type listEntry struct {
	Ports     map[string]listPort `json:"ports"`
	Tailscale listTailscale       `json:"tailscale"`
	Dashboard listDashboard       `json:"dashboard"`
}

// listPort always carries every boolean explicitly. tsdproxy applies its
// struct defaults before decoding the ports map, so entries created during
// decoding start from zero values: an omitted tlsValidate would mean false.
type listPort struct {
	Targets     []string          `json:"targets"`
	IsRedirect  bool              `json:"isRedirect"`
	TLSValidate bool              `json:"tlsValidate"`
	Tailscale   listPortTailscale `json:"tailscale"`
}

type listPortTailscale struct {
	Funnel bool `json:"funnel"`
}

type listTailscale struct {
	Tags         string `json:"tags,omitempty"`
	Ephemeral    bool   `json:"ephemeral"`
	RunWebClient bool   `json:"runWebClient"`
	Verbose      bool   `json:"verbose"`
}

// listDashboard omits an empty icon so tsdproxy keeps its default icon, and
// always sets visible: tsdproxy's REST API hides invisible proxies, which
// would blind the operator's status reporting.
type listDashboard struct {
	Label    string `json:"label,omitempty"`
	Icon     string `json:"icon,omitempty"`
	Category string `json:"category,omitempty"`
	Visible  bool   `json:"visible"`
}

// ProxyList renders proxies.yaml from the proxies attached to an instance.
// tsdproxy reloads this file on change and only restarts the proxies whose
// entry differs. The output is deterministic for a given input.
func ProxyList(inst *v1alpha1.TSDProxy, attached []attach.Attached) ([]byte, error) {
	list := make(map[string]listEntry, len(attached))
	for _, a := range attached {
		if _, dup := list[a.Hostname]; dup {
			return nil, fmt.Errorf("hostname %q attached more than once", a.Hostname)
		}
		list[a.Hostname] = proxyEntry(inst, a.Proxy)
	}
	return yaml.Marshal(list)
}

func proxyEntry(inst *v1alpha1.TSDProxy, p *v1alpha1.Proxy) listEntry {
	entry := listEntry{
		Ports:     make(map[string]listPort, len(p.Spec.Ports)),
		Tailscale: listTailscale{Ephemeral: inst.Spec.Tailscale.Ephemeral},
		Dashboard: listDashboard{Visible: true},
	}

	if ts := p.Spec.Tailscale; ts != nil {
		entry.Tailscale.Tags = strings.Join(ts.Tags, ",")
		if ts.Ephemeral != nil {
			entry.Tailscale.Ephemeral = *ts.Ephemeral
		}
		entry.Tailscale.RunWebClient = ts.RunWebClient
		entry.Tailscale.Verbose = ts.Verbose
	}

	if d := p.Spec.Dashboard; d != nil {
		entry.Dashboard.Label = d.Label
		entry.Dashboard.Icon = d.Icon
		entry.Dashboard.Category = d.Category
	}

	for _, port := range p.Spec.Ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = v1alpha1.ProtocolHTTPS
		}
		key := fmt.Sprintf("%d/%s", port.Port, protocol)
		entry.Ports[key] = listPort{
			Targets:     []string{TargetURL(p.Namespace, protocol, port.Target)},
			IsRedirect:  port.Redirect,
			TLSValidate: !port.InsecureSkipTLSVerify,
			Tailscale:   listPortTailscale{Funnel: port.Funnel},
		}
	}

	return entry
}

// TargetURL resolves a port target to the URL tsdproxy dials. A Service
// target becomes its cluster DNS name, which the tsdproxy pod resolves through
// the cluster's search domains.
func TargetURL(namespace string, protocol v1alpha1.PortProtocol, target v1alpha1.PortTarget) string {
	if target.Service == nil {
		return target.URL
	}
	scheme := target.Service.Scheme
	if scheme == "" {
		scheme = defaultTargetScheme(protocol)
	}
	return fmt.Sprintf("%s://%s.%s.svc:%d", scheme, target.Service.Name, namespace, target.Service.Port)
}

func defaultTargetScheme(protocol v1alpha1.PortProtocol) string {
	switch protocol {
	case v1alpha1.ProtocolTCP:
		return "tcp"
	case v1alpha1.ProtocolUDP:
		return "udp"
	default:
		return "http"
	}
}
