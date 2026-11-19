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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
)

// These specs exercise the validation rules compiled into the CRDs. They only
// need the API server.
var _ = Describe("API validation", func() {
	var namespace string

	BeforeEach(func() {
		namespace = newNamespace()
	})

	oauth := &v1alpha1.OAuthCredentials{SecretName: "oauth"}

	DescribeTable("TSDProxy",
		func(mutate func(*v1alpha1.TSDProxy), wantErr string) {
			inst := newInstance(namespace, "home", mutate)
			err := k8sClient.Create(ctx, inst)
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("auth key", func(*v1alpha1.TSDProxy) {}, ""),
		Entry("OAuth with tags", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.AuthKey = nil
			i.Spec.Tailscale.OAuth = oauth
			i.Spec.Tailscale.Tags = []string{"tag:k8s"}
		}, ""),
		Entry("Services mode with OAuth and hostname", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale = v1alpha1.TailscaleSpec{
				OAuth: oauth, Tags: []string{"tag:k8s"},
				Mode: v1alpha1.TailscaleModeServices, Hostname: "proxy",
			}
		}, ""),
		Entry("no credentials", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.AuthKey = nil
		}, "exactly one of authKey or oauth"),
		Entry("both credentials", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.OAuth = oauth
			i.Spec.Tailscale.Tags = []string{"tag:k8s"}
		}, "exactly one of authKey or oauth"),
		Entry("OAuth without tags", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.AuthKey = nil
			i.Spec.Tailscale.OAuth = oauth
		}, "oauth requires at least one tag"),
		Entry("a tag without the tag: prefix", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.Tags = []string{"k8s"}
		}, "spec.tailscale.tags"),
		Entry("Services mode with an auth key", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.Mode = v1alpha1.TailscaleModeServices
			i.Spec.Tailscale.Hostname = "proxy"
		}, "Services mode requires oauth and hostname"),
		Entry("Services mode without a hostname", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale = v1alpha1.TailscaleSpec{
				OAuth: oauth, Tags: []string{"tag:k8s"}, Mode: v1alpha1.TailscaleModeServices,
			}
		}, "Services mode requires oauth and hostname"),
		Entry("an unknown mode", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.Mode = "Shared"
		}, "spec.tailscale.mode"),
		Entry("a Selector without a selector", func(i *v1alpha1.TSDProxy) {
			i.Spec.AllowedNamespaces = &v1alpha1.AllowedNamespaces{From: v1alpha1.NamespacesFromSelector}
		}, "selector is required when from is Selector"),
		Entry("a control URL that is not a URL", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.ControlURL = "headscale.example.com"
		}, "spec.tailscale.controlURL"),
	)

	It("rejects a TSDProxy name too long for its children", func() {
		inst := newInstance(namespace, "an-instance-name-that-is-far-too-long-to-prefix")
		Expect(k8sClient.Create(ctx, inst)).To(MatchError(ContainSubstring("at most 40 characters")))

		dotted := newInstance(namespace, "home.lab")
		Expect(k8sClient.Create(ctx, dotted)).To(MatchError(ContainSubstring("DNS label")))
	})

	It("applies TSDProxy defaults", func() {
		inst := newInstance(namespace, "home", func(i *v1alpha1.TSDProxy) {
			i.Spec.Storage = &v1alpha1.StorageSpec{}
			i.Spec.AllowedNamespaces = &v1alpha1.AllowedNamespaces{}
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		Expect(inst.Spec.Image).To(Equal(defaultImage))
		Expect(inst.Spec.Tailscale.Mode).To(Equal(v1alpha1.TailscaleModePerProxy))
		Expect(inst.Spec.Storage.Type).To(Equal(v1alpha1.StorageTypePersistentVolumeClaim))
		Expect(inst.Spec.Storage.Size.String()).To(Equal("1Gi"))
		Expect(inst.Spec.AllowedNamespaces.From).To(Equal(v1alpha1.NamespacesFromSame))
	})

	It("keeps TSDProxy storage immutable", func() {
		inst := newInstance(namespace, "home", func(i *v1alpha1.TSDProxy) {
			i.Spec.Storage = &v1alpha1.StorageSpec{Type: v1alpha1.StorageTypePersistentVolumeClaim}
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())

		// update applies mutate to the latest version. The controller writes
		// status concurrently, so a conflict means "read again", not failure.
		update := func(mutate func(*v1alpha1.TSDProxy)) error {
			var err error
			Eventually(func() bool {
				current := &v1alpha1.TSDProxy{}
				if err = k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), current); err != nil {
					return true
				}
				mutate(current)
				err = k8sClient.Update(ctx, current)
				return !apierrors.IsConflict(err)
			}, timeout, interval).Should(BeTrue())
			return err
		}

		Expect(update(func(i *v1alpha1.TSDProxy) {
			bigger := resource.MustParse("10Gi")
			i.Spec.Storage.Size = &bigger
		})).To(MatchError(ContainSubstring("storage is immutable")))

		Expect(update(func(i *v1alpha1.TSDProxy) {
			i.Spec.Storage = nil
		})).To(MatchError(ContainSubstring("storage is immutable")))

		By("still allowing other changes")
		Expect(update(func(i *v1alpha1.TSDProxy) {
			i.Spec.Log = &v1alpha1.LogSpec{Level: "debug"}
		})).To(Succeed())
	})

	DescribeTable("Proxy",
		func(mutate func(*v1alpha1.Proxy), wantErr string) {
			proxy := newProxy(namespace, "app", newInstance(namespace, "home"), mutate)
			err := k8sClient.Create(ctx, proxy)
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("a Service target", func(*v1alpha1.Proxy) {}, ""),
		Entry("a URL target", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Target = v1alpha1.PortTarget{URL: "tcp://db.internal:5432"}
			p.Spec.Ports[0].Protocol = v1alpha1.ProtocolTCP
		}, ""),
		Entry("the same port number with two protocols", func(p *v1alpha1.Proxy) {
			p.Spec.Ports = append(p.Spec.Ports, v1alpha1.ProxyPort{
				Port: 443, Protocol: v1alpha1.ProtocolUDP,
				Target: v1alpha1.PortTarget{URL: "udp://10.0.0.1:443"},
			})
		}, ""),
		Entry("funnel on https", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Funnel = true
		}, ""),
		Entry("no ports", func(p *v1alpha1.Proxy) {
			p.Spec.Ports = []v1alpha1.ProxyPort{}
		}, "spec.ports"),
		Entry("a duplicate port", func(p *v1alpha1.Proxy) {
			p.Spec.Ports = append(p.Spec.Ports, p.Spec.Ports[0])
		}, "Duplicate value"),
		Entry("a target with neither Service nor URL", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Target = v1alpha1.PortTarget{}
		}, "exactly one of service or url"),
		Entry("a target with both Service and URL", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Target.URL = "http://10.0.0.1:80"
		}, "exactly one of service or url"),
		Entry("a URL without a scheme", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Target = v1alpha1.PortTarget{URL: "10.0.0.1:80"}
		}, "spec.ports[0].target.url"),
		Entry("funnel on tcp", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Protocol = v1alpha1.ProtocolTCP
			p.Spec.Ports[0].Funnel = true
		}, "funnel requires the https protocol"),
		Entry("a redirect to a Service", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Redirect = true
		}, "redirect requires a url target"),
		Entry("a port out of range", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Port = 70000
		}, "spec.ports[0].port"),
		Entry("an unknown protocol", func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Protocol = "sctp"
		}, "spec.ports[0].protocol"),
		Entry("a hostname with a dot", func(p *v1alpha1.Proxy) {
			p.Spec.Hostname = "app.example"
		}, "spec.hostname"),
		Entry("an upper-case hostname", func(p *v1alpha1.Proxy) {
			p.Spec.Hostname = "App"
		}, "spec.hostname"),
	)

	It("defaults a Proxy port to https", func() {
		proxy := newProxy(namespace, "app", newInstance(namespace, "home"), func(p *v1alpha1.Proxy) {
			p.Spec.Ports[0].Protocol = ""
		})
		Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
		Expect(proxy.Spec.Ports[0].Protocol).To(Equal(v1alpha1.ProtocolHTTPS))
	})
})
