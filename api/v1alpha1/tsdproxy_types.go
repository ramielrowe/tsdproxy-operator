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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TailscaleMode selects how proxies are exposed on the tailnet.
// +kubebuilder:validation:Enum=PerProxy;Services
type TailscaleMode string

const (
	// TailscaleModePerProxy gives every Proxy its own tailnet device.
	TailscaleModePerProxy TailscaleMode = "PerProxy"
	// TailscaleModeServices runs one tailnet device for the instance and
	// exposes every Proxy as a Tailscale VIP Service on it.
	TailscaleModeServices TailscaleMode = "Services"
)

// OAuthCredentials points at a Secret holding a Tailscale OAuth client.
type OAuthCredentials struct {
	// secretName is the name of a Secret in the TSDProxy's namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	SecretName string `json:"secretName"`

	// clientIDKey is the Secret key holding the OAuth client ID.
	// +kubebuilder:default=client_id
	// +kubebuilder:validation:MinLength=1
	// +optional
	ClientIDKey string `json:"clientIDKey,omitempty"`

	// clientSecretKey is the Secret key holding the OAuth client secret.
	// +kubebuilder:default=client_secret
	// +kubebuilder:validation:MinLength=1
	// +optional
	ClientSecretKey string `json:"clientSecretKey,omitempty"`
}

// TailscaleSpec configures the tailnet this instance joins.
//
// +kubebuilder:validation:XValidation:rule="has(self.authKey) != has(self.oauth)",message="exactly one of authKey or oauth must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.oauth) || (has(self.tags) && size(self.tags) > 0)",message="oauth requires at least one tag: Tailscale only issues OAuth auth keys for tagged devices"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'Services' || (has(self.oauth) && has(self.hostname))",message="Services mode requires oauth and hostname"
type TailscaleSpec struct {
	// authKey references a Secret key holding a Tailscale auth key. In
	// PerProxy mode every Proxy registers its own device with this key, so it
	// must be reusable.
	// +optional
	AuthKey *corev1.SecretKeySelector `json:"authKey,omitempty"`

	// oauth references a Secret holding a Tailscale OAuth client. tsdproxy
	// mints a one-time auth key per device from it.
	// +optional
	OAuth *OAuthCredentials `json:"oauth,omitempty"`

	// tags applied to every device of this instance. A Proxy can override
	// them. Required with oauth.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^tag:[a-zA-Z][a-zA-Z0-9-]*$`
	// +listType=set
	// +optional
	Tags []string `json:"tags,omitempty"`

	// controlURL overrides the Tailscale control server, for example to use
	// Headscale. Defaults to Tailscale's hosted control plane.
	// +kubebuilder:validation:Pattern=`^https?://.+$`
	// +optional
	ControlURL string `json:"controlURL,omitempty"`

	// mode selects how proxies are exposed on the tailnet.
	// +kubebuilder:default=PerProxy
	// +optional
	Mode TailscaleMode `json:"mode,omitempty"`

	// hostname is the tailnet hostname of the instance's own device. Only
	// used, and required, in Services mode.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// ephemeral is the default for whether proxy devices register as
	// ephemeral nodes, which Tailscale removes shortly after they go offline.
	// A Proxy can override it.
	// +optional
	Ephemeral bool `json:"ephemeral,omitempty"`

	// preventDuplicates makes tsdproxy delete a stale tailnet device with the
	// same hostname before registering a new one. Requires oauth.
	// +optional
	PreventDuplicates bool `json:"preventDuplicates,omitempty"`

	// autoApproveDevices approves the instance's device automatically.
	// Services mode only.
	// +optional
	AutoApproveDevices bool `json:"autoApproveDevices,omitempty"`

	// autoRemoveConflicts removes conflicting VIP Services automatically.
	// Services mode only.
	// +optional
	AutoRemoveConflicts bool `json:"autoRemoveConflicts,omitempty"`

	// autoProvisionACL lets tsdproxy add the tags to the tailnet policy's
	// tagOwners. Requires oauth with policy scopes.
	// +optional
	AutoProvisionACL bool `json:"autoProvisionACL,omitempty"`
}

// FromNamespaces selects which namespaces may attach Proxies to an instance.
// +kubebuilder:validation:Enum=Same;All;Selector
type FromNamespaces string

const (
	// NamespacesFromSame only admits Proxies from the instance's own namespace.
	NamespacesFromSame FromNamespaces = "Same"
	// NamespacesFromAll admits Proxies from every namespace.
	NamespacesFromAll FromNamespaces = "All"
	// NamespacesFromSelector admits Proxies from namespaces matching selector.
	NamespacesFromSelector FromNamespaces = "Selector"
)

// AllowedNamespaces controls which namespaces may attach Proxies.
//
// +kubebuilder:validation:XValidation:rule="!has(self.from) || self.from != 'Selector' || has(self.selector)",message="selector is required when from is Selector"
type AllowedNamespaces struct {
	// from selects the namespaces that may attach Proxies to this instance.
	// +kubebuilder:default=Same
	// +optional
	From FromNamespaces `json:"from,omitempty"`

	// selector matches namespace labels. Only used when from is Selector. The
	// instance's own namespace is always allowed.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// StorageType selects what backs the tsdproxy data directory.
// +kubebuilder:validation:Enum=PersistentVolumeClaim;EmptyDir
type StorageType string

const (
	// StorageTypePersistentVolumeClaim keeps tailnet device identities across
	// pod restarts.
	StorageTypePersistentVolumeClaim StorageType = "PersistentVolumeClaim"
	// StorageTypeEmptyDir loses device identities on every pod restart, so
	// every proxy re-registers. Combine it with ephemeral devices.
	StorageTypeEmptyDir StorageType = "EmptyDir"
)

// StorageSpec configures the tsdproxy data directory, which holds the
// Tailscale state of every device.
type StorageSpec struct {
	// type selects what backs the data directory.
	// +kubebuilder:default=PersistentVolumeClaim
	// +optional
	Type StorageType `json:"type,omitempty"`

	// size of the PersistentVolumeClaim.
	// +kubebuilder:default="1Gi"
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// storageClassName of the PersistentVolumeClaim. Unset uses the cluster
	// default.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// LogSpec configures tsdproxy logging.
type LogSpec struct {
	// level is the tsdproxy log level.
	// +kubebuilder:validation:Enum=trace;debug;info;warn;error
	// +kubebuilder:default=info
	// +optional
	Level string `json:"level,omitempty"`

	// json switches tsdproxy to JSON log output.
	// +optional
	JSON bool `json:"json,omitempty"`
}

// TSDProxySpec defines the desired state of a tsdproxy instance. One instance
// joins one tailnet.
//
// +kubebuilder:validation:XValidation:rule="has(self.storage) == has(oldSelf.storage) && (!has(self.storage) || self.storage == oldSelf.storage)",message="storage is immutable"
type TSDProxySpec struct {
	// image is the tsdproxy container image. The operator renders
	// configuration for tsdproxy v3.
	// +kubebuilder:default="ghcr.io/almeidapaulopt/tsdproxy:3.0.0-beta.4"
	// +kubebuilder:validation:MinLength=1
	// +optional
	Image string `json:"image,omitempty"`

	// imagePullPolicy for the tsdproxy container.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// tailscale configures the tailnet this instance joins.
	// +required
	Tailscale TailscaleSpec `json:"tailscale"`

	// allowedNamespaces controls which namespaces may attach Proxies to this
	// instance. Defaults to the instance's own namespace only.
	// +optional
	AllowedNamespaces *AllowedNamespaces `json:"allowedNamespaces,omitempty"`

	// storage configures the data directory. Immutable once set.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	// log configures tsdproxy logging.
	// +optional
	Log *LogSpec `json:"log,omitempty"`

	// admins lists Tailscale user IDs allowed to use admin actions in the
	// tsdproxy dashboard. When empty, every tailnet user reaching the
	// dashboard is an admin.
	// +listType=set
	// +optional
	Admins []string `json:"admins,omitempty"`

	// resources of the tsdproxy container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// nodeSelector of the tsdproxy pod.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations of the tsdproxy pod.
	// +listType=atomic
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// TSDProxyStatus defines the observed state of TSDProxy.
type TSDProxyStatus struct {
	// observedGeneration is the generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// attachedProxies is the number of Proxies rendered into this instance's
	// proxy list.
	// +optional
	AttachedProxies int32 `json:"attachedProxies"`

	// configHash identifies the startup configuration the pod was last
	// rolled for. It changes when the instance restarts, not when proxies
	// are added or removed.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// conditions represent the current state of the TSDProxy resource.
	//
	// Condition types:
	// - "Ready": the tsdproxy pod is running with the current configuration
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=tsdproxies,singular=tsdproxy
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.tailscale.mode`
// +kubebuilder:printcolumn:name="Proxies",type=integer,JSONPath=`.status.attachedProxies`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="metadata.name must be at most 40 characters: it prefixes the names of the StatefulSet, Service and pod"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must be a DNS label"

// TSDProxy is one tsdproxy instance joined to one tailnet.
type TSDProxy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of TSDProxy
	// +required
	Spec TSDProxySpec `json:"spec"`

	// status defines the observed state of TSDProxy
	// +optional
	Status TSDProxyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TSDProxyList contains a list of TSDProxy
type TSDProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TSDProxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TSDProxy{}, &TSDProxyList{})
		return nil
	})
}
