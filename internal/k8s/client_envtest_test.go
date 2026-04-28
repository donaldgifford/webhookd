// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package k8s_test

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/k8s"
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// testREST is the shared envtest *rest.Config. nil when KUBEBUILDER_ASSETS
// isn't set so non-envtest tests in this package run unaffected.
var (
	testEnv  *envtest.Environment
	testREST *rest.Config
)

// TestMain wires the envtest lifecycle for the k8s package's
// envtest-backed tests. Mirrors internal/webhook/executor_test.go
// (each package needs its own TestMain since they are separate test
// binaries). Tests that need envtest call requireEnvtest; everything
// else in the package runs whether envtest is available or not.
func TestMain(m *testing.M) {
	if assets := os.Getenv("KUBEBUILDER_ASSETS"); assets != "" {
		testEnv = &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "deploy", "crds"),
			},
			ErrorIfCRDPathMissing: true,
			Scheme:                k8s.Scheme,
		}
		cfg, err := testEnv.Start()
		if err != nil {
			log.Fatalf("envtest start: %v", err)
		}
		testREST = cfg
	}
	code := m.Run()
	if testEnv != nil {
		if err := testEnv.Stop(); err != nil {
			log.Printf("envtest stop: %v", err)
		}
	}
	os.Exit(code)
}

func requireEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	if testREST == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; run via 'make test'")
	}
	return testREST
}

// TestNewClients_KubeconfigPath exercises the happy path through
// NewClients with cfg.Kubeconfig pointing at a real kubeconfig file.
// Asserts that both flavors (CtrlClient + Clientset) actually talk to
// the apiserver — not just that construction returns no error. Closes
// the package's coverage gap on the success branch of NewClients and
// loadRESTConfig.
func TestNewClients_KubeconfigPath(t *testing.T) {
	rc := requireEnvtest(t)
	kubeconfigPath := writeKubeconfig(t, rc)

	cfg := &config.Config{
		Kubeconfig:      kubeconfigPath,
		ShutdownTimeout: 25 * time.Second,
		CR: config.CRConfig{
			APIGroup:    wizapi.GroupVersion.Group,
			APIVersion:  wizapi.GroupVersion.Version,
			Namespace:   "wiz-operator",
			SyncTimeout: 20 * time.Second,
		},
	}

	clients, err := k8s.NewClients(cfg)
	if err != nil {
		t.Fatalf("NewClients() error = %v", err)
	}
	if clients.CtrlClient == nil || clients.Clientset == nil || clients.RESTConfig == nil {
		t.Fatalf("NewClients() returned partial clients: %+v", clients)
	}

	// CtrlClient: typed Get against a core resource the scheme knows.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var ns corev1.Namespace
	if err := clients.CtrlClient.Get(ctx, client.ObjectKey{Name: "default"}, &ns); err != nil {
		t.Fatalf("CtrlClient.Get(default ns) = %v", err)
	}
	if ns.Name != "default" {
		t.Errorf("got ns %q, want %q", ns.Name, "default")
	}

	// Clientset: same resource via the client-go path. Confirms the
	// second client is also wired to the same apiserver.
	got, err := clients.Clientset.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Clientset Get(default ns) = %v", err)
	}
	if got.Name != "default" {
		t.Errorf("got ns %q, want %q", got.Name, "default")
	}
}

// TestNewClients_InClusterFallback_UsesKUBECONFIGEnv exercises the
// loadRESTConfig branch where cfg.Kubeconfig is empty so we fall through
// to ctrl.GetConfig(). ctrl.GetConfig() honors the KUBECONFIG env var
// before checking for in-cluster credentials, so pointing it at the
// envtest kubeconfig drives the same code path an in-cluster pod would.
func TestNewClients_InClusterFallback_UsesKUBECONFIGEnv(t *testing.T) {
	rc := requireEnvtest(t)
	kubeconfigPath := writeKubeconfig(t, rc)
	t.Setenv("KUBECONFIG", kubeconfigPath)

	cfg := &config.Config{
		Kubeconfig:      "",
		ShutdownTimeout: 25 * time.Second,
		CR: config.CRConfig{
			APIGroup:    wizapi.GroupVersion.Group,
			APIVersion:  wizapi.GroupVersion.Version,
			Namespace:   "wiz-operator",
			SyncTimeout: 20 * time.Second,
		},
	}

	clients, err := k8s.NewClients(cfg)
	if err != nil {
		t.Fatalf("NewClients() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var ns corev1.Namespace
	if err := clients.CtrlClient.Get(ctx, client.ObjectKey{Name: "default"}, &ns); err != nil {
		t.Fatalf("CtrlClient.Get(default ns) = %v", err)
	}
}

// writeKubeconfig serializes a *rest.Config to a kubeconfig file in
// t.TempDir(). NewClients goes through clientcmd, which only reads from
// disk — same trick cmd/webhookd's e2e test uses.
func writeKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	api := clientcmdapi.NewConfig()
	api.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	api.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	api.Contexts["envtest"] = &clientcmdapi.Context{
		Cluster:  "envtest",
		AuthInfo: "envtest",
	}
	api.CurrentContext = "envtest"
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	if err := clientcmd.WriteToFile(*api, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}
