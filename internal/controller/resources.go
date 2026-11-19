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

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/render"
)

const (
	// ConfigHashAnnotation on the pod template identifies the startup
	// configuration. tsdproxy reads its main configuration and credentials
	// once, so a new hash is what rolls the pod.
	ConfigHashAnnotation = "tsdproxy-operator.apmelton.com/config-hash"

	// ProxiesHashAnnotation on the running pod identifies the proxy list it
	// was last told about. Changing a pod annotation makes the kubelet sync
	// the pod right away, which refreshes the mounted ConfigMap without
	// waiting for the kubelet's periodic sync.
	ProxiesHashAnnotation = "tsdproxy-operator.apmelton.com/proxies-hash"

	defaultImage       = "ghcr.io/almeidapaulopt/tsdproxy:3.0.0-beta.4"
	defaultStorageSize = "1Gi"

	defaultClientIDKey     = "client_id"
	defaultClientSecretKey = "client_secret"

	containerName = "tsdproxy"
	httpPortName  = "http"

	volumeConfig      = "config"
	volumeData        = "data"
	volumeCredentials = "tailscale-credentials"
	volumeAPIKey      = "api-key"
	volumeTmp         = "tmp"

	// tsdproxy ships in a scratch image without a passwd file, so the pod
	// picks an arbitrary unprivileged user. fsGroup makes the data volume
	// and the mounted Secrets usable by it.
	runAsID = 65532

	secretFileMode = 0o440
)

// childName is the name shared by the instance's StatefulSet, Service and
// ConfigMap.
func childName(inst *v1alpha1.TSDProxy) string {
	return "tsdproxy-" + inst.Name
}

// apiKeySecretName is the Secret holding the operator-generated API key.
func apiKeySecretName(inst *v1alpha1.TSDProxy) string {
	return childName(inst) + "-api"
}

// podKey is the single pod of the instance's StatefulSet.
func podKey(inst *v1alpha1.TSDProxy) types.NamespacedName {
	return types.NamespacedName{Namespace: inst.Namespace, Name: childName(inst) + "-0"}
}

// apiBaseURL is where the operator reaches the instance's REST API.
func apiBaseURL(inst *v1alpha1.TSDProxy) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", childName(inst), inst.Namespace, render.HTTPPort)
}

// credentialsSecretName is the user's Secret holding the Tailscale auth key
// or OAuth client, or "" when the spec names none.
func credentialsSecretName(inst *v1alpha1.TSDProxy) string {
	ts := inst.Spec.Tailscale
	switch {
	case ts.OAuth != nil:
		return ts.OAuth.SecretName
	case ts.AuthKey != nil:
		return ts.AuthKey.Name
	default:
		return ""
	}
}

// credentialKeys lists the keys the instance needs from its credentials
// Secret.
func credentialKeys(inst *v1alpha1.TSDProxy) []string {
	ts := inst.Spec.Tailscale
	switch {
	case ts.OAuth != nil:
		return []string{oauthClientIDKey(ts.OAuth), oauthClientSecretKey(ts.OAuth)}
	case ts.AuthKey != nil:
		return []string{ts.AuthKey.Key}
	default:
		return nil
	}
}

func oauthClientIDKey(o *v1alpha1.OAuthCredentials) string {
	if o.ClientIDKey != "" {
		return o.ClientIDKey
	}
	return defaultClientIDKey
}

func oauthClientSecretKey(o *v1alpha1.OAuthCredentials) string {
	if o.ClientSecretKey != "" {
		return o.ClientSecretKey
	}
	return defaultClientSecretKey
}

func selectorLabels(inst *v1alpha1.TSDProxy) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "tsdproxy",
		"app.kubernetes.io/instance": inst.Name,
	}
}

func childLabels(inst *v1alpha1.TSDProxy) map[string]string {
	l := selectorLabels(inst)
	l["app.kubernetes.io/managed-by"] = "tsdproxy-operator"
	return l
}

// hashOf returns a hex digest over the given parts. Each part is length
// prefixed so that moving bytes between parts changes the digest.
func hashOf(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(p))
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func servicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{{
		Name:       httpPortName,
		Protocol:   corev1.ProtocolTCP,
		Port:       render.HTTPPort,
		TargetPort: intstr.FromString(httpPortName),
	}}
}

func usesPVC(inst *v1alpha1.TSDProxy) bool {
	return inst.Spec.Storage == nil || inst.Spec.Storage.Type != v1alpha1.StorageTypeEmptyDir
}

// dataVolumeClaim is the claim template for the data directory. StatefulSet
// claim templates are immutable, so it is only applied on creation.
func dataVolumeClaim(inst *v1alpha1.TSDProxy) corev1.PersistentVolumeClaim {
	size := resource.MustParse(defaultStorageSize)
	var class *string
	if s := inst.Spec.Storage; s != nil {
		if s.Size != nil {
			size = *s.Size
		}
		class = s.StorageClassName
	}
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeData, Labels: childLabels(inst)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
}

// podTemplate builds the tsdproxy pod. configHash covers everything tsdproxy
// only reads at startup.
func podTemplate(inst *v1alpha1.TSDProxy, configHash string) corev1.PodTemplateSpec {
	image := inst.Spec.Image
	if image == "" {
		image = defaultImage
	}

	volumes := []corev1.Volume{
		{
			Name: volumeConfig,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: childName(inst)},
			}},
		},
		{
			Name: volumeAPIKey,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  apiKeySecretName(inst),
				DefaultMode: ptr.To[int32](secretFileMode),
			}},
		},
		{Name: volumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if !usesPVC(inst) {
		volumes = append(volumes, corev1.Volume{
			Name:         volumeData,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	// The credentials Secret is projected onto the fixed file names the
	// rendered configuration refers to, whatever the user's keys are called.
	var env []corev1.EnvVar
	credentials := &corev1.SecretVolumeSource{
		SecretName:  credentialsSecretName(inst),
		DefaultMode: ptr.To[int32](secretFileMode),
	}
	switch ts := inst.Spec.Tailscale; {
	case ts.OAuth != nil:
		credentials.Items = []corev1.KeyToPath{
			{Key: oauthClientSecretKey(ts.OAuth), Path: render.ClientSecretFileName},
		}
		env = append(env, corev1.EnvVar{
			Name: render.ClientIDEnv,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: ts.OAuth.SecretName},
				Key:                  oauthClientIDKey(ts.OAuth),
			}},
		})
	case ts.AuthKey != nil:
		credentials.Items = []corev1.KeyToPath{{Key: ts.AuthKey.Key, Path: render.AuthKeyFileName}}
	}
	volumes = append(volumes, corev1.Volume{
		Name:         volumeCredentials,
		VolumeSource: corev1.VolumeSource{Secret: credentials},
	})

	ready := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: "/health/ready/",
		Port: intstr.FromString(httpPortName),
	}}

	container := corev1.Container{
		Name:            containerName,
		Image:           image,
		ImagePullPolicy: inst.Spec.ImagePullPolicy,
		Args:            []string{"--config", render.ConfigDir + "/" + render.MainConfigFile},
		Env:             env,
		Ports: []corev1.ContainerPort{{
			Name:          httpPortName,
			ContainerPort: render.HTTPPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources: inst.Spec.Resources,
		VolumeMounts: []corev1.VolumeMount{
			// Mounted as a directory, never through subPath: tsdproxy
			// notices list updates through the ConfigMap volume's symlink
			// swap, and subPath mounts are never updated.
			{Name: volumeConfig, MountPath: render.ConfigDir, ReadOnly: true},
			{Name: volumeData, MountPath: render.DataDir},
			{Name: volumeCredentials, MountPath: render.TailscaleSecretDir, ReadOnly: true},
			{Name: volumeAPIKey, MountPath: render.APIKeyDir, ReadOnly: true},
			{Name: volumeTmp, MountPath: "/tmp"},
		},
		// tsdproxy reports ready once its providers are wired up, which
		// with OAuth includes calls to the Tailscale API.
		StartupProbe: &corev1.Probe{
			ProbeHandler:     ready,
			PeriodSeconds:    2,
			FailureThreshold: 90,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  ready,
			PeriodSeconds: 10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromString(httpPortName),
			}},
			PeriodSeconds: 20,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      childLabels(inst),
			Annotations: map[string]string{ConfigHashAnnotation: configHash},
		},
		Spec: corev1.PodSpec{
			Containers:   []corev1.Container{container},
			Volumes:      volumes,
			NodeSelector: inst.Spec.NodeSelector,
			Tolerations:  inst.Spec.Tolerations,
			// The pod never talks to the Kubernetes API.
			AutomountServiceAccountToken: new(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   new(true),
				RunAsUser:      ptr.To[int64](runAsID),
				RunAsGroup:     ptr.To[int64](runAsID),
				FSGroup:        ptr.To[int64](runAsID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		},
	}
}

// mutateStatefulSet sets the fields the operator owns on a new or existing
// StatefulSet. It leaves server-populated and immutable fields alone.
func mutateStatefulSet(sts *appsv1.StatefulSet, inst *v1alpha1.TSDProxy, configHash string) {
	sts.Labels = mergeLabels(sts.Labels, childLabels(inst))

	if sts.CreationTimestamp.IsZero() {
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(inst)}
		sts.Spec.ServiceName = childName(inst)
		if usesPVC(inst) {
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{dataVolumeClaim(inst)}
		}
	}

	// One replica only: the Tailscale state of every device lives in the
	// data directory, and two pods sharing a device identity would fight
	// over it.
	sts.Spec.Replicas = ptr.To[int32](1)

	// Keep pod template annotations set by others, such as the restartedAt
	// stamp of "kubectl rollout restart"; dropping them would roll the pod a
	// second time.
	template := podTemplate(inst, configHash)
	for k, v := range sts.Spec.Template.Annotations {
		if _, ours := template.Annotations[k]; !ours {
			template.Annotations[k] = v
		}
	}
	sts.Spec.Template = template
}

func mergeLabels(existing, desired map[string]string) map[string]string {
	if existing == nil {
		existing = map[string]string{}
	}
	maps.Copy(existing, desired)
	return existing
}
