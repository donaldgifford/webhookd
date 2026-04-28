// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

// Package k8s wires webhookd's Kubernetes access: a single shared
// runtime.Scheme that recognizes core types and the operator's CRDs,
// plus the construction of the controller-runtime client and the
// client-go clientset that share one rest.Config. Every other package
// that talks to Kubernetes imports through this package — no
// alternative paths to `ctrl.GetConfig()` or `runtime.NewScheme()` are
// allowed.
//
// The dual-client decision (controller-runtime + clientset) is in
// IMPL-0002 §Resolved Decisions §6: the executor uses the typed
// controller-runtime client for SSA, and the clientset's
// `cache.ListWatch` underpins `watch.UntilWithSync`.
package k8s

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// Scheme is the package-level runtime.Scheme shared by the
// controller-runtime client and any code that needs to recognize
// SAMLGroupMapping / Project / UserRole. It is populated at init time
// so importers don't have to remember to call AddToScheme.
//
// Tests that need an isolated scheme should construct their own with
// runtime.NewScheme() rather than mutate this one.
var Scheme = runtime.NewScheme()

func init() {
	// utilruntime.Must converts an AddToScheme error into a panic.
	// The only realistic source of error is a duplicate type, which
	// would mean the operator's API and core types collide — that is
	// a programming error worth crashing the binary at startup.
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(wizapi.AddToScheme(Scheme))
}
