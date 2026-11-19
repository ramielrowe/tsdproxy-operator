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
	"errors"
	"fmt"
	"sync"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/attach"
	"github.com/ramielrowe/tsdproxy-operator/internal/render"
	"github.com/ramielrowe/tsdproxy-operator/internal/tsdproxyapi"
)

// Reasons reported on a Proxy's Ready condition.
const (
	ReasonNotAccepted       = "NotAccepted"
	ReasonInstanceNotReady  = "InstanceNotReady"
	ReasonPending           = "Pending"
	ReasonNotServing        = "NotServing"
	ReasonStatusUnavailable = "StatusUnavailable"
	ReasonRunning           = "Running"

	// DefaultStatusPollInterval is how often a Proxy's state is refreshed
	// from tsdproxy.
	DefaultStatusPollInterval = 30 * time.Second

	// DefaultPendingGrace is how long a Proxy missing from tsdproxy is
	// reported as Pending before it is reported as NotServing.
	DefaultPendingGrace = time.Minute

	// pendingPollInterval applies while tsdproxy has not loaded the proxy
	// yet, which normally resolves within seconds.
	pendingPollInterval = 5 * time.Second
)

// ProxyReconciler reconciles a Proxy object. It does not write any tsdproxy
// configuration: the TSDProxy controller renders the proxy list. This
// controller reports, on the Proxy, whether it made it into that list and
// what tsdproxy says about it.
type ProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// StatusClient reads proxy state from tsdproxy.
	StatusClient tsdproxyapi.Client
	// StatusPollInterval defaults to DefaultStatusPollInterval.
	StatusPollInterval time.Duration
	// PendingGrace defaults to DefaultPendingGrace.
	PendingGrace time.Duration

	// missingSince records when tsdproxy was first seen not to list an
	// accepted Proxy, keyed by missingKey. It only feeds the choice between
	// the Pending and NotServing reasons, so losing it on restart is fine.
	missingSince sync.Map
}

// missingKey identifies one attempt of tsdproxy to start a proxy. tsdproxy
// starts a proxy again whenever its list entry changes, which a new Proxy
// generation implies.
type missingKey struct {
	uid        types.UID
	generation int64
}

// +kubebuilder:rbac:groups=tsdproxy-operator.apmelton.com,resources=proxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tsdproxy-operator.apmelton.com,resources=proxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tsdproxy-operator.apmelton.com,resources=proxies/finalizers,verbs=update

// Reconcile reports a Proxy's attachment and runtime state in its status.
func (r *ProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var proxy v1alpha1.Proxy
	if err := r.Get(ctx, req.NamespacedName, &proxy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !proxy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	instKey := attach.InstanceKey(&proxy)
	var inst v1alpha1.TSDProxy
	if err := r.Get(ctx, instKey, &inst); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// The TSDProxy watch brings the Proxy back once the instance exists.
		return ctrl.Result{}, r.updateStatus(ctx, &proxy, func(s *v1alpha1.ProxyStatus) {
			rejectStatus(&proxy, s, attach.Rejection{
				Reason:  v1alpha1.ReasonInstanceNotFound,
				Message: fmt.Sprintf("TSDProxy %s not found", instKey),
			})
		})
	}

	attachments, err := resolveAttachments(ctx, r.Client, &inst)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve Proxies: %w", err)
	}
	if rejection, rejected := attachments.Rejected[req.NamespacedName]; rejected {
		return ctrl.Result{}, r.updateStatus(ctx, &proxy, func(s *v1alpha1.ProxyStatus) {
			rejectStatus(&proxy, s, rejection)
		})
	}

	hostname := attach.Hostname(&proxy)
	observed := r.observe(ctx, &inst, &proxy, hostname)

	err = r.updateStatus(ctx, &proxy, func(s *v1alpha1.ProxyStatus) {
		s.Hostname = hostname
		s.State, s.Health, s.URL = observed.state, observed.health, observed.url
		setCondition(&proxy, &s.Conditions, v1alpha1.ConditionAccepted, metav1.ConditionTrue,
			v1alpha1.ReasonAccepted, fmt.Sprintf("attached to TSDProxy %s as %q", instKey, hostname))
		setCondition(&proxy, &s.Conditions, v1alpha1.ConditionReady,
			observed.ready, observed.reason, observed.message)
	})
	return ctrl.Result{RequeueAfter: observed.requeueAfter}, err
}

// observation is what the operator could learn about a proxy from tsdproxy.
type observation struct {
	state, health, url string
	ready              metav1.ConditionStatus
	reason, message    string
	requeueAfter       time.Duration
}

// observe asks the instance's tsdproxy for the proxy's state. Every failure
// becomes a condition instead of a reconcile error: an unreachable tsdproxy
// is an expected state, and retrying on the poll interval is all there is to
// do about it.
func (r *ProxyReconciler) observe(ctx context.Context, inst *v1alpha1.TSDProxy, proxy *v1alpha1.Proxy, hostname string) observation {
	poll := r.StatusPollInterval
	if poll <= 0 {
		poll = DefaultStatusPollInterval
	}
	grace := r.PendingGrace
	if grace <= 0 {
		grace = DefaultPendingGrace
	}

	// Anything but "tsdproxy does not list the proxy" ends the current
	// wait; the deferred cleanup keeps that in one place.
	missing := missingKey{uid: proxy.UID, generation: proxy.Generation}
	stillMissing := false
	defer func() {
		if !stillMissing {
			r.missingSince.Delete(missing)
		}
	}()

	if !meta.IsStatusConditionTrue(inst.Status.Conditions, v1alpha1.ConditionReady) {
		// The TSDProxy watch re-queues the Proxy when the instance turns
		// ready, so no timer is needed.
		return observation{
			ready:   metav1.ConditionFalse,
			reason:  ReasonInstanceNotReady,
			message: fmt.Sprintf("TSDProxy %s/%s is not ready", inst.Namespace, inst.Name),
		}
	}

	unavailable := func(err error) observation {
		return observation{
			ready:        metav1.ConditionUnknown,
			reason:       ReasonStatusUnavailable,
			message:      fmt.Sprintf("cannot read proxy state from tsdproxy: %v", err),
			requeueAfter: poll,
		}
	}

	var secret corev1.Secret
	secretKey := types.NamespacedName{Namespace: inst.Namespace, Name: apiKeySecretName(inst)}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		return unavailable(err)
	}

	status, err := r.StatusClient.GetProxy(ctx, apiBaseURL(inst), string(secret.Data[render.APIKeyFileName]), hostname)
	switch {
	case errors.Is(err, tsdproxyapi.ErrNotFound):
		stillMissing = true
		since, _ := r.missingSince.LoadOrStore(missing, time.Now())
		if time.Since(since.(time.Time)) < grace {
			return observation{
				ready:        metav1.ConditionFalse,
				reason:       ReasonPending,
				message:      "tsdproxy has not loaded this proxy yet",
				requeueAfter: pendingPollInterval,
			}
		}
		// tsdproxy removes a proxy that fails to start, and only says why
		// in its log. From the API that is indistinguishable from a proxy
		// it has not loaded, so the message points at the log.
		return observation{
			ready:  metav1.ConditionFalse,
			reason: ReasonNotServing,
			message: fmt.Sprintf("tsdproxy does not list this proxy. It drops a proxy that fails to start, "+
				"for example when Tailscale rejects the auth key or a tag, and logs the cause: see the logs of pod %s. "+
				"It tries again when the Proxy changes or the pod restarts.", podKey(inst)),
			requeueAfter: poll,
		}
	case err != nil:
		return unavailable(err)
	}

	obs := observation{
		state:        status.Status,
		health:       status.Health,
		url:          status.URL,
		requeueAfter: poll,
	}
	if status.Status == tsdproxyapi.StateRunning {
		obs.ready, obs.reason, obs.message = metav1.ConditionTrue, ReasonRunning, "tsdproxy reports the proxy as running"
	} else {
		// tsdproxy's states (Authenticating, AuthFailed, Error, ...) are
		// CamelCase words and serve as the reason directly.
		obs.ready, obs.reason = metav1.ConditionFalse, status.Status
		obs.message = fmt.Sprintf("tsdproxy reports the proxy as %s", status.Status)
		if obs.reason == "" {
			obs.reason = ReasonStatusUnavailable
		}
	}
	return obs
}

// rejectStatus records that the Proxy is not rendered and clears everything
// that described it while it was.
func rejectStatus(proxy *v1alpha1.Proxy, s *v1alpha1.ProxyStatus, rejection attach.Rejection) {
	s.Hostname, s.State, s.Health, s.URL = "", "", "", ""
	setCondition(proxy, &s.Conditions, v1alpha1.ConditionAccepted, metav1.ConditionFalse,
		rejection.Reason, rejection.Message)
	setCondition(proxy, &s.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
		ReasonNotAccepted, "the Proxy is not attached to a TSDProxy")
}

// updateStatus applies mutate to the Proxy's status and writes it only when
// something changed.
func (r *ProxyReconciler) updateStatus(ctx context.Context, proxy *v1alpha1.Proxy, mutate func(*v1alpha1.ProxyStatus)) error {
	before := proxy.DeepCopy()
	proxy.Status.ObservedGeneration = proxy.Generation
	mutate(&proxy.Status)
	if apiequality.Semantic.DeepEqual(before.Status, proxy.Status) {
		return nil
	}
	return r.Status().Patch(ctx, proxy, client.MergeFrom(before))
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only spec changes matter between Proxies; without the predicate every
	// status write would fan out to all Proxies of the instance.
	specChanged := builder.WithPredicates(predicate.GenerationChangedPredicate{})

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Proxy{}, specChanged).
		// A Proxy's verdict depends on the others attached to the same
		// instance: when the holder of a hostname goes away, the next one
		// takes over.
		Watches(&v1alpha1.Proxy{}, handler.EnqueueRequestsFromMapFunc(r.siblingProxies), specChanged).
		Watches(&v1alpha1.TSDProxy{}, handler.EnqueueRequestsFromMapFunc(r.proxiesOfInstance)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.proxiesInNamespace),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Named("proxy").
		Complete(r)
}

func (r *ProxyReconciler) siblingProxies(ctx context.Context, obj client.Object) []reconcile.Request {
	proxy, ok := obj.(*v1alpha1.Proxy)
	if !ok {
		return nil
	}
	return r.proxiesReferencing(ctx, attach.InstanceKey(proxy))
}

func (r *ProxyReconciler) proxiesOfInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.proxiesReferencing(ctx, client.ObjectKeyFromObject(obj))
}

func (r *ProxyReconciler) proxiesReferencing(ctx context.Context, instance types.NamespacedName) []reconcile.Request {
	var proxies v1alpha1.ProxyList
	if err := r.List(ctx, &proxies, client.MatchingFields{proxyInstanceIndex: instance.String()}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Proxies of TSDProxy", "tsdproxy", instance)
		return nil
	}
	return proxyRequests(proxies.Items)
}

func (r *ProxyReconciler) proxiesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var proxies v1alpha1.ProxyList
	if err := r.List(ctx, &proxies, client.InNamespace(obj.GetName())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Proxies in Namespace", "namespace", obj.GetName())
		return nil
	}
	return proxyRequests(proxies.Items)
}

func proxyRequests(proxies []v1alpha1.Proxy) []reconcile.Request {
	out := make([]reconcile.Request, 0, len(proxies))
	for i := range proxies {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&proxies[i])})
	}
	return out
}
