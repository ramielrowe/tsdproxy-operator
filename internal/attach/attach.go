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

// Package attach decides which Proxies are rendered into a TSDProxy
// instance's proxy list. Both controllers call Resolve with the same inputs,
// so the instance's rendered list and each Proxy's Accepted condition agree.
package attach

import (
	"cmp"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
)

// Attached is a Proxy admitted to an instance, with its hostname in effect.
type Attached struct {
	Proxy    *v1alpha1.Proxy
	Hostname string
}

// Rejection explains why a Proxy is not rendered.
type Rejection struct {
	Reason  string
	Message string
}

// Result is the outcome of resolving the Proxies that reference an instance.
type Result struct {
	// Accepted is sorted by hostname.
	Accepted []Attached
	// Rejected is keyed by the Proxy's namespace and name.
	Rejected map[types.NamespacedName]Rejection
}

// NamespaceLabels returns the labels of a namespace, and false when the
// namespace is unknown.
type NamespaceLabels func(namespace string) (map[string]string, bool)

// InstanceKey returns the namespace and name of the TSDProxy a Proxy
// references.
func InstanceKey(p *v1alpha1.Proxy) types.NamespacedName {
	ns := p.Spec.InstanceRef.Namespace
	if ns == "" {
		ns = p.Namespace
	}
	return types.NamespacedName{Namespace: ns, Name: p.Spec.InstanceRef.Name}
}

// Hostname returns the tailnet hostname a Proxy asks for.
func Hostname(p *v1alpha1.Proxy) string {
	if p.Spec.Hostname != "" {
		return p.Spec.Hostname
	}
	return p.Name
}

// Resolve decides, for the Proxies referencing inst, which are rendered and
// why the others are not. Proxies referencing another instance are ignored.
//
// An allowedNamespaces selector that does not parse admits no other
// namespace; SelectorError reports the problem.
func Resolve(inst *v1alpha1.TSDProxy, proxies []v1alpha1.Proxy, nsLabels NamespaceLabels) Result {
	res := Result{Rejected: map[types.NamespacedName]Rejection{}}
	instKey := types.NamespacedName{Namespace: inst.Namespace, Name: inst.Name}

	selector, err := namespaceSelector(inst)
	if err != nil {
		selector = labels.Nothing()
	}

	// candidates passed every check that only concerns the Proxy itself and
	// now compete for their hostname.
	byHostname := map[string][]*v1alpha1.Proxy{}
	for i := range proxies {
		p := &proxies[i]
		if InstanceKey(p) != instKey || !p.DeletionTimestamp.IsZero() {
			continue
		}
		key := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}

		if !namespaceAllowed(inst, p.Namespace, selector, nsLabels) {
			res.Rejected[key] = Rejection{
				Reason: v1alpha1.ReasonNamespaceNotAllowed,
				Message: fmt.Sprintf("TSDProxy %s does not allow Proxies from namespace %q",
					instKey, p.Namespace),
			}
			continue
		}

		hostname := Hostname(p)
		if errs := validation.IsDNS1123Label(hostname); len(errs) > 0 {
			res.Rejected[key] = Rejection{
				Reason: v1alpha1.ReasonInvalidHostname,
				Message: fmt.Sprintf("hostname %q is not a valid DNS label (%s); set spec.hostname",
					hostname, errs[0]),
			}
			continue
		}

		if msg := unsupported(inst, p); msg != "" {
			res.Rejected[key] = Rejection{Reason: v1alpha1.ReasonUnsupported, Message: msg}
			continue
		}

		byHostname[hostname] = append(byHostname[hostname], p)
	}

	for hostname, contenders := range byHostname {
		slices.SortFunc(contenders, byAge)
		winner := contenders[0]
		res.Accepted = append(res.Accepted, Attached{Proxy: winner, Hostname: hostname})
		for _, loser := range contenders[1:] {
			res.Rejected[types.NamespacedName{Namespace: loser.Namespace, Name: loser.Name}] = Rejection{
				Reason: v1alpha1.ReasonHostnameConflict,
				Message: fmt.Sprintf("hostname %q is already held by the older Proxy %s/%s",
					hostname, winner.Namespace, winner.Name),
			}
		}
	}
	slices.SortFunc(res.Accepted, func(a, b Attached) int { return cmp.Compare(a.Hostname, b.Hostname) })

	return res
}

// SelectorError returns why the instance's allowedNamespaces selector cannot
// be used, or nil.
func SelectorError(inst *v1alpha1.TSDProxy) error {
	_, err := namespaceSelector(inst)
	return err
}

func namespaceSelector(inst *v1alpha1.TSDProxy) (labels.Selector, error) {
	allowed := inst.Spec.AllowedNamespaces
	if allowed == nil || allowed.From != v1alpha1.NamespacesFromSelector {
		return nil, nil
	}
	if allowed.Selector == nil {
		return labels.Nothing(), nil
	}
	selector, err := metav1.LabelSelectorAsSelector(allowed.Selector)
	if err != nil {
		return nil, fmt.Errorf("allowedNamespaces.selector: %w", err)
	}
	return selector, nil
}

func namespaceAllowed(inst *v1alpha1.TSDProxy, namespace string, selector labels.Selector, nsLabels NamespaceLabels) bool {
	if namespace == inst.Namespace {
		return true
	}
	allowed := inst.Spec.AllowedNamespaces
	if allowed == nil {
		return false
	}
	switch allowed.From {
	case v1alpha1.NamespacesFromAll:
		return true
	case v1alpha1.NamespacesFromSelector:
		set, ok := nsLabels(namespace)
		return ok && selector.Matches(labels.Set(set))
	default:
		return false
	}
}

// unsupported reports a Proxy feature the instance's mode cannot serve.
// tsdproxy would refuse to start such a proxy; rejecting it here puts the
// reason on the Proxy instead of in the tsdproxy log.
func unsupported(inst *v1alpha1.TSDProxy, p *v1alpha1.Proxy) string {
	if inst.Spec.Tailscale.Mode != v1alpha1.TailscaleModeServices {
		return ""
	}
	for _, port := range p.Spec.Ports {
		if port.Protocol == v1alpha1.ProtocolUDP {
			return fmt.Sprintf("port %d/udp: Services mode does not support UDP", port.Port)
		}
	}
	return ""
}

// byAge orders Proxies by creation time, then by namespace and name so that
// the order is total and both controllers pick the same winner.
func byAge(a, b *v1alpha1.Proxy) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
}
