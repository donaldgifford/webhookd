// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/webhook"
)

// fakeProvider is a minimum-viable Provider for registry tests.
type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string                              { return f.name }
func (*fakeProvider) VerifySignature(*http.Request, []byte) error { return nil }
func (*fakeProvider) Handle(context.Context, []byte) (webhook.Action, error) {
	return webhook.NoopAction{}, nil
}

func (*fakeProvider) BuildResponse(webhook.ExecResult, string, string) any { return nil }

func TestRegistry_Build_ReturnsProvidersInEnabledOrder(t *testing.T) {
	t.Parallel()
	r := webhook.NewRegistry()
	r.Register("alpha", func(webhook.ProviderDeps) (webhook.Provider, error) {
		return &fakeProvider{name: "alpha"}, nil
	})
	r.Register("beta", func(webhook.ProviderDeps) (webhook.Provider, error) {
		return &fakeProvider{name: "beta"}, nil
	})

	cfg := &config.Config{EnabledProviders: []string{"beta", "alpha"}}
	got, err := r.Build(webhook.ProviderDeps{Config: cfg})
	if err != nil {
		t.Fatalf("Build err = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Name() != "beta" || got[1].Name() != "alpha" {
		t.Errorf("order = %q,%q, want beta,alpha", got[0].Name(), got[1].Name())
	}
}

func TestRegistry_Build_UnknownProviderErrors(t *testing.T) {
	t.Parallel()
	r := webhook.NewRegistry()
	cfg := &config.Config{EnabledProviders: []string{"ghost"}}
	_, err := r.Build(webhook.ProviderDeps{Config: cfg})
	if err == nil {
		t.Fatal("Build err = nil, want unknown-provider error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err = %q, want it to mention provider name", err)
	}
}

func TestRegistry_Build_FactoryErrorPropagates(t *testing.T) {
	t.Parallel()
	r := webhook.NewRegistry()
	boom := errors.New("boom")
	r.Register("broken", func(webhook.ProviderDeps) (webhook.Provider, error) {
		return nil, boom
	})
	cfg := &config.Config{EnabledProviders: []string{"broken"}}
	_, err := r.Build(webhook.ProviderDeps{Config: cfg})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want errors.Is(boom)", err)
	}
}

func TestRegistry_Register_DuplicatePanics(t *testing.T) {
	t.Parallel()
	r := webhook.NewRegistry()
	r.Register("dup", func(webhook.ProviderDeps) (webhook.Provider, error) {
		return &fakeProvider{name: "dup"}, nil
	})
	defer func() {
		if recover() == nil {
			t.Error("second Register did not panic")
		}
	}()
	r.Register("dup", func(webhook.ProviderDeps) (webhook.Provider, error) {
		return &fakeProvider{name: "dup-again"}, nil
	})
}

func TestRegistry_Build_EmptyEnabledReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := webhook.NewRegistry()
	cfg := &config.Config{EnabledProviders: nil}
	got, err := r.Build(webhook.ProviderDeps{Config: cfg})
	if err != nil {
		t.Fatalf("Build err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}
