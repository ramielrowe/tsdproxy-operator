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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/render"
)

func volumeNamed(sts *appsv1.StatefulSet, name string) *corev1.Volume {
	for i := range sts.Spec.Template.Spec.Volumes {
		if sts.Spec.Template.Spec.Volumes[i].Name == name {
			return &sts.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

var _ = Describe("TSDProxy controller", func() {
	var namespace string

	BeforeEach(func() {
		namespace = newNamespace()
	})

	It("waits for the credentials Secret before creating the workload", func() {
		inst := newInstance(namespace, "home")
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())

		eventuallyInstanceCondition(inst, metav1.ConditionFalse, ReasonCredentialsNotFound)
		Consistently(func() bool {
			err := k8sClient.Get(ctx, childKey(inst), &appsv1.StatefulSet{})
			return apierrors.IsNotFound(err)
		}, settle, interval).Should(BeTrue(), "StatefulSet created without credentials")

		By("creating a Secret that lacks the referenced key")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: credentialsSecret},
			StringData: map[string]string{"other": "value"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		Eventually(func(g Gomega) {
			cond := instanceCondition(g, inst, v1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Message).To(ContainSubstring(`no key "authkey"`))
		}, timeout, interval).Should(Succeed())

		By("adding the key")
		secret.StringData = map[string]string{"authkey": "tskey-auth-test"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, childKey(inst), &appsv1.StatefulSet{})
		}, timeout, interval).Should(Succeed())
		eventuallyInstanceCondition(inst, metav1.ConditionFalse, ReasonRolloutInProgress)
	})

	It("creates the ConfigMap, Secret, Service and StatefulSet", func() {
		inst := createInstance(namespace, "home")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), inst)).To(Succeed())

		By("rendering both configuration files into the ConfigMap")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, childKey(inst), cm)).To(Succeed())
		wantMain, err := render.MainConfig(inst)
		Expect(err).NotTo(HaveOccurred())
		Expect(cm.Data).To(HaveKeyWithValue(render.MainConfigFile, string(wantMain)))
		Expect(cm.Data).To(HaveKeyWithValue(render.ProxyListFile, "{}\n"))
		Expect(metav1.IsControlledBy(cm, inst)).To(BeTrue())

		By("generating an API key")
		apiSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: apiKeySecretName(inst)}, apiSecret)).To(Succeed())
		Expect(apiSecret.Data[render.APIKeyFileName]).To(HaveLen(2 * apiKeyBytes))
		Expect(metav1.IsControlledBy(apiSecret, inst)).To(BeTrue())

		By("exposing the HTTP port")
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, childKey(inst), svc)).To(Succeed())
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Port).To(BeEquivalentTo(render.HTTPPort))
		Expect(svc.Spec.Selector).To(Equal(selectorLabels(inst)))

		By("running one tsdproxy pod with persistent state")
		sts := getStatefulSet(inst)
		Expect(metav1.IsControlledBy(sts, inst)).To(BeTrue())
		Expect(*sts.Spec.Replicas).To(BeEquivalentTo(1))
		Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
		Expect(sts.Spec.VolumeClaimTemplates[0].Name).To(Equal(volumeData))
		Expect(sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]).
			To(Equal(resource.MustParse("1Gi")))
		Expect(sts.Spec.Template.Annotations[ConfigHashAnnotation]).NotTo(BeEmpty())

		c := sts.Spec.Template.Spec.Containers[0]
		Expect(c.Image).To(Equal(defaultImage))
		Expect(c.Env).To(BeEmpty(), "an auth key instance needs no environment")

		By("mounting the ConfigMap as a directory so tsdproxy sees updates")
		for _, m := range c.VolumeMounts {
			Expect(m.SubPath).To(BeEmpty(), "mount %s uses subPath", m.Name)
		}
		Expect(c.VolumeMounts).To(ContainElement(
			corev1.VolumeMount{Name: volumeConfig, MountPath: render.ConfigDir, ReadOnly: true}))

		By("projecting the auth key onto the path the configuration names")
		creds := volumeNamed(sts, volumeCredentials)
		Expect(creds).NotTo(BeNil())
		Expect(creds.Secret.SecretName).To(Equal(credentialsSecret))
		Expect(creds.Secret.Items).To(ConsistOf(corev1.KeyToPath{Key: "authkey", Path: render.AuthKeyFileName}))
		Expect(string(wantMain)).To(ContainSubstring(render.TailscaleSecretDir + "/" + render.AuthKeyFileName))
	})

	It("wires OAuth credentials through a file and an environment variable", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "oauth"},
			StringData: map[string]string{"id": "client-id", "client_secret": "client-secret"},
		})).To(Succeed())
		inst := newInstance(namespace, "work", func(i *v1alpha1.TSDProxy) {
			i.Spec.Tailscale.AuthKey = nil
			i.Spec.Tailscale.OAuth = &v1alpha1.OAuthCredentials{SecretName: "oauth", ClientIDKey: "id"}
			i.Spec.Tailscale.Tags = []string{"tag:k8s"}
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())

		var sts *appsv1.StatefulSet
		Eventually(func() error {
			sts = &appsv1.StatefulSet{}
			return k8sClient.Get(ctx, childKey(inst), sts)
		}, timeout, interval).Should(Succeed())

		creds := volumeNamed(sts, volumeCredentials)
		Expect(creds.Secret.SecretName).To(Equal("oauth"))
		// clientSecretKey was left to the CRD default.
		Expect(creds.Secret.Items).To(ConsistOf(
			corev1.KeyToPath{Key: "client_secret", Path: render.ClientSecretFileName}))

		env := sts.Spec.Template.Spec.Containers[0].Env
		Expect(env).To(HaveLen(1))
		Expect(env[0].Name).To(Equal(render.ClientIDEnv))
		Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal("oauth"))
		Expect(env[0].ValueFrom.SecretKeyRef.Key).To(Equal("id"))
	})

	It("uses an emptyDir when asked to", func() {
		inst := createInstance(namespace, "scratch", func(i *v1alpha1.TSDProxy) {
			i.Spec.Storage = &v1alpha1.StorageSpec{Type: v1alpha1.StorageTypeEmptyDir}
		})
		sts := getStatefulSet(inst)
		Expect(sts.Spec.VolumeClaimTemplates).To(BeEmpty())
		data := volumeNamed(sts, volumeData)
		Expect(data).NotTo(BeNil())
		Expect(data.EmptyDir).NotTo(BeNil())
	})

	It("applies the storage settings to the volume claim", func() {
		inst := createInstance(namespace, "big", func(i *v1alpha1.TSDProxy) {
			size := resource.MustParse("5Gi")
			class := "fast"
			i.Spec.Storage = &v1alpha1.StorageSpec{Size: &size, StorageClassName: &class}
		})
		claim := getStatefulSet(inst).Spec.VolumeClaimTemplates[0]
		Expect(claim.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("5Gi")))
		Expect(*claim.Spec.StorageClassName).To(Equal("fast"))
	})

	It("rolls the pod for startup configuration, not for proxy changes", func() {
		inst := createInstance(namespace, "home")
		original := configHash(inst)
		generation := getStatefulSet(inst).Generation

		By("attaching a Proxy")
		proxy := newProxy(namespace, "app", inst)
		Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
		eventuallyHostnames(inst, "app")
		Consistently(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, childKey(inst), sts)).To(Succeed())
			g.Expect(sts.Spec.Template.Annotations[ConfigHashAnnotation]).To(Equal(original))
			g.Expect(sts.Generation).To(Equal(generation), "the StatefulSet spec changed")
		}, settle, interval).Should(Succeed())

		By("removing the Proxy")
		Expect(k8sClient.Delete(ctx, proxy)).To(Succeed())
		eventuallyHostnames(inst)
		Expect(configHash(inst)).To(Equal(original))

		By("changing the main configuration")
		Eventually(func(g Gomega) {
			current := &v1alpha1.TSDProxy{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), current)).To(Succeed())
			current.Spec.Log = &v1alpha1.LogSpec{Level: "debug"}
			g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		var afterLog string
		Eventually(func() string {
			afterLog = configHash(inst)
			return afterLog
		}, timeout, interval).ShouldNot(Equal(original))

		By("rotating the auth key")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: credentialsSecret}, secret)).To(Succeed())
		secret.StringData = map[string]string{"authkey": "tskey-auth-rotated"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Eventually(func() string { return configHash(inst) }, timeout, interval).ShouldNot(Equal(afterLog))

		By("reporting the hash in status")
		Eventually(func(g Gomega) {
			current := &v1alpha1.TSDProxy{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), current)).To(Succeed())
			g.Expect(current.Status.ConfigHash).To(Equal(configHash(inst)))
			g.Expect(current.Status.ObservedGeneration).To(Equal(current.Generation))
		}, timeout, interval).Should(Succeed())
	})

	It("keeps pod template annotations it does not own", func() {
		inst := createInstance(namespace, "home")

		By("restarting the StatefulSet the way kubectl rollout restart does")
		Eventually(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, childKey(inst), sts)).To(Succeed())
			sts.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = "2026-11-19T00:00:00Z"
			g.Expect(k8sClient.Update(ctx, sts)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		// The StatefulSet update itself makes the operator reconcile.
		Consistently(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, childKey(inst), sts)).To(Succeed())
			g.Expect(sts.Spec.Template.Annotations).To(HaveKey("kubectl.kubernetes.io/restartedAt"))
			g.Expect(sts.Spec.Template.Annotations).To(HaveKey(ConfigHashAnnotation))
		}, settle, interval).Should(Succeed())
	})

	It("restores children that drift", func() {
		inst := createInstance(namespace, "home")

		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8sClient.Get(ctx, childKey(inst), cm)).To(Succeed())
			cm.Data[render.ProxyListFile] = "tampered: {}\n"
			g.Expect(k8sClient.Update(ctx, cm)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			current := &corev1.ConfigMap{}
			g.Expect(k8sClient.Get(ctx, childKey(inst), current)).To(Succeed())
			g.Expect(current.Data[render.ProxyListFile]).To(Equal("{}\n"))
		}, timeout, interval).Should(Succeed())
	})

	It("stamps the proxy list hash on the running pod", func() {
		inst := createInstance(namespace, "home")

		By("creating the pod the StatefulSet controller would create")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: podKey(inst).Name},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: containerName, Image: defaultImage,
			}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		podHash := func(g Gomega) string {
			current := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, podKey(inst), current)).To(Succeed())
			return current.Annotations[ProxiesHashAnnotation]
		}

		Expect(k8sClient.Create(ctx, newProxy(namespace, "one", inst))).To(Succeed())
		var first string
		Eventually(func(g Gomega) {
			first = podHash(g)
			g.Expect(first).NotTo(BeEmpty())
		}, timeout, interval).Should(Succeed())

		Expect(k8sClient.Create(ctx, newProxy(namespace, "two", inst))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(podHash(g)).NotTo(Equal(first))
		}, timeout, interval).Should(Succeed())

		By("leaving the pod template alone")
		Expect(getStatefulSet(inst).Spec.Template.Annotations).NotTo(HaveKey(ProxiesHashAnnotation))
	})

	It("reports readiness and the number of attached Proxies", func() {
		inst := createInstance(namespace, "home")
		eventuallyInstanceCondition(inst, metav1.ConditionFalse, ReasonRolloutInProgress)

		markPodReady(inst)
		eventuallyInstanceCondition(inst, metav1.ConditionTrue, ReasonReady)

		Expect(k8sClient.Create(ctx, newProxy(namespace, "one", inst))).To(Succeed())
		Expect(k8sClient.Create(ctx, newProxy(namespace, "two", inst))).To(Succeed())
		Eventually(func(g Gomega) {
			current := &v1alpha1.TSDProxy{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), current)).To(Succeed())
			g.Expect(current.Status.AttachedProxies).To(BeEquivalentTo(2))
		}, timeout, interval).Should(Succeed())

		By("losing the ready pod")
		Eventually(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, childKey(inst), sts)).To(Succeed())
			sts.Status.ReadyReplicas = 0
			g.Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		eventuallyInstanceCondition(inst, metav1.ConditionFalse, ReasonPodNotReady)
	})
})
