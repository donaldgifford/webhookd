// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook_test

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/donaldgifford/webhookd/internal/k8s"
	"github.com/donaldgifford/webhookd/internal/webhook"
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// envtestREST is shared across executor_test.go tests. Started in
// TestMain when KUBEBUILDER_ASSETS is set; nil otherwise so envtest
// tests skip cleanly when run via plain `go test` outside of make.
var (
	testEnv  *envtest.Environment
	testREST *rest.Config
)

// TestMain wires the envtest lifecycle: start the in-process apiserver
// once, run all package tests, stop. Tests that need envtest skip when
// testREST is nil. We don't call goleak here — envtest leaks
// transport-pool goroutines that aren't worth maintaining IgnoreTopFunction
// allowlists for, and the existing cmd/webhookd integration test
// already exercises the no-leak invariant for real binary code.
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

// requireEnvtest skips the test if envtest isn't available — protects
// developers running `go test` outside `make test`.
func requireEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	if testREST == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; run via 'make test'")
	}
	return testREST
}

// newTestClient builds a controller-runtime client.WithWatch backed by
// the shared envtest. Each test gets a fresh namespace so concurrent
// tests don't collide on CR names.
func newTestClient(t *testing.T) (client.WithWatch, string) {
	t.Helper()
	cfg := requireEnvtest(t)

	c, err := client.NewWithWatch(cfg, client.Options{Scheme: k8s.Scheme})
	if err != nil {
		t.Fatalf("client.NewWithWatch: %v", err)
	}

	ns := newTestNamespace(t, c)
	return c, ns
}

func newTestNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	nsName := "test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if len(nsName) > 63 {
		nsName = nsName[:63]
	}
	nsName = sanitizeDNSLabel(nsName)
	corev1ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: nsName},
	}
	if err := c.Create(t.Context(), corev1ns); err != nil {
		t.Fatalf("create namespace %s: %v", nsName, err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), corev1ns)
	})
	return nsName
}

func sanitizeDNSLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	if out == "" {
		out = "test"
	}
	return out
}

// TestExecutor_Execute_Noop returns ResultNoop with the supplied reason
// and never touches the cluster — verified by passing a nil client
// that would panic on any method call.
func TestExecutor_Execute_Noop(t *testing.T) {
	t.Parallel()
	exe := webhook.NewExecutor(nil, nil, nil, webhook.ExecutorConfig{
		Namespace:   "wiz-operator",
		SyncTimeout: time.Second,
	})
	res := exe.Execute(t.Context(), webhook.NoopAction{Reason: "ticket not in trigger status"})
	if res.Kind != webhook.ResultNoop {
		t.Errorf("Kind = %v, want ResultNoop", res.Kind)
	}
	if res.Reason != "ticket not in trigger status" {
		t.Errorf("Reason = %q, want pass-through", res.Reason)
	}
}

// TestExecutor_Execute_HappyPath drives the full apply + watch path:
// the test goroutine plays the role of the operator by waiting for
// the CR to appear, then writing Status with Ready=True. The executor
// returns ResultReady once observedGeneration catches up.
func TestExecutor_Execute_HappyPath(t *testing.T) {
	c, ns := newTestClient(t)
	exe := webhook.NewExecutor(c, nil, nil, webhook.ExecutorConfig{
		Namespace:    ns,
		FieldManager: "webhookd-test",
		SyncTimeout:  20 * time.Second,
	})

	act := webhook.ApplySAMLGroupMapping{
		IssueKey: "SEC-100",
		Spec: wizapi.SAMLGroupMappingSpec{
			IdentityProviderID: "okta-prod",
			ProviderGroupID:    "team-platform",
			RoleRef:            wizapi.RoleRef{Name: "admin"},
			ProjectRefs:        []wizapi.ProjectRef{{Name: "core"}},
		},
	}

	// Start the operator-impersonator goroutine before Execute so we
	// never miss the first reconcile window.
	done := make(chan struct{})
	go func() {
		defer close(done)
		markReady(t, c, ns, "jsm-sec-100")
	}()

	res := exe.Execute(t.Context(), act)
	if res.Kind != webhook.ResultReady {
		t.Fatalf("Kind = %v (%s), want ResultReady", res.Kind, res.Reason)
	}
	if res.CRName != "jsm-sec-100" {
		t.Errorf("CRName = %q, want jsm-sec-100", res.CRName)
	}
	if res.Namespace != ns {
		t.Errorf("Namespace = %q, want %q", res.Namespace, ns)
	}
	if res.ObservedGeneration < 1 {
		t.Errorf("ObservedGeneration = %d, want >= 1", res.ObservedGeneration)
	}
	<-done
}

// TestExecutor_Execute_Timeout asserts that an unhandled CR (no
// operator playing, status never written) resolves to ResultTimeout
// once SyncTimeout expires — distinct from ResultTransientFailure
// because the apply itself succeeded.
func TestExecutor_Execute_Timeout(t *testing.T) {
	c, ns := newTestClient(t)
	exe := webhook.NewExecutor(c, nil, nil, webhook.ExecutorConfig{
		Namespace:    ns,
		FieldManager: "webhookd-test",
		SyncTimeout:  500 * time.Millisecond,
	})

	res := exe.Execute(t.Context(), webhook.ApplySAMLGroupMapping{
		IssueKey: "SEC-200",
		Spec: wizapi.SAMLGroupMappingSpec{
			IdentityProviderID: "okta-prod",
			ProviderGroupID:    "team-platform",
			RoleRef:            wizapi.RoleRef{Name: "admin"},
			ProjectRefs:        []wizapi.ProjectRef{{Name: "core"}},
		},
	})
	if res.Kind != webhook.ResultTimeout {
		t.Fatalf("Kind = %v (%s), want ResultTimeout", res.Kind, res.Reason)
	}
	if res.CRName != "jsm-sec-200" {
		t.Errorf("CRName = %q, want jsm-sec-200", res.CRName)
	}
}

// TestExecutor_Execute_ReadyFalseIsTransient codifies IMPL-0002 §3:
// Ready=False is treated as still-pending until the deadline expires,
// not as a terminal failure. We write a Ready=False status, expect the
// deadline to fire, and assert ResultTimeout — never a 4xx-mapped kind.
func TestExecutor_Execute_ReadyFalseIsTransient(t *testing.T) {
	c, ns := newTestClient(t)
	exe := webhook.NewExecutor(c, nil, nil, webhook.ExecutorConfig{
		Namespace:    ns,
		FieldManager: "webhookd-test",
		SyncTimeout:  500 * time.Millisecond,
	})

	go func() {
		// Wait for the CR to appear, then mark Ready=False.
		obj := &wizapi.SAMLGroupMapping{}
		key := client.ObjectKey{Namespace: ns, Name: "jsm-sec-300"}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := c.Get(context.Background(), key, obj); err == nil {
				obj.Status.ObservedGeneration = obj.Generation
				obj.Status.Conditions = []metav1.Condition{{
					Type:               wizapi.ConditionTypeReady,
					Status:             metav1.ConditionFalse,
					Reason:             "OperatorReconciling",
					Message:            "talking to wiz",
					LastTransitionTime: metav1.Now(),
				}}
				_ = c.Status().Update(context.Background(), obj)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	res := exe.Execute(t.Context(), webhook.ApplySAMLGroupMapping{
		IssueKey: "SEC-300",
		Spec: wizapi.SAMLGroupMappingSpec{
			IdentityProviderID: "okta-prod",
			ProviderGroupID:    "team-platform",
			RoleRef:            wizapi.RoleRef{Name: "admin"},
			ProjectRefs:        []wizapi.ProjectRef{{Name: "core"}},
		},
	})
	if res.Kind != webhook.ResultTimeout {
		t.Fatalf("Kind = %v (%s), want ResultTimeout (Ready=False must NOT terminate)", res.Kind, res.Reason)
	}
}

// TestExecutor_Execute_InvalidSpec relies on the CRD's openAPIV3Schema
// validation (minLength on identityProviderId). The apply step gets
// 422 IsInvalid back from the apiserver, which classifyK8sErr maps to
// ResultUnprocessable.
func TestExecutor_Execute_InvalidSpec(t *testing.T) {
	c, ns := newTestClient(t)
	exe := webhook.NewExecutor(c, nil, nil, webhook.ExecutorConfig{
		Namespace:    ns,
		FieldManager: "webhookd-test",
		SyncTimeout:  2 * time.Second,
	})

	res := exe.Execute(t.Context(), webhook.ApplySAMLGroupMapping{
		IssueKey: "SEC-400",
		Spec: wizapi.SAMLGroupMappingSpec{
			IdentityProviderID: "", // violates minLength: 1
			ProviderGroupID:    "team-platform",
			RoleRef:            wizapi.RoleRef{Name: "admin"},
			ProjectRefs:        []wizapi.ProjectRef{{Name: "core"}},
		},
	})
	if res.Kind != webhook.ResultUnprocessable {
		t.Fatalf("Kind = %v (%s), want ResultUnprocessable", res.Kind, res.Reason)
	}
}

// TestExecutor_Execute_Idempotent applies the same spec twice with the
// same fieldManager and asserts metadata.generation stays at 1. SSA
// noops when the desired state already matches actual state.
func TestExecutor_Execute_Idempotent(t *testing.T) {
	c, ns := newTestClient(t)
	exe := webhook.NewExecutor(c, nil, nil, webhook.ExecutorConfig{
		Namespace:    ns,
		FieldManager: "webhookd-test",
		SyncTimeout:  2 * time.Second,
		// Pin Now so the applied-at annotation is identical between
		// calls — otherwise SSA would bump generation each apply.
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	act := webhook.ApplySAMLGroupMapping{
		IssueKey: "SEC-500",
		Spec: wizapi.SAMLGroupMappingSpec{
			IdentityProviderID: "okta-prod",
			ProviderGroupID:    "team-platform",
			RoleRef:            wizapi.RoleRef{Name: "admin"},
			ProjectRefs:        []wizapi.ProjectRef{{Name: "core"}},
		},
	}

	// First apply: timeout is fine; we only care that the CR lands.
	_ = exe.Execute(t.Context(), act)

	got1 := &wizapi.SAMLGroupMapping{}
	if err := c.Get(t.Context(),
		client.ObjectKey{Namespace: ns, Name: "jsm-sec-500"}, got1); err != nil {
		t.Fatalf("first get: %v", err)
	}
	gen1 := got1.Generation

	// Second apply with identical inputs.
	_ = exe.Execute(t.Context(), act)

	got2 := &wizapi.SAMLGroupMapping{}
	if err := c.Get(t.Context(),
		client.ObjectKey{Namespace: ns, Name: "jsm-sec-500"}, got2); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if got2.Generation != gen1 {
		t.Errorf("generation bumped: %d -> %d (idempotent SSA should hold steady)", gen1, got2.Generation)
	}
}

// TestClassifyK8sErr table-tests the apply-step error classification
// in isolation — no envtest needed. Keeps the mapping deterministic
// even when CRD validation behavior changes between K8s versions.
func TestClassifyK8sErr(t *testing.T) {
	t.Parallel()
	gvk := schema.GroupKind{Group: "wiz.webhookd.io", Kind: "SAMLGroupMapping"}
	gr := schema.GroupResource{Group: gvk.Group, Resource: "samlgroupmappings"}

	tests := []struct {
		name string
		err  error
		want webhook.ResultKind
	}{
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("rbac")), webhook.ResultInternalError},
		{"invalid", apierrors.NewInvalid(gvk, "x", nil), webhook.ResultUnprocessable},
		{"conflict", apierrors.NewConflict(gr, "x", errors.New("conflict")), webhook.ResultTransientFailure},
		{"too_many_requests", apierrors.NewTooManyRequests("rate limited", 5), webhook.ResultTransientFailure},
		{"server_timeout", apierrors.NewServerTimeout(gr, "patch", 5), webhook.ResultTransientFailure},
		{"service_unavailable", apierrors.NewServiceUnavailable("down"), webhook.ResultTransientFailure},
		{"random_error", errors.New("network broken"), webhook.ResultTransientFailure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := webhook.ClassifyK8sErrForTest(tc.err, "ns", "name")
			if got.Kind != tc.want {
				t.Errorf("classifyK8sErr(%v) = %v, want %v", tc.err, got.Kind, tc.want)
			}
		})
	}
}

// markReady is the operator-impersonator: poll for the CR webhookd
// applied, then write a Ready=True status. Returns when the status
// patch lands or t deadline expires.
func markReady(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		obj := &wizapi.SAMLGroupMapping{}
		err := c.Get(context.Background(),
			client.ObjectKey{Namespace: namespace, Name: name}, obj)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		obj.Status.ObservedGeneration = obj.Generation
		obj.Status.Conditions = []metav1.Condition{{
			Type:               wizapi.ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "wiz happy",
			LastTransitionTime: metav1.Now(),
		}}
		if err := c.Status().Update(context.Background(), obj); err != nil {
			t.Logf("status update failed (will retry): %v", err)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return
	}
	t.Errorf("timed out waiting to mark %s/%s Ready", namespace, name)
}
