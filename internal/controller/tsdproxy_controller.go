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
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/attach"
	"github.com/ramielrowe/tsdproxy-operator/internal/render"
)

// Reasons reported on a TSDProxy's Ready condition.
const (
	ReasonReady               = "Ready"
	ReasonCredentialsNotFound = "CredentialsNotFound"
	ReasonInvalidSelector     = "InvalidSelector"
	ReasonRolloutInProgress   = "RolloutInProgress"
	ReasonPodNotReady         = "PodNotReady"

	apiKeyBytes = 32
)

// TSDProxyReconciler reconciles a TSDProxy object: one tsdproxy pod per
// instance, fed by a ConfigMap rendered from the instance and the Proxies
// attached to it.
type TSDProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=tsdproxy-operator.apmelton.com,resources=tsdproxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tsdproxy-operator.apmelton.com,resources=tsdproxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tsdproxy-operator.apmelton.com,resources=tsdproxies/finalizers,verbs=update
// +kubebuilder:rbac:groups=tsdproxy-operator.apmelton.com,resources=proxies,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;services;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile renders the instance's configuration and keeps its Secret,
// ConfigMap, Service and StatefulSet in line with it.
//
// tsdproxy treats the two rendered files differently, and so does this
// function. The proxy list is watched by tsdproxy, so updating the ConfigMap
// is enough to add, change or remove proxies without a restart. The main
// configuration and the credentials are only read at startup, so a change to
// either rolls the pod through a hash on the pod template.
func (r *TSDProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var inst v1alpha1.TSDProxy
	if err := r.Get(ctx, req.NamespacedName, &inst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !inst.DeletionTimestamp.IsZero() {
		// Every child carries an owner reference; garbage collection
		// removes them.
		return ctrl.Result{}, nil
	}

	credentials, missing, err := r.credentials(ctx, &inst)
	if err != nil {
		return ctrl.Result{}, err
	}
	if missing != "" {
		// Without credentials the pod could not start. The Secret watch
		// triggers the next attempt.
		return ctrl.Result{}, r.updateStatus(ctx, &inst, func(s *v1alpha1.TSDProxyStatus) {
			setCondition(&inst, &s.Conditions, v1alpha1.ConditionReady,
				metav1.ConditionFalse, ReasonCredentialsNotFound, missing)
		})
	}

	apiKey, err := r.reconcileAPIKey(ctx, &inst)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile API key Secret: %w", err)
	}

	attachments, err := resolveAttachments(ctx, r.Client, &inst)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve Proxies: %w", err)
	}

	mainConfig, err := render.MainConfig(&inst)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render %s: %w", render.MainConfigFile, err)
	}
	proxyList, err := render.ProxyList(&inst, attachments.Accepted)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render %s: %w", render.ProxyListFile, err)
	}

	if err := r.reconcileConfigMap(ctx, &inst, mainConfig, proxyList); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile ConfigMap: %w", err)
	}
	if err := r.reconcileService(ctx, &inst); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Service: %w", err)
	}

	configHash := hashOf(append([][]byte{mainConfig, apiKey}, credentials...)...)
	sts, err := r.reconcileStatefulSet(ctx, &inst, configHash)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile StatefulSet: %w", err)
	}

	if err := r.nudgePod(ctx, &inst, hashOf(proxyList)); err != nil {
		// The kubelet's periodic sync delivers the list anyway, only later.
		log.Info("Could not nudge the pod to sync its ConfigMap volume", "error", err.Error())
	}

	return ctrl.Result{}, r.updateStatus(ctx, &inst, func(s *v1alpha1.TSDProxyStatus) {
		s.AttachedProxies = int32(len(attachments.Accepted))
		s.ConfigHash = configHash
		status, reason, message := readiness(&inst, sts)
		setCondition(&inst, &s.Conditions, v1alpha1.ConditionReady, status, reason, message)
	})
}

// credentials reads the values tsdproxy needs from the user's Secret, in a
// stable order for hashing. missing is non-empty, and describes the problem,
// when the Secret or one of its keys does not exist.
func (r *TSDProxyReconciler) credentials(ctx context.Context, inst *v1alpha1.TSDProxy) (values [][]byte, missing string, err error) {
	name := credentialsSecretName(inst)
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: inst.Namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("Secret %q not found", name), nil
		}
		return nil, "", err
	}
	for _, key := range credentialKeys(inst) {
		value, ok := secret.Data[key]
		if !ok || len(value) == 0 {
			return nil, fmt.Sprintf("Secret %q has no key %q", name, key), nil
		}
		values = append(values, value)
	}
	return values, "", nil
}

// reconcileAPIKey ensures the Secret with the API key the operator uses to
// read proxy status from tsdproxy, and returns the key. The key is generated
// once and then kept: tsdproxy only reads it at startup.
func (r *TSDProxyReconciler) reconcileAPIKey(ctx context.Context, inst *v1alpha1.TSDProxy) ([]byte, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: inst.Namespace,
		Name:      apiKeySecretName(inst),
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = mergeLabels(secret.Labels, childLabels(inst))
		if len(secret.Data[render.APIKeyFileName]) == 0 {
			raw := make([]byte, apiKeyBytes)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data[render.APIKeyFileName] = []byte(hex.EncodeToString(raw))
		}
		return controllerutil.SetControllerReference(inst, secret, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return secret.Data[render.APIKeyFileName], nil
}

func (r *TSDProxyReconciler) reconcileConfigMap(ctx context.Context, inst *v1alpha1.TSDProxy, mainConfig, proxyList []byte) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: inst.Namespace, Name: childName(inst)}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = mergeLabels(cm.Labels, childLabels(inst))
		cm.Data = map[string]string{
			render.MainConfigFile: string(mainConfig),
			render.ProxyListFile:  string(proxyList),
		}
		return controllerutil.SetControllerReference(inst, cm, r.Scheme)
	})
	return err
}

func (r *TSDProxyReconciler) reconcileService(ctx context.Context, inst *v1alpha1.TSDProxy) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: inst.Namespace, Name: childName(inst)}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = mergeLabels(svc.Labels, childLabels(inst))
		svc.Spec.Selector = selectorLabels(inst)
		svc.Spec.Ports = servicePorts()
		return controllerutil.SetControllerReference(inst, svc, r.Scheme)
	})
	return err
}

func (r *TSDProxyReconciler) reconcileStatefulSet(ctx context.Context, inst *v1alpha1.TSDProxy, configHash string) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: inst.Namespace, Name: childName(inst)}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		mutateStatefulSet(sts, inst, configHash)
		return controllerutil.SetControllerReference(inst, sts, r.Scheme)
	})
	return sts, err
}

// nudgePod stamps the proxy list's hash on the running pod. tsdproxy picks a
// list change up from the mounted ConfigMap, but the kubelet only rewrites
// that volume when it syncs the pod, which without a trigger can take a
// minute or more. A changed annotation is such a trigger.
func (r *TSDProxyReconciler) nudgePod(ctx context.Context, inst *v1alpha1.TSDProxy, proxiesHash string) error {
	var pod corev1.Pod
	if err := r.Get(ctx, podKey(inst), &pod); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !pod.DeletionTimestamp.IsZero() || pod.Annotations[ProxiesHashAnnotation] == proxiesHash {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[ProxiesHashAnnotation] = proxiesHash
	return r.Patch(ctx, &pod, patch)
}

// readiness derives the Ready condition from the StatefulSet.
func readiness(inst *v1alpha1.TSDProxy, sts *appsv1.StatefulSet) (metav1.ConditionStatus, string, string) {
	if err := attach.SelectorError(inst); err != nil {
		return metav1.ConditionFalse, ReasonInvalidSelector,
			fmt.Sprintf("%v; only Proxies in namespace %q are attached", err, inst.Namespace)
	}
	st := sts.Status
	switch {
	case st.ObservedGeneration < sts.Generation ||
		(st.UpdateRevision != "" && st.UpdateRevision != st.CurrentRevision) ||
		st.UpdatedReplicas < 1:
		return metav1.ConditionFalse, ReasonRolloutInProgress, "the tsdproxy pod is being rolled out"
	case st.ReadyReplicas < 1:
		return metav1.ConditionFalse, ReasonPodNotReady, "the tsdproxy pod is not ready"
	default:
		return metav1.ConditionTrue, ReasonReady, "the tsdproxy pod is ready"
	}
}

func setCondition(obj client.Object, conditions *[]metav1.Condition, conditionType string,
	status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	})
}

// updateStatus applies mutate to the instance's status and writes it only
// when something changed, so that a steady state causes no writes.
func (r *TSDProxyReconciler) updateStatus(ctx context.Context, inst *v1alpha1.TSDProxy, mutate func(*v1alpha1.TSDProxyStatus)) error {
	before := inst.DeepCopy()
	inst.Status.ObservedGeneration = inst.Generation
	mutate(&inst.Status)
	if apiequality.Semantic.DeepEqual(before.Status, inst.Status) {
		return nil
	}
	return r.Status().Patch(ctx, inst, client.MergeFrom(before))
}

// SetupWithManager sets up the controller with the Manager.
func (r *TSDProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.TSDProxy{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		// Secrets are watched by metadata only and read straight from the
		// API server, so the manager never caches Secret contents.
		Owns(&corev1.Secret{}, builder.OnlyMetadata).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.instancesForSecret), builder.OnlyMetadata).
		Watches(&v1alpha1.Proxy{}, handler.EnqueueRequestsFromMapFunc(instanceForProxy)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.instancesWithSelector)).
		Named("tsdproxy").
		Complete(r)
}

// instanceForProxy maps a Proxy to the instance it references.
func instanceForProxy(_ context.Context, obj client.Object) []reconcile.Request {
	proxy, ok := obj.(*v1alpha1.Proxy)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: attach.InstanceKey(proxy)}}
}

// instancesForSecret maps a Secret to the instances using it as credentials.
func (r *TSDProxyReconciler) instancesForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var instances v1alpha1.TSDProxyList
	if err := r.List(ctx, &instances, client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{instanceCredentialsIndex: obj.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list TSDProxies for Secret", "secret", client.ObjectKeyFromObject(obj))
		return nil
	}
	return requestsFor(instances.Items)
}

// instancesWithSelector maps a Namespace event to every instance whose
// attachments depend on namespace labels.
func (r *TSDProxyReconciler) instancesWithSelector(ctx context.Context, _ client.Object) []reconcile.Request {
	var instances v1alpha1.TSDProxyList
	if err := r.List(ctx, &instances); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list TSDProxies for Namespace")
		return nil
	}
	selecting := instances.Items[:0]
	for _, inst := range instances.Items {
		if a := inst.Spec.AllowedNamespaces; a != nil && a.From == v1alpha1.NamespacesFromSelector {
			selecting = append(selecting, inst)
		}
	}
	return requestsFor(selecting)
}

func requestsFor(instances []v1alpha1.TSDProxy) []reconcile.Request {
	out := make([]reconcile.Request, 0, len(instances))
	for i := range instances {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&instances[i])})
	}
	return out
}
