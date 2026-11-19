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

package render

import (
	"bytes"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/attach"
)

func authKeyInstance() *v1alpha1.TSDProxy {
	return &v1alpha1.TSDProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: "tailnet-home"},
		Spec: v1alpha1.TSDProxySpec{
			Tailscale: v1alpha1.TailscaleSpec{
				AuthKey: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "ts"},
					Key:                  "authkey",
				},
			},
		},
	}
}

func servicesInstance() *v1alpha1.TSDProxy {
	return &v1alpha1.TSDProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: "tailnet-work"},
		Spec: v1alpha1.TSDProxySpec{
			Tailscale: v1alpha1.TailscaleSpec{
				OAuth:               &v1alpha1.OAuthCredentials{SecretName: "ts-oauth"},
				Tags:                []string{"tag:k8s", "tag:proxy"},
				ControlURL:          "https://headscale.example.com",
				Mode:                v1alpha1.TailscaleModeServices,
				Hostname:            "work-proxy",
				Ephemeral:           true,
				PreventDuplicates:   true,
				AutoApproveDevices:  true,
				AutoRemoveConflicts: true,
				AutoProvisionACL:    true,
			},
			Log:    &v1alpha1.LogSpec{Level: "debug", JSON: true},
			Admins: []string{"12345"},
		},
	}
}

func TestMainConfigAuthKey(t *testing.T) {
	got, err := MainConfig(authKeyInstance())
	if err != nil {
		t.Fatalf("MainConfig: %v", err)
	}
	want := `adminAllowLocalhost: false
apiKeyFile: /secrets/api/api-key
defaultProxyProvider: default
http:
  hostname: 0.0.0.0
  port: 8080
lists:
  kubernetes:
    defaultProxyProvider: default
    filename: /config/proxies.yaml
log:
  json: false
  level: info
tailscale:
  dataDir: /data
  providers:
    default:
      authKeyFile: /secrets/tailscale/authkey
`
	if string(got) != want {
		t.Errorf("MainConfig mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestMainConfigServicesOAuth(t *testing.T) {
	got, err := MainConfig(servicesInstance())
	if err != nil {
		t.Fatalf("MainConfig: %v", err)
	}
	want := `adminAllowLocalhost: false
admins:
- "12345"
apiKeyFile: /secrets/api/api-key
defaultProxyProvider: default
http:
  hostname: 0.0.0.0
  port: 8080
lists:
  kubernetes:
    defaultProxyProvider: default
    filename: /config/proxies.yaml
log:
  json: true
  level: debug
tailscale:
  dataDir: /data
  providers:
    default:
      autoApproveDevices: true
      autoProvisionAcl: true
      autoRemoveConflicts: true
      clientSecretFile: /secrets/tailscale/client_secret
      controlUrl: https://headscale.example.com
      hostname: work-proxy
      preventDuplicates: true
      services: true
      tags: tag:k8s,tag:proxy
`
	if string(got) != want {
		t.Errorf("MainConfig mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

// Services-only settings must not leak into a PerProxy instance, where
// tsdproxy warns that they have no effect.
func TestMainConfigIgnoresServicesFieldsInPerProxyMode(t *testing.T) {
	inst := servicesInstance()
	inst.Spec.Tailscale.Mode = v1alpha1.TailscaleModePerProxy

	got, err := MainConfig(inst)
	if err != nil {
		t.Fatalf("MainConfig: %v", err)
	}
	for _, key := range []string{"services:", "hostname: work-proxy", "autoApproveDevices", "autoRemoveConflicts"} {
		if bytes.Contains(got, []byte(key)) {
			t.Errorf("PerProxy config contains %q:\n%s", key, got)
		}
	}
}

func TestMainConfigRequiresCredentials(t *testing.T) {
	inst := authKeyInstance()
	inst.Spec.Tailscale.AuthKey = nil
	if _, err := MainConfig(inst); err == nil {
		t.Fatal("MainConfig succeeded without credentials")
	}
}

func jellyfin() *v1alpha1.Proxy {
	return &v1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{Name: "jellyfin", Namespace: "media"},
		Spec: v1alpha1.ProxySpec{
			InstanceRef: v1alpha1.InstanceReference{Name: "home", Namespace: "tailnet-home"},
			Ports: []v1alpha1.ProxyPort{{
				Port:     443,
				Protocol: v1alpha1.ProtocolHTTPS,
				Target:   v1alpha1.PortTarget{Service: &v1alpha1.ServiceTarget{Name: "jellyfin", Port: 8096}},
			}},
		},
	}
}

func everything() *v1alpha1.Proxy {
	return &v1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{Name: "everything", Namespace: "apps"},
		Spec: v1alpha1.ProxySpec{
			InstanceRef: v1alpha1.InstanceReference{Name: "home", Namespace: "tailnet-home"},
			Hostname:    "all",
			Ports: []v1alpha1.ProxyPort{
				{
					Port:     443,
					Protocol: v1alpha1.ProtocolHTTPS,
					Funnel:   true,
					Target: v1alpha1.PortTarget{
						Service: &v1alpha1.ServiceTarget{Name: "web", Port: 8443, Scheme: "https"},
					},
					InsecureSkipTLSVerify: true,
				},
				{
					Port:     80,
					Protocol: v1alpha1.ProtocolHTTP,
					Redirect: true,
					Target:   v1alpha1.PortTarget{URL: "https://all.example.ts.net"},
				},
				{
					Port:     5432,
					Protocol: v1alpha1.ProtocolTCP,
					Target:   v1alpha1.PortTarget{Service: &v1alpha1.ServiceTarget{Name: "db", Port: 5432}},
				},
				{
					Port:     53,
					Protocol: v1alpha1.ProtocolUDP,
					Target:   v1alpha1.PortTarget{URL: "udp://192.168.1.1:53"},
				},
			},
			Tailscale: &v1alpha1.ProxyTailscale{
				Tags:         []string{"tag:db"},
				Ephemeral:    new(true),
				RunWebClient: true,
				Verbose:      true,
			},
			Dashboard: &v1alpha1.ProxyDashboard{Label: "Everything", Icon: "sh/postgres", Category: "apps"},
		},
	}
}

func TestProxyList(t *testing.T) {
	got, err := ProxyList(authKeyInstance(), []attach.Attached{
		{Proxy: everything(), Hostname: "all"},
		{Proxy: jellyfin(), Hostname: "jellyfin"},
	})
	if err != nil {
		t.Fatalf("ProxyList: %v", err)
	}
	want := `all:
  dashboard:
    category: apps
    icon: sh/postgres
    label: Everything
    visible: true
  ports:
    53/udp:
      isRedirect: false
      tailscale:
        funnel: false
      targets:
      - udp://192.168.1.1:53
      tlsValidate: true
    80/http:
      isRedirect: true
      tailscale:
        funnel: false
      targets:
      - https://all.example.ts.net
      tlsValidate: true
    443/https:
      isRedirect: false
      tailscale:
        funnel: true
      targets:
      - https://web.apps.svc:8443
      tlsValidate: false
    5432/tcp:
      isRedirect: false
      tailscale:
        funnel: false
      targets:
      - tcp://db.apps.svc:5432
      tlsValidate: true
  tailscale:
    ephemeral: true
    runWebClient: true
    tags: tag:db
    verbose: true
jellyfin:
  dashboard:
    visible: true
  ports:
    443/https:
      isRedirect: false
      tailscale:
        funnel: false
      targets:
      - http://jellyfin.media.svc:8096
      tlsValidate: true
  tailscale:
    ephemeral: false
    runWebClient: false
    verbose: false
`
	if string(got) != want {
		t.Errorf("ProxyList mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestProxyListEmpty(t *testing.T) {
	got, err := ProxyList(authKeyInstance(), nil)
	if err != nil {
		t.Fatalf("ProxyList: %v", err)
	}
	if string(got) != "{}\n" {
		t.Errorf("empty list rendered as %q, want %q", got, "{}\n")
	}
}

// The order Proxies arrive in must not change the output: a reordered but
// otherwise identical file would still be rewritten into the ConfigMap.
func TestProxyListIsOrderIndependent(t *testing.T) {
	a := []attach.Attached{{Proxy: everything(), Hostname: "all"}, {Proxy: jellyfin(), Hostname: "jellyfin"}}
	b := []attach.Attached{a[1], a[0]}

	first, err := ProxyList(authKeyInstance(), a)
	if err != nil {
		t.Fatalf("ProxyList: %v", err)
	}
	second, err := ProxyList(authKeyInstance(), b)
	if err != nil {
		t.Fatalf("ProxyList: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("output depends on input order\n--- first ---\n%s--- second ---\n%s", first, second)
	}
}

func TestProxyListEphemeralDefaultAndOverride(t *testing.T) {
	inst := authKeyInstance()
	inst.Spec.Tailscale.Ephemeral = true

	inherits := jellyfin()
	overrides := jellyfin()
	overrides.Spec.Tailscale = &v1alpha1.ProxyTailscale{Ephemeral: new(false)}

	if e := proxyEntry(inst, inherits); !e.Tailscale.Ephemeral {
		t.Error("proxy without an override did not inherit the instance's ephemeral default")
	}
	if e := proxyEntry(inst, overrides); e.Tailscale.Ephemeral {
		t.Error("proxy override ephemeral=false was ignored")
	}
}

func TestProxyListRejectsDuplicateHostnames(t *testing.T) {
	_, err := ProxyList(authKeyInstance(), []attach.Attached{
		{Proxy: jellyfin(), Hostname: "same"},
		{Proxy: everything(), Hostname: "same"},
	})
	if err == nil {
		t.Fatal("ProxyList accepted two proxies with one hostname")
	}
}

func TestTargetURL(t *testing.T) {
	tests := []struct {
		name     string
		protocol v1alpha1.PortProtocol
		target   v1alpha1.PortTarget
		want     string
	}{
		{"https defaults to http", v1alpha1.ProtocolHTTPS, svc("web", 80, ""), "http://web.ns.svc:80"},
		{"http defaults to http", v1alpha1.ProtocolHTTP, svc("web", 80, ""), "http://web.ns.svc:80"},
		{"tcp defaults to tcp", v1alpha1.ProtocolTCP, svc("db", 5432, ""), "tcp://db.ns.svc:5432"},
		{"udp defaults to udp", v1alpha1.ProtocolUDP, svc("dns", 53, ""), "udp://dns.ns.svc:53"},
		{"explicit scheme wins", v1alpha1.ProtocolHTTPS, svc("web", 8443, "https"), "https://web.ns.svc:8443"},
		{"url passes through", v1alpha1.ProtocolHTTPS, v1alpha1.PortTarget{URL: "http://10.0.0.1:1"}, "http://10.0.0.1:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TargetURL("ns", tt.protocol, tt.target); got != tt.want {
				t.Errorf("TargetURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func svc(name string, port int32, scheme string) v1alpha1.PortTarget {
	return v1alpha1.PortTarget{Service: &v1alpha1.ServiceTarget{Name: name, Port: port, Scheme: scheme}}
}
