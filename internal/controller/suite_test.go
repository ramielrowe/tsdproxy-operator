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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	tsdproxyoperatorv1alpha1 "github.com/ramielrowe/tsdproxy-operator/api/v1alpha1"
	"github.com/ramielrowe/tsdproxy-operator/internal/tsdproxyapi"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.
//
// Both controllers run inside a real manager against envtest's API server, so
// the tests exercise the watches and field indexes as well as the reconcile
// logic. envtest has no StatefulSet controller, kubelet or garbage collector:
// tests that need a ready pod set the StatefulSet status themselves.

var (
	ctx        context.Context
	cancel     context.CancelFunc
	testEnv    *envtest.Environment
	cfg        *rest.Config
	k8sClient  client.Client
	fakeStatus *fakeStatusClient
)

// fakeStatusClient stands in for the tsdproxy REST API.
type fakeStatusClient struct {
	mu      sync.Mutex
	proxies map[string]tsdproxyapi.ProxyStatus
	apiKeys map[string]string
}

func fakeKey(baseURL, hostname string) string { return baseURL + "|" + hostname }

func (f *fakeStatusClient) GetProxy(_ context.Context, baseURL, apiKey, hostname string) (*tsdproxyapi.ProxyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiKeys[baseURL] = apiKey
	status, ok := f.proxies[fakeKey(baseURL, hostname)]
	if !ok {
		return nil, tsdproxyapi.ErrNotFound
	}
	return &status, nil
}

// set makes the fake tsdproxy at baseURL report status for hostname.
func (f *fakeStatusClient) set(baseURL, hostname string, status tsdproxyapi.ProxyStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proxies[fakeKey(baseURL, hostname)] = status
}

// apiKeyUsed returns the API key the operator last presented to baseURL.
func (f *fakeStatusClient) apiKeyUsed(baseURL string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiKeys[baseURL]
}

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = tsdproxyoperatorv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("starting the controllers")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Same client configuration as cmd/main.go.
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}, &corev1.Pod{}},
		}},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(SetupIndexes(ctx, mgr.GetFieldIndexer())).To(Succeed())

	fakeStatus = &fakeStatusClient{
		proxies: map[string]tsdproxyapi.ProxyStatus{},
		apiKeys: map[string]string{},
	}
	Expect((&TSDProxyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())
	Expect((&ProxyReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		StatusClient:       fakeStatus,
		StatusPollInterval: 250 * time.Millisecond,
		PendingGrace:       3 * time.Second,
	}).SetupWithManager(mgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
