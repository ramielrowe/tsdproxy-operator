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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/render"
	"github.com/ramielrowe/tsdproxy-operator/internal/tsdproxyapi"
)

func updateInstance(inst *v1alpha1.TSDProxy, mutate func(*v1alpha1.TSDProxy)) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		current := &v1alpha1.TSDProxy{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), current)).To(Succeed())
		mutate(current)
		g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
	}, timeout, interval).Should(Succeed())
}

var _ = Describe("Proxy controller", func() {
	var namespace string

	BeforeEach(func() {
		namespace = newNamespace()
	})

	Context("attachment", func() {
		It("renders an accepted Proxy into the instance's proxy list", func() {
			inst := createInstance(namespace, "home")
			proxy := newProxy(namespace, "jellyfin", inst, func(p *v1alpha1.Proxy) {
				p.Spec.InstanceRef.Namespace = "" // defaults to the Proxy's namespace
			})
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())

			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
			eventuallyHostnames(inst, "jellyfin")

			Eventually(func(g Gomega) {
				list := proxyList(g, inst)
				g.Expect(list["jellyfin"].Ports).To(HaveKey("443/https"))
				g.Expect(list["jellyfin"].Ports["443/https"].Targets).
					To(ConsistOf("http://jellyfin." + namespace + ".svc:8080"))
				g.Expect(getProxy(g, proxy).Status.Hostname).To(Equal("jellyfin"))
			}, timeout, interval).Should(Succeed())

			By("updating the Proxy")
			Eventually(func(g Gomega) {
				current := getProxy(g, proxy)
				current.Spec.Ports[0].Target = v1alpha1.PortTarget{URL: "http://192.168.1.10:8096"}
				g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(proxyList(g, inst)["jellyfin"].Ports["443/https"].Targets).
					To(ConsistOf("http://192.168.1.10:8096"))
			}, timeout, interval).Should(Succeed())

			By("deleting the Proxy")
			Expect(k8sClient.Delete(ctx, proxy)).To(Succeed())
			eventuallyHostnames(inst)
		})

		It("uses spec.hostname over the name", func() {
			inst := createInstance(namespace, "home")
			proxy := newProxy(namespace, "jellyfin.v2", inst, func(p *v1alpha1.Proxy) {
				p.Spec.Hostname = "films"
			})
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
			eventuallyHostnames(inst, "films")
		})

		It("rejects a name that is not a hostname until spec.hostname is set", func() {
			inst := createInstance(namespace, "home")
			proxy := newProxy(namespace, "jellyfin.v2", inst)
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())

			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonInvalidHostname)
			eventuallyProxyCondition(proxy, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNotAccepted)
			eventuallyHostnames(inst)
		})

		It("waits for an instance that does not exist yet", func() {
			pending := newInstance(namespace, "later")
			proxy := newProxy(namespace, "app", pending)
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonInstanceNotFound)

			inst := createInstance(namespace, "later")
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
			eventuallyHostnames(inst, "app")
		})

		It("only admits other namespaces the instance allows", func() {
			inst := createInstance(namespace, "home")
			other := newNamespace()
			proxy := newProxy(other, "app", inst)
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())

			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonNamespaceNotAllowed)
			Consistently(func(g Gomega) {
				g.Expect(proxyList(g, inst)).To(BeEmpty())
			}, settle, interval).Should(Succeed())

			By("allowing all namespaces")
			updateInstance(inst, func(i *v1alpha1.TSDProxy) {
				i.Spec.AllowedNamespaces = &v1alpha1.AllowedNamespaces{From: v1alpha1.NamespacesFromAll}
			})
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
			eventuallyHostnames(inst, "app")
			Eventually(func(g Gomega) {
				// A Service target resolves in the Proxy's namespace, not
				// the instance's.
				g.Expect(proxyList(g, inst)["app"].Ports["443/https"].Targets).
					To(ConsistOf("http://app." + other + ".svc:8080"))
			}, timeout, interval).Should(Succeed())

			By("going back to the instance's own namespace")
			updateInstance(inst, func(i *v1alpha1.TSDProxy) { i.Spec.AllowedNamespaces = nil })
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonNamespaceNotAllowed)
			eventuallyHostnames(inst)
		})

		It("follows namespace labels when the instance uses a selector", func() {
			inst := createInstance(namespace, "home", func(i *v1alpha1.TSDProxy) {
				i.Spec.AllowedNamespaces = &v1alpha1.AllowedNamespaces{
					From:     v1alpha1.NamespacesFromSelector,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"tailnet": "home"}},
				}
			})
			other := newNamespace()
			proxy := newProxy(other, "app", inst)
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonNamespaceNotAllowed)

			By("labelling the namespace")
			setLabel := func(value string) {
				Eventually(func(g Gomega) {
					ns := &corev1.Namespace{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: other}, ns)).To(Succeed())
					ns.Labels = map[string]string{"tailnet": value}
					g.Expect(k8sClient.Update(ctx, ns)).To(Succeed())
				}, timeout, interval).Should(Succeed())
			}
			setLabel("home")
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
			eventuallyHostnames(inst, "app")

			By("changing the label")
			setLabel("work")
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonNamespaceNotAllowed)
			eventuallyHostnames(inst)
		})

		It("gives a contested hostname to the oldest Proxy and hands it over", func() {
			inst := createInstance(namespace, "home")

			first := newProxy(namespace, "first", inst, func(p *v1alpha1.Proxy) { p.Spec.Hostname = "app" })
			Expect(k8sClient.Create(ctx, first)).To(Succeed())
			eventuallyProxyCondition(first, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)

			// creationTimestamp has one-second resolution; the tie-break by
			// name also favours "first".
			second := newProxy(namespace, "second", inst, func(p *v1alpha1.Proxy) { p.Spec.Hostname = "app" })
			Expect(k8sClient.Create(ctx, second)).To(Succeed())
			eventuallyProxyCondition(second, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonHostnameConflict)

			Consistently(func(g Gomega) {
				g.Expect(proxyList(g, inst)["app"].Ports["443/https"].Targets).
					To(ConsistOf("http://first." + namespace + ".svc:8080"))
				g.Expect(getProxy(g, first).Status.Conditions).NotTo(BeEmpty())
			}, settle, interval).Should(Succeed())
			eventuallyProxyCondition(first, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)

			By("deleting the holder")
			Expect(k8sClient.Delete(ctx, first)).To(Succeed())
			eventuallyProxyCondition(second, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
			Eventually(func(g Gomega) {
				g.Expect(proxyList(g, inst)["app"].Ports["443/https"].Targets).
					To(ConsistOf("http://second." + namespace + ".svc:8080"))
			}, timeout, interval).Should(Succeed())
		})

		It("moves between instances when instanceRef changes", func() {
			a := createInstance(namespace, "a")
			b := newInstance(namespace, "b")
			Expect(k8sClient.Create(ctx, b)).To(Succeed())

			proxy := newProxy(namespace, "app", a)
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
			eventuallyHostnames(a, "app")

			Eventually(func(g Gomega) {
				current := getProxy(g, proxy)
				current.Spec.InstanceRef.Name = "b"
				g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			eventuallyHostnames(b, "app")
			eventuallyHostnames(a)
		})

		It("rejects what the instance's mode cannot serve", func() {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "oauth"},
				StringData: map[string]string{"client_id": "id", "client_secret": "secret"},
			})).To(Succeed())
			inst := newInstance(namespace, "svc", func(i *v1alpha1.TSDProxy) {
				i.Spec.Tailscale = v1alpha1.TailscaleSpec{
					OAuth:    &v1alpha1.OAuthCredentials{SecretName: "oauth"},
					Tags:     []string{"tag:k8s"},
					Mode:     v1alpha1.TailscaleModeServices,
					Hostname: "svc-host",
				}
			})
			Expect(k8sClient.Create(ctx, inst)).To(Succeed())

			udp := newProxy(namespace, "dns", inst, func(p *v1alpha1.Proxy) {
				p.Spec.Ports = []v1alpha1.ProxyPort{{
					Port: 53, Protocol: v1alpha1.ProtocolUDP,
					Target: v1alpha1.PortTarget{URL: "udp://10.0.0.1:53"},
				}}
			})
			Expect(k8sClient.Create(ctx, udp)).To(Succeed())
			eventuallyProxyCondition(udp, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonUnsupported)
		})
	})

	Context("runtime state", func() {
		It("reports what tsdproxy says about the proxy", func() {
			inst := createInstance(namespace, "home")
			proxy := newProxy(namespace, "app", inst)
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())

			By("the instance not being ready")
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
			eventuallyProxyCondition(proxy, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInstanceNotReady)

			By("tsdproxy running but not knowing the proxy yet")
			markPodReady(inst)
			eventuallyProxyCondition(proxy, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonPending)

			By("tsdproxy still not listing the proxy after the grace period")
			eventuallyProxyCondition(proxy, v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNotServing)
			Eventually(func(g Gomega) {
				cond := meta.FindStatusCondition(getProxy(g, proxy).Status.Conditions, v1alpha1.ConditionReady)
				g.Expect(cond.Message).To(ContainSubstring("pod " + podKey(inst).String()))
			}, timeout, interval).Should(Succeed())

			By("the proxy authenticating")
			fakeStatus.set(apiBaseURL(inst), "app", tsdproxyapi.ProxyStatus{
				Status: "Authenticating", Health: "unknown", URL: "https://login.tailscale.com/a/abc",
			})
			eventuallyProxyCondition(proxy, v1alpha1.ConditionReady, metav1.ConditionFalse, "Authenticating")
			Eventually(func(g Gomega) {
				status := getProxy(g, proxy).Status
				g.Expect(status.State).To(Equal("Authenticating"))
				g.Expect(status.URL).To(Equal("https://login.tailscale.com/a/abc"))
			}, timeout, interval).Should(Succeed())

			By("the proxy running")
			fakeStatus.set(apiBaseURL(inst), "app", tsdproxyapi.ProxyStatus{
				Status: tsdproxyapi.StateRunning, Health: "healthy", URL: "https://app.example.ts.net",
			})
			eventuallyProxyCondition(proxy, v1alpha1.ConditionReady, metav1.ConditionTrue, ReasonRunning)
			Eventually(func(g Gomega) {
				status := getProxy(g, proxy).Status
				g.Expect(status.State).To(Equal(tsdproxyapi.StateRunning))
				g.Expect(status.Health).To(Equal("healthy"))
				g.Expect(status.URL).To(Equal("https://app.example.ts.net"))
				g.Expect(status.ObservedGeneration).To(Equal(getProxy(g, proxy).Generation))
			}, timeout, interval).Should(Succeed())

			By("authenticating to tsdproxy with the generated API key")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: apiKeySecretName(inst)}, secret)).To(Succeed())
			Expect(fakeStatus.apiKeyUsed(apiBaseURL(inst))).To(Equal(string(secret.Data[render.APIKeyFileName])))

			By("the Proxy losing its attachment")
			Eventually(func(g Gomega) {
				current := getProxy(g, proxy)
				current.Spec.InstanceRef.Name = "gone"
				g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
			}, timeout, interval).Should(Succeed())
			eventuallyProxyCondition(proxy, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonInstanceNotFound)
			Eventually(func(g Gomega) {
				status := getProxy(g, proxy).Status
				g.Expect(status.URL).To(BeEmpty())
				g.Expect(status.State).To(BeEmpty())
				g.Expect(status.Hostname).To(BeEmpty())
			}, timeout, interval).Should(Succeed())
		})
	})
})
