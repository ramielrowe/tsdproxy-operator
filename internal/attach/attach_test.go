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

package attach

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func instance(allowed *v1alpha1.AllowedNamespaces) *v1alpha1.TSDProxy {
	return &v1alpha1.TSDProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: "tailnet"},
		Spec:       v1alpha1.TSDProxySpec{AllowedNamespaces: allowed},
	}
}

// proxy builds a Proxy referencing tailnet/home, created age seconds after
// the epoch.
func proxy(namespace, name string, age int, mutate ...func(*v1alpha1.Proxy)) v1alpha1.Proxy {
	p := v1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			CreationTimestamp: metav1.NewTime(epoch.Add(time.Duration(age) * time.Second)),
		},
		Spec: v1alpha1.ProxySpec{
			InstanceRef: v1alpha1.InstanceReference{Name: "home", Namespace: "tailnet"},
			Ports: []v1alpha1.ProxyPort{{
				Port:     443,
				Protocol: v1alpha1.ProtocolHTTPS,
				Target:   v1alpha1.PortTarget{URL: "http://10.0.0.1:80"},
			}},
		},
	}
	for _, m := range mutate {
		m(&p)
	}
	return p
}

func noNamespaces(string) (map[string]string, bool) { return nil, false }

func namespaces(byName map[string]map[string]string) NamespaceLabels {
	return func(ns string) (map[string]string, bool) {
		l, ok := byName[ns]
		return l, ok
	}
}

func key(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func hostnames(res Result) []string {
	out := make([]string, 0, len(res.Accepted))
	for _, a := range res.Accepted {
		out = append(out, a.Hostname+"="+a.Proxy.Namespace+"/"+a.Proxy.Name)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustResolve(t *testing.T, inst *v1alpha1.TSDProxy, proxies []v1alpha1.Proxy, ns NamespaceLabels) Result {
	t.Helper()
	if err := SelectorError(inst); err != nil {
		t.Fatalf("SelectorError: %v", err)
	}
	return Resolve(inst, proxies, ns)
}

func expectRejected(t *testing.T, res Result, k types.NamespacedName, reason string) {
	t.Helper()
	got, ok := res.Rejected[k]
	if !ok {
		t.Errorf("%s was not rejected, want reason %s", k, reason)
		return
	}
	if got.Reason != reason {
		t.Errorf("%s rejected with %s (%s), want %s", k, got.Reason, got.Message, reason)
	}
	if got.Message == "" {
		t.Errorf("%s rejected without a message", k)
	}
}

func TestInstanceKeyDefaultsToProxyNamespace(t *testing.T) {
	p := proxy("media", "jellyfin", 0, func(p *v1alpha1.Proxy) { p.Spec.InstanceRef.Namespace = "" })
	if got := InstanceKey(&p); got != key("media", "home") {
		t.Errorf("InstanceKey = %s, want media/home", got)
	}
}

func TestHostnameDefaultsToName(t *testing.T) {
	p := proxy("media", "jellyfin", 0)
	if got := Hostname(&p); got != "jellyfin" {
		t.Errorf("Hostname = %q, want jellyfin", got)
	}
	p.Spec.Hostname = "films"
	if got := Hostname(&p); got != "films" {
		t.Errorf("Hostname = %q, want films", got)
	}
}

func TestSameNamespaceOnlyByDefault(t *testing.T) {
	proxies := []v1alpha1.Proxy{proxy("tailnet", "local", 0), proxy("media", "remote", 1)}

	for name, allowed := range map[string]*v1alpha1.AllowedNamespaces{
		"unset":     nil,
		"from Same": {From: v1alpha1.NamespacesFromSame},
		"empty":     {},
	} {
		t.Run(name, func(t *testing.T) {
			res := mustResolve(t, instance(allowed), proxies, noNamespaces)
			if got, want := hostnames(res), []string{"local=tailnet/local"}; !equal(got, want) {
				t.Errorf("accepted %v, want %v", got, want)
			}
			expectRejected(t, res, key("media", "remote"), v1alpha1.ReasonNamespaceNotAllowed)
		})
	}
}

func TestAllNamespaces(t *testing.T) {
	res := mustResolve(t,
		instance(&v1alpha1.AllowedNamespaces{From: v1alpha1.NamespacesFromAll}),
		[]v1alpha1.Proxy{proxy("tailnet", "local", 0), proxy("media", "remote", 1)},
		noNamespaces)
	if got, want := hostnames(res), []string{"local=tailnet/local", "remote=media/remote"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
	if len(res.Rejected) != 0 {
		t.Errorf("rejected %v, want none", res.Rejected)
	}
}

func TestNamespaceSelector(t *testing.T) {
	inst := instance(&v1alpha1.AllowedNamespaces{
		From:     v1alpha1.NamespacesFromSelector,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tailnet": "home"}},
	})
	proxies := []v1alpha1.Proxy{
		proxy("tailnet", "local", 0),
		proxy("media", "labelled", 1),
		proxy("games", "other-label", 2),
		proxy("ghost", "unknown-namespace", 3),
	}
	ns := namespaces(map[string]map[string]string{
		"tailnet": {}, // the instance's own namespace needs no label
		"media":   {"tailnet": "home"},
		"games":   {"tailnet": "work"},
	})

	res := mustResolve(t, inst, proxies, ns)
	if got, want := hostnames(res), []string{"labelled=media/labelled", "local=tailnet/local"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
	expectRejected(t, res, key("games", "other-label"), v1alpha1.ReasonNamespaceNotAllowed)
	expectRejected(t, res, key("ghost", "unknown-namespace"), v1alpha1.ReasonNamespaceNotAllowed)
}

func TestSelectorWithoutSelectorAdmitsOnlyOwnNamespace(t *testing.T) {
	res := mustResolve(t,
		instance(&v1alpha1.AllowedNamespaces{From: v1alpha1.NamespacesFromSelector}),
		[]v1alpha1.Proxy{proxy("tailnet", "local", 0), proxy("media", "remote", 1)},
		namespaces(map[string]map[string]string{"media": {"any": "label"}}))
	if got, want := hostnames(res), []string{"local=tailnet/local"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
}

// A selector that does not parse must fail closed: it admits no other
// namespace, and the instance's own namespace keeps working.
func TestInvalidSelectorAdmitsOnlyOwnNamespace(t *testing.T) {
	inst := instance(&v1alpha1.AllowedNamespaces{
		From: v1alpha1.NamespacesFromSelector,
		Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "tailnet", Operator: "Bogus"},
		}},
	})
	if err := SelectorError(inst); err == nil {
		t.Fatal("SelectorError accepted an invalid selector")
	}

	res := Resolve(inst,
		[]v1alpha1.Proxy{proxy("tailnet", "local", 0), proxy("media", "remote", 1)},
		namespaces(map[string]map[string]string{"media": {"tailnet": "home"}}))
	if got, want := hostnames(res), []string{"local=tailnet/local"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
	expectRejected(t, res, key("media", "remote"), v1alpha1.ReasonNamespaceNotAllowed)
}

func TestProxiesOfOtherInstancesAreIgnored(t *testing.T) {
	other := proxy("tailnet", "elsewhere", 0, func(p *v1alpha1.Proxy) { p.Spec.InstanceRef.Name = "work" })
	res := mustResolve(t, instance(nil), []v1alpha1.Proxy{other, proxy("tailnet", "mine", 1)}, noNamespaces)
	if got, want := hostnames(res), []string{"mine=tailnet/mine"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
	if _, ok := res.Rejected[key("tailnet", "elsewhere")]; ok {
		t.Error("a Proxy of another instance was reported as rejected")
	}
}

func TestDeletingProxyReleasesItsHostname(t *testing.T) {
	now := metav1.NewTime(epoch)
	deleting := proxy("tailnet", "old", 0, func(p *v1alpha1.Proxy) {
		p.Spec.Hostname = "app"
		p.DeletionTimestamp = &now
	})
	successor := proxy("tailnet", "new", 5, func(p *v1alpha1.Proxy) { p.Spec.Hostname = "app" })

	res := mustResolve(t, instance(nil), []v1alpha1.Proxy{deleting, successor}, noNamespaces)
	if got, want := hostnames(res), []string{"app=tailnet/new"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
}

func TestInvalidHostname(t *testing.T) {
	// Kubernetes names may contain dots; tailnet hostnames may not.
	dotted := proxy("tailnet", "app.v2", 0)
	fixed := proxy("tailnet", "other.v2", 1, func(p *v1alpha1.Proxy) { p.Spec.Hostname = "other-v2" })

	res := mustResolve(t, instance(nil), []v1alpha1.Proxy{dotted, fixed}, noNamespaces)
	expectRejected(t, res, key("tailnet", "app.v2"), v1alpha1.ReasonInvalidHostname)
	if got, want := hostnames(res), []string{"other-v2=tailnet/other.v2"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
}

func TestHostnameConflictOldestWins(t *testing.T) {
	all := &v1alpha1.AllowedNamespaces{From: v1alpha1.NamespacesFromAll}
	newer := proxy("aaa", "app", 10)
	older := proxy("zzz", "app", 5)
	viaHostname := proxy("mmm", "something", 20, func(p *v1alpha1.Proxy) { p.Spec.Hostname = "app" })

	// Input order must not decide the winner.
	for _, proxies := range [][]v1alpha1.Proxy{
		{newer, older, viaHostname},
		{viaHostname, older, newer},
	} {
		res := mustResolve(t, instance(all), proxies, noNamespaces)
		if got, want := hostnames(res), []string{"app=zzz/app"}; !equal(got, want) {
			t.Errorf("accepted %v, want %v", got, want)
		}
		expectRejected(t, res, key("aaa", "app"), v1alpha1.ReasonHostnameConflict)
		expectRejected(t, res, key("mmm", "something"), v1alpha1.ReasonHostnameConflict)
	}
}

func TestHostnameConflictTieBreaksByNamespaceAndName(t *testing.T) {
	all := &v1alpha1.AllowedNamespaces{From: v1alpha1.NamespacesFromAll}
	res := mustResolve(t, instance(all),
		[]v1alpha1.Proxy{proxy("bbb", "app", 0), proxy("aaa", "app", 0)}, noNamespaces)
	if got, want := hostnames(res), []string{"app=aaa/app"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
}

// A Proxy that is rejected for its own reasons must not take a hostname away
// from a valid one.
func TestRejectedProxyDoesNotHoldAHostname(t *testing.T) {
	notAllowed := proxy("media", "app", 0)
	valid := proxy("tailnet", "app", 10)

	res := mustResolve(t, instance(nil), []v1alpha1.Proxy{notAllowed, valid}, noNamespaces)
	if got, want := hostnames(res), []string{"app=tailnet/app"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
	expectRejected(t, res, key("media", "app"), v1alpha1.ReasonNamespaceNotAllowed)
}

func TestServicesModeRejectsUDP(t *testing.T) {
	inst := instance(nil)
	inst.Spec.Tailscale.Mode = v1alpha1.TailscaleModeServices

	udp := proxy("tailnet", "dns", 0, func(p *v1alpha1.Proxy) {
		p.Spec.Ports = append(p.Spec.Ports, v1alpha1.ProxyPort{
			Port: 53, Protocol: v1alpha1.ProtocolUDP, Target: v1alpha1.PortTarget{URL: "udp://10.0.0.1:53"},
		})
	})
	res := mustResolve(t, inst, []v1alpha1.Proxy{udp, proxy("tailnet", "web", 1)}, noNamespaces)
	expectRejected(t, res, key("tailnet", "dns"), v1alpha1.ReasonUnsupported)
	if got, want := hostnames(res), []string{"web=tailnet/web"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}

	// The same Proxy is fine on a PerProxy instance.
	res = mustResolve(t, instance(nil), []v1alpha1.Proxy{udp}, noNamespaces)
	if got, want := hostnames(res), []string{"dns=tailnet/dns"}; !equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
}
