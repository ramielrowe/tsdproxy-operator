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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/attach"
)

const (
	// proxyInstanceIndex indexes Proxies by the namespace/name of the
	// TSDProxy they reference.
	proxyInstanceIndex = "spec.instanceRef"
	// instanceCredentialsIndex indexes TSDProxies by the name of their
	// credentials Secret.
	instanceCredentialsIndex = "spec.tailscale.credentialsSecret"
)

// SetupIndexes registers the field indexes both controllers rely on. Call it
// once before starting the manager.
func SetupIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &v1alpha1.Proxy{}, proxyInstanceIndex,
		func(obj client.Object) []string {
			return []string{attach.InstanceKey(obj.(*v1alpha1.Proxy)).String()}
		}); err != nil {
		return err
	}
	return indexer.IndexField(ctx, &v1alpha1.TSDProxy{}, instanceCredentialsIndex,
		func(obj client.Object) []string {
			if name := credentialsSecretName(obj.(*v1alpha1.TSDProxy)); name != "" {
				return []string{name}
			}
			return nil
		})
}

// resolveAttachments lists the Proxies referencing inst and decides which are
// rendered. Both controllers go through it so they reach the same verdict.
func resolveAttachments(ctx context.Context, c client.Reader, inst *v1alpha1.TSDProxy) (attach.Result, error) {
	var proxies v1alpha1.ProxyList
	key := types.NamespacedName{Namespace: inst.Namespace, Name: inst.Name}
	if err := c.List(ctx, &proxies, client.MatchingFields{proxyInstanceIndex: key.String()}); err != nil {
		return attach.Result{}, err
	}

	// Namespace labels only matter for a selector; look each one up once.
	var lookupErr error
	cache := map[string]map[string]string{}
	labelsOf := func(namespace string) (map[string]string, bool) {
		if l, ok := cache[namespace]; ok {
			return l, l != nil
		}
		var ns corev1.Namespace
		if err := c.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
			if !apierrors.IsNotFound(err) && lookupErr == nil {
				lookupErr = err
			}
			cache[namespace] = nil
			return nil, false
		}
		l := ns.Labels
		if l == nil {
			l = map[string]string{}
		}
		cache[namespace] = l
		return l, true
	}

	return attach.Resolve(inst, proxies.Items, labelsOf), lookupErr
}
