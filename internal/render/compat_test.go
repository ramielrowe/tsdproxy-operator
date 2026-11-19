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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/attach"
)

// compatPort and compatProxy are what a Proxy resource asks tsdproxy for,
// derived from the resource and not from this package's rendering structs,
// so a rendering mistake cannot hide itself in the expectations.
type compatPort struct {
	Protocol    string `json:"protocol"`
	Port        int    `json:"port"`
	Target      string `json:"target"`
	TLSValidate bool   `json:"tlsValidate"`
	IsRedirect  bool   `json:"isRedirect"`
	Funnel      bool   `json:"funnel"`
}

type compatProxy struct {
	Ports        map[string]compatPort `json:"ports"`
	Tags         string                `json:"tags"`
	Icon         string                `json:"icon"`
	Label        string                `json:"label"`
	Category     string                `json:"category"`
	Ephemeral    bool                  `json:"ephemeral"`
	RunWebClient bool                  `json:"runWebClient"`
	Verbose      bool                  `json:"verbose"`
}

const tsdproxyModule = "github.com/almeidapaulopt/tsdproxy"

// tsdproxySource returns a directory holding tsdproxy's source.
//
// By default that is the version pinned in testdata/tsdproxy/go.mod, which
// the go command downloads into the module cache and verifies against that
// module's go.sum. TSDPROXY_SRC overrides it with a local checkout, to check
// the operator against a tsdproxy that is not released yet.
func tsdproxySource(t *testing.T) string {
	t.Helper()

	if src := os.Getenv("TSDPROXY_SRC"); src != "" {
		abs, err := filepath.Abs(src)
		if err != nil {
			t.Fatalf("resolve TSDPROXY_SRC: %v", err)
		}
		t.Logf("tsdproxy from TSDPROXY_SRC=%s", abs)
		return abs
	}

	cmd := exec.Command("go", "mod", "download", "-json", tsdproxyModule)
	cmd.Dir = filepath.Join("testdata", "tsdproxy")
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()

	// On failure the go command still prints JSON, with the reason in Error.
	var mod struct{ Version, Dir, Error string }
	if err := json.Unmarshal(out, &mod); err != nil || mod.Dir == "" {
		t.Fatalf("download %s pinned in %s/go.mod: %v %s %s (run with -short to skip)",
			tsdproxyModule, cmd.Dir, runErr, mod.Error, stderr.String())
	}
	t.Logf("tsdproxy %s from the module cache", mod.Version)
	return mod.Dir
}

// copyTSDProxy copies the part of tsdproxy the checks compile into dst: the
// module files and the internal tree. The copy is what makes it possible to
// add test files: the module cache is read-only, and the go command refuses
// build overlays beneath it.
func copyTSDProxy(t *testing.T, src, dst string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(src, "internal", "config", "config.go")); err != nil {
		t.Fatalf("%s does not look like a tsdproxy source tree: %v", src, err)
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		copyFile(t, filepath.Join(src, name), filepath.Join(dst, name))
	}
	internal := filepath.Join(src, "internal")
	err := filepath.WalkDir(internal, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		copyFile(t, path, filepath.Join(dst, rel))
		return nil
	})
	if err != nil {
		t.Fatalf("copy tsdproxy source: %v", err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, to, string(data))
}

// TestTSDProxyCompat feeds the rendered files to tsdproxy's own configuration
// loader, validator and list provider. tsdproxy keeps that code in internal
// packages, which Go does not let another module import, so the checks are
// test files added to a temporary copy of tsdproxy's source and run there.
func TestTSDProxyCompat(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads and compiles tsdproxy packages")
	}
	src := tsdproxySource(t)
	dir := t.TempDir()

	// Stand-ins for the volumes the pod mounts at the paths the
	// configuration refers to.
	rewrite := strings.NewReplacer(
		ConfigDir+"/", filepath.Join(dir, "config")+"/",
		TailscaleSecretDir+"/", filepath.Join(dir, "tailscale")+"/",
		APIKeyDir+"/", filepath.Join(dir, "api")+"/",
		"dataDir: "+DataDir, "dataDir: "+filepath.Join(dir, "data"),
	)
	writeFile(t, filepath.Join(dir, "tailscale", AuthKeyFileName), "compat-authkey\n")
	writeFile(t, filepath.Join(dir, "tailscale", ClientSecretFileName), "compat-client-secret\n")
	writeFile(t, filepath.Join(dir, "api", APIKeyFileName), "compat-api-key\n")
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	inst := authKeyInstance()
	attached := []attach.Attached{
		{Proxy: everything(), Hostname: "all"},
		{Proxy: jellyfin(), Hostname: "jellyfin"},
	}
	list, err := ProxyList(inst, attached)
	if err != nil {
		t.Fatalf("ProxyList: %v", err)
	}
	writeFile(t, filepath.Join(dir, "proxies.yaml"), string(list))
	// The main configuration's list filename must exist for tsdproxy's
	// validator, at the rewritten /config path.
	writeFile(t, filepath.Join(dir, "config", ProxyListFile), string(list))

	expect := map[string]compatProxy{}
	for _, a := range attached {
		expect[a.Hostname] = expectation(inst, a.Proxy)
	}
	raw, err := json.Marshal(expect)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "expect.json"), string(raw))

	for name, instance := range map[string]*v1alpha1.TSDProxy{
		"main-authkey.yaml": authKeyInstance(),
		"main-oauth.yaml":   servicesInstance(),
	} {
		cfg, err := MainConfig(instance)
		if err != nil {
			t.Fatalf("MainConfig %s: %v", name, err)
		}
		writeFile(t, filepath.Join(dir, name), rewrite.Replace(string(cfg)))
	}

	work := filepath.Join(dir, "tsdproxy")
	copyTSDProxy(t, src, work)
	copyFile(t, filepath.Join("testdata", "compat", "config_compat_test.go.txt"),
		filepath.Join(work, "internal", "config", "zz_operator_compat_test.go"))
	copyFile(t, filepath.Join("testdata", "compat", "list_compat_test.go.txt"),
		filepath.Join(work, "internal", "targetproviders", "list", "zz_operator_compat_test.go"))

	cmd := exec.Command("go", "test", "-count=1", "-run", "^TestOperatorCompat$", "-v",
		"./internal/config/", "./internal/targetproviders/list/")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "OPERATOR_COMPAT_DIR="+dir, "GOFLAGS=-mod=readonly", "GOWORK=off")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	t.Logf("tsdproxy compat run:\n%s", out.String())
	if runErr != nil {
		t.Fatalf("tsdproxy rejected the rendered configuration: %v", runErr)
	}

	// A skipped or unmatched inner test would also exit zero.
	for _, want := range []string{
		"--- PASS: TestOperatorCompat/main-authkey.yaml",
		"--- PASS: TestOperatorCompat/main-oauth.yaml",
		"--- PASS: TestOperatorCompat/all",
		"--- PASS: TestOperatorCompat/jellyfin",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tsdproxy compat output is missing %q", want)
		}
	}
}

func expectation(inst *v1alpha1.TSDProxy, p *v1alpha1.Proxy) compatProxy {
	want := compatProxy{
		Ports:     map[string]compatPort{},
		Ephemeral: inst.Spec.Tailscale.Ephemeral,
		// tsdproxy's default when the list entry names no icon.
		Icon: "tsdproxy",
	}
	if ts := p.Spec.Tailscale; ts != nil {
		want.Tags = strings.Join(ts.Tags, ",")
		if ts.Ephemeral != nil {
			want.Ephemeral = *ts.Ephemeral
		}
		want.RunWebClient = ts.RunWebClient
		want.Verbose = ts.Verbose
	}
	if d := p.Spec.Dashboard; d != nil {
		want.Label, want.Category = d.Label, d.Category
		if d.Icon != "" {
			want.Icon = d.Icon
		}
	}
	for _, port := range p.Spec.Ports {
		target := port.Target.URL
		if s := port.Target.Service; s != nil {
			scheme := s.Scheme
			if scheme == "" {
				scheme = map[v1alpha1.PortProtocol]string{
					v1alpha1.ProtocolHTTPS: "http", v1alpha1.ProtocolHTTP: "http",
					v1alpha1.ProtocolTCP: "tcp", v1alpha1.ProtocolUDP: "udp",
				}[port.Protocol]
			}
			target = fmt.Sprintf("%s://%s.%s.svc:%d", scheme, s.Name, p.Namespace, s.Port)
		}
		want.Ports[fmt.Sprintf("%d/%s", port.Port, port.Protocol)] = compatPort{
			Protocol:    string(port.Protocol),
			Port:        int(port.Port),
			Target:      target,
			TLSValidate: !port.InsecureSkipTLSVerify,
			IsRedirect:  port.Redirect,
			Funnel:      port.Funnel,
		}
	}
	return want
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
