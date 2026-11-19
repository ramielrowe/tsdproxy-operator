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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/render"
)

const (
	timeout  = 15 * time.Second
	interval = 100 * time.Millisecond
	// settle is how long a "this must not happen" assertion watches.
	settle = 2 * time.Second

	credentialsSecret = "tailscale"
)

// newNamespace creates a namespace with a generated name, so every test
// works in isolation.
func newNamespace() string {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func createAuthKeySecret(namespace string) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: credentialsSecret},
		StringData: map[string]string{"authkey": "tskey-auth-test"},
	})).To(Succeed())
}

// newInstance builds, without creating, a TSDProxy using an auth key.
func newInstance(namespace, name string, mutate ...func(*v1alpha1.TSDProxy)) *v1alpha1.TSDProxy {
	inst := &v1alpha1.TSDProxy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.TSDProxySpec{
			Tailscale: v1alpha1.TailscaleSpec{
				AuthKey: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credentialsSecret},
					Key:                  "authkey",
				},
			},
		},
	}
	for _, m := range mutate {
		m(inst)
	}
	return inst
}

// createInstance creates the credentials Secret and a TSDProxy, and waits
// for the operator to create its StatefulSet.
func createInstance(namespace, name string, mutate ...func(*v1alpha1.TSDProxy)) *v1alpha1.TSDProxy {
	GinkgoHelper()
	createAuthKeySecret(namespace)
	inst := newInstance(namespace, name, mutate...)
	Expect(k8sClient.Create(ctx, inst)).To(Succeed())
	Eventually(func() error {
		return k8sClient.Get(ctx, childKey(inst), &appsv1.StatefulSet{})
	}, timeout, interval).Should(Succeed())
	return inst
}

// newProxy builds, without creating, a Proxy with one https port.
func newProxy(namespace, name string, inst *v1alpha1.TSDProxy, mutate ...func(*v1alpha1.Proxy)) *v1alpha1.Proxy {
	p := &v1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.ProxySpec{
			InstanceRef: v1alpha1.InstanceReference{Name: inst.Name, Namespace: inst.Namespace},
			Ports: []v1alpha1.ProxyPort{{
				Port:     443,
				Protocol: v1alpha1.ProtocolHTTPS,
				Target:   v1alpha1.PortTarget{Service: &v1alpha1.ServiceTarget{Name: name, Port: 8080}},
			}},
		},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func childKey(inst *v1alpha1.TSDProxy) types.NamespacedName {
	return types.NamespacedName{Namespace: inst.Namespace, Name: childName(inst)}
}

func getStatefulSet(inst *v1alpha1.TSDProxy) *appsv1.StatefulSet {
	GinkgoHelper()
	sts := &appsv1.StatefulSet{}
	Expect(k8sClient.Get(ctx, childKey(inst), sts)).To(Succeed())
	return sts
}

// configHash returns the startup-configuration hash on the pod template.
func configHash(inst *v1alpha1.TSDProxy) string {
	GinkgoHelper()
	return getStatefulSet(inst).Spec.Template.Annotations[ConfigHashAnnotation]
}

// listEntry is the part of a rendered proxy list entry the tests look at.
type listEntry struct {
	Ports map[string]struct {
		Targets []string `json:"targets"`
	} `json:"ports"`
}

// proxyList returns the proxy list currently rendered for the instance.
func proxyList(g Gomega, inst *v1alpha1.TSDProxy) map[string]listEntry {
	cm := &corev1.ConfigMap{}
	g.Expect(k8sClient.Get(ctx, childKey(inst), cm)).To(Succeed())
	list := map[string]listEntry{}
	g.Expect(yaml.Unmarshal([]byte(cm.Data[render.ProxyListFile]), &list)).To(Succeed())
	return list
}

// eventuallyHostnames waits until exactly the given hostnames are rendered.
func eventuallyHostnames(inst *v1alpha1.TSDProxy, hostnames ...string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		list := proxyList(g, inst)
		got := make([]string, 0, len(list))
		for h := range list {
			got = append(got, h)
		}
		g.Expect(got).To(ConsistOf(hostnames))
	}, timeout, interval).Should(Succeed())
}

// markPodReady plays the part of the StatefulSet controller and kubelet,
// which envtest does not run.
func markPodReady(inst *v1alpha1.TSDProxy) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, childKey(inst), sts)).To(Succeed())
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.Replicas = 1
		sts.Status.ReadyReplicas = 1
		sts.Status.UpdatedReplicas = 1
		sts.Status.CurrentRevision = "rev-1"
		sts.Status.UpdateRevision = "rev-1"
		g.Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}, timeout, interval).Should(Succeed())
}

func instanceCondition(g Gomega, inst *v1alpha1.TSDProxy, conditionType string) *metav1.Condition {
	current := &v1alpha1.TSDProxy{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), current)).To(Succeed())
	return meta.FindStatusCondition(current.Status.Conditions, conditionType)
}

func getProxy(g Gomega, p *v1alpha1.Proxy) *v1alpha1.Proxy {
	current := &v1alpha1.Proxy{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), current)).To(Succeed())
	return current
}

// eventuallyProxyCondition waits for a condition with the given status and
// reason on the Proxy.
func eventuallyProxyCondition(p *v1alpha1.Proxy, conditionType string, status metav1.ConditionStatus, reason string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		cond := meta.FindStatusCondition(getProxy(g, p).Status.Conditions, conditionType)
		g.Expect(cond).NotTo(BeNil(), "condition %s not set", conditionType)
		g.Expect(cond.Status).To(Equal(status), "message: %s", cond.Message)
		g.Expect(cond.Reason).To(Equal(reason), "message: %s", cond.Message)
	}, timeout, interval).Should(Succeed())
}

func eventuallyInstanceCondition(inst *v1alpha1.TSDProxy, status metav1.ConditionStatus, reason string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		cond := instanceCondition(g, inst, v1alpha1.ConditionReady)
		g.Expect(cond).NotTo(BeNil(), "Ready condition not set")
		g.Expect(cond.Status).To(Equal(status), "message: %s", cond.Message)
		g.Expect(cond.Reason).To(Equal(reason), "message: %s", cond.Message)
	}, timeout, interval).Should(Succeed())
}
