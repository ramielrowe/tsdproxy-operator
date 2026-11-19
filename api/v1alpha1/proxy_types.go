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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types and reasons reported on a Proxy.
const (
	// ConditionAccepted reports whether the Proxy is rendered into its
	// instance's proxy list.
	ConditionAccepted = "Accepted"
	// ConditionReady reports whether the workload behind the resource is
	// serving: the tsdproxy pod for a TSDProxy, the tailnet proxy for a Proxy.
	ConditionReady = "Ready"

	ReasonAccepted            = "Accepted"
	ReasonInstanceNotFound    = "InstanceNotFound"
	ReasonNamespaceNotAllowed = "NamespaceNotAllowed"
	ReasonInvalidHostname     = "InvalidHostname"
	ReasonHostnameConflict    = "HostnameConflict"
	ReasonUnsupported         = "Unsupported"
)

// InstanceReference names the TSDProxy a Proxy attaches to.
type InstanceReference struct {
	// name of the TSDProxy.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// namespace of the TSDProxy. Defaults to the Proxy's namespace. Another
	// namespace only works when that TSDProxy's allowedNamespaces admits this
	// one.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PortProtocol is the protocol a proxy port speaks on the tailnet.
// +kubebuilder:validation:Enum=https;http;tcp;udp
type PortProtocol string

const (
	ProtocolHTTPS PortProtocol = "https"
	ProtocolHTTP  PortProtocol = "http"
	ProtocolTCP   PortProtocol = "tcp"
	ProtocolUDP   PortProtocol = "udp"
)

// ServiceTarget points a port at a Service in the Proxy's namespace.
type ServiceTarget struct {
	// name of the Service, in the same namespace as the Proxy.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// port number of the Service.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	Port int32 `json:"port"`

	// scheme used to reach the Service. Defaults to http for https and http
	// ports, tcp for tcp ports and udp for udp ports.
	// +kubebuilder:validation:Enum=http;https;tcp;udp
	// +optional
	Scheme string `json:"scheme,omitempty"`
}

// PortTarget is where a port sends traffic.
//
// +kubebuilder:validation:XValidation:rule="has(self.service) != has(self.url)",message="exactly one of service or url must be set"
type PortTarget struct {
	// service targets a Service in the Proxy's namespace.
	// +optional
	Service *ServiceTarget `json:"service,omitempty"`

	// url targets an arbitrary URL, for example http://192.168.1.10:8080 or
	// tcp://db.internal:5432.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9+.-]*://[^/\s]+.*$`
	// +optional
	URL string `json:"url,omitempty"`
}

// ProxyPort exposes one port on the proxy's tailnet hostname.
//
// +kubebuilder:validation:XValidation:rule="!has(self.funnel) || !self.funnel || !has(self.protocol) || self.protocol == 'https'",message="funnel requires the https protocol"
// +kubebuilder:validation:XValidation:rule="!has(self.redirect) || !self.redirect || has(self.target.url)",message="redirect requires a url target"
type ProxyPort struct {
	// port number exposed on the tailnet.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +required
	Port int32 `json:"port"`

	// protocol spoken on the tailnet.
	// +kubebuilder:default=https
	// +optional
	Protocol PortProtocol `json:"protocol,omitempty"`

	// target is where traffic is sent.
	// +required
	Target PortTarget `json:"target"`

	// redirect answers requests with an HTTP redirect to the target URL
	// instead of proxying.
	// +optional
	Redirect bool `json:"redirect,omitempty"`

	// funnel exposes this port to the public internet through Tailscale
	// Funnel. Only valid for https.
	// +optional
	Funnel bool `json:"funnel,omitempty"`

	// insecureSkipTLSVerify disables verification of the target's TLS
	// certificate when the target is https.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// ProxyTailscale holds per-proxy Tailscale settings.
type ProxyTailscale struct {
	// tags for this proxy's device, replacing the instance's tags.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^tag:[a-zA-Z][a-zA-Z0-9-]*$`
	// +listType=set
	// +optional
	Tags []string `json:"tags,omitempty"`

	// ephemeral overrides the instance default for registering this proxy's
	// device as an ephemeral node.
	// +optional
	Ephemeral *bool `json:"ephemeral,omitempty"`

	// runWebClient serves the Tailscale web client on the device.
	// +optional
	RunWebClient bool `json:"runWebClient,omitempty"`

	// verbose enables verbose Tailscale logging for this proxy.
	// +optional
	Verbose bool `json:"verbose,omitempty"`
}

// ProxyDashboard controls how the proxy shows up in the tsdproxy dashboard.
type ProxyDashboard struct {
	// label shown in the dashboard. Defaults to the hostname.
	// +optional
	Label string `json:"label,omitempty"`

	// icon shown in the dashboard, for example "sh/jellyfin".
	// +optional
	Icon string `json:"icon,omitempty"`

	// category groups proxies in the dashboard.
	// +optional
	Category string `json:"category,omitempty"`
}

// ProxySpec defines one hostname exposed on a tailnet.
type ProxySpec struct {
	// instanceRef names the TSDProxy this proxy attaches to.
	// +required
	InstanceRef InstanceReference `json:"instanceRef"`

	// hostname on the tailnet. Defaults to the Proxy's name. Must be unique
	// among the Proxies attached to the same instance; when two collide, the
	// oldest keeps the hostname.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// ports exposed on the tailnet hostname.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=port
	// +listMapKey=protocol
	// +required
	Ports []ProxyPort `json:"ports"`

	// tailscale holds per-proxy Tailscale settings.
	// +optional
	Tailscale *ProxyTailscale `json:"tailscale,omitempty"`

	// dashboard controls how the proxy shows up in the tsdproxy dashboard.
	// +optional
	Dashboard *ProxyDashboard `json:"dashboard,omitempty"`
}

// ProxyStatus defines the observed state of Proxy.
type ProxyStatus struct {
	// observedGeneration is the generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// hostname is the tailnet hostname in effect.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// url is the proxy's tailnet URL as reported by tsdproxy. While the
	// device waits for interactive login it is the login URL instead.
	// +optional
	URL string `json:"url,omitempty"`

	// state is the proxy state reported by tsdproxy, for example Running or
	// Authenticating.
	// +optional
	State string `json:"state,omitempty"`

	// health is the backend health reported by tsdproxy.
	// +optional
	Health string `json:"health,omitempty"`

	// conditions represent the current state of the Proxy resource.
	//
	// Condition types:
	// - "Accepted": the proxy is rendered into its instance's proxy list
	// - "Ready": tsdproxy reports the proxy as running
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Proxy exposes one hostname on the tailnet of a TSDProxy instance.
type Proxy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Proxy
	// +required
	Spec ProxySpec `json:"spec"`

	// status defines the observed state of Proxy
	// +optional
	Status ProxyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProxyList contains a list of Proxy
type ProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Proxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Proxy{}, &ProxyList{})
		return nil
	})
}
