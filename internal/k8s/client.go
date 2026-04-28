// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// Clients groups the two flavors of K8s client webhookd uses. The
// controller-runtime client owns typed Patch / Get / Watch on
// SAMLGroupMapping (it knows the operator's scheme); the clientset is
// kept around for any code path that needs core Kubernetes APIs
// (events, namespaces, etc.) — Phase 2 doesn't yet, but the executor
// keeps the option open without forcing every call site to mint its
// own. Both are constructed from the same *rest.Config so they share
// connection behavior, auth, and rate limits.
type Clients struct {
	// CtrlClient is the typed controller-runtime client used for SSA,
	// Get, and List/Watch against the operator's CRDs. Returned as
	// the WithWatch flavor so the executor's cache.ListWatch can call
	// Watch() directly without a separate dynamic client.
	CtrlClient client.WithWatch

	// Clientset is the client-go clientset for any core-K8s-only code
	// paths. Currently unused but cheap to construct from the shared
	// rest.Config.
	Clientset kubernetes.Interface

	// RESTConfig is exposed for tests that need to construct
	// additional clients (e.g., dynamic clients in envtest).
	RESTConfig *rest.Config
}

// NewClients builds the controller-runtime client and the clientset
// from a single *rest.Config. cfg.Kubeconfig, when non-empty, points
// at a kubeconfig file; otherwise the controller-runtime helper
// honors in-cluster service account and `KUBECONFIG` environment.
//
// Startup-time GVK sanity check: if cfg.CR.APIGroup disagrees with the
// imported types' wizapi.GroupVersion.Group, fail fast — the typed
// client will silently send to the imported group, which would be
// confusing if the operator has been moved.
func NewClients(cfg *config.Config) (*Clients, error) {
	if got, want := cfg.CR.APIGroup, wizapi.GroupVersion.Group; got != want {
		return nil, fmt.Errorf(
			"WEBHOOK_CR_API_GROUP=%q disagrees with imported wizapi.GroupVersion.Group=%q; "+
				"reconcile config and rebuild before continuing", got, want)
	}

	restCfg, err := loadRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}

	ctrlClient, err := client.NewWithWatch(restCfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s clientset: %w", err)
	}

	return &Clients{
		CtrlClient: ctrlClient,
		Clientset:  clientset,
		RESTConfig: restCfg,
	}, nil
}

// loadRESTConfig is the single seam where webhookd chooses between
// in-cluster config and a kubeconfig file. Tests construct a
// *rest.Config from envtest and bypass this path entirely.
func loadRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	return ctrl.GetConfig()
}
