// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package k8s_test

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/k8s"
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// TestScheme_RecognizesCoreAndOperatorTypes pins the scheme registry:
// core types must be present (the executor's annotations / status
// patches need them), and the operator's three CRDs must be
// recognized too. Without this the controller-runtime client would
// fail at runtime with "no kind registered" — a regression no other
// test catches.
func TestScheme_RecognizesCoreAndOperatorTypes(t *testing.T) {
	want := []struct {
		group, version, kind string
	}{
		{"", "v1", "ConfigMap"},
		{"", "v1", "Namespace"},
		{wizapi.GroupVersion.Group, wizapi.GroupVersion.Version, "SAMLGroupMapping"},
		{wizapi.GroupVersion.Group, wizapi.GroupVersion.Version, "Project"},
		{wizapi.GroupVersion.Group, wizapi.GroupVersion.Version, "UserRole"},
	}
	for _, w := range want {
		gvk := corev1.SchemeGroupVersion.WithKind(w.kind)
		gvk.Group = w.group
		gvk.Version = w.version
		if !k8s.Scheme.Recognizes(gvk) {
			t.Errorf("Scheme does not recognize %s", gvk)
		}
	}
}

// TestNewClients_RejectsAPIGroupMismatch covers the startup sanity
// check: if the operator was renamed and an operator pinned to the
// new group is deployed alongside a webhookd binary still pointing at
// the old group, the typed Patch call would silently send to the
// imported group and never hit the operator. NewClients refuses to
// build a client in that state.
func TestNewClients_RejectsAPIGroupMismatch(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.CR.APIGroup = "wiz.different.io"

	_, err := k8s.NewClients(cfg)
	if err == nil {
		t.Fatal("NewClients() = nil err, want mismatch error")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_CR_API_GROUP") {
		t.Errorf("err = %v, want substring WEBHOOK_CR_API_GROUP", err)
	}
}

// TestNewClients_BadKubeconfigPath covers the typed-error wrapping.
// Live cluster access is exercised in Phase 4 via envtest; here we
// only need to know that an unreachable kubeconfig surfaces with the
// "k8s config:" prefix so operators can grep their boot logs.
func TestNewClients_BadKubeconfigPath(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.Kubeconfig = "/this/path/should/not/exist/kubeconfig"

	_, err := k8s.NewClients(cfg)
	if err == nil {
		t.Fatal("NewClients() = nil err, want config error")
	}
	if !strings.Contains(err.Error(), "k8s config:") {
		t.Errorf("err = %v, want \"k8s config:\" prefix", err)
	}
}

// minimalConfig returns the smallest *config.Config sufficient for
// NewClients calls. Tests that need richer config build on top of it.
func minimalConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		ShutdownTimeout: 25 * time.Second,
		CR: config.CRConfig{
			APIGroup:    wizapi.GroupVersion.Group,
			APIVersion:  wizapi.GroupVersion.Version,
			Namespace:   "wiz-operator",
			SyncTimeout: 20 * time.Second,
		},
	}
}
