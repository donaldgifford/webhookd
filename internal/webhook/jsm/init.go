// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm

import (
	"github.com/donaldgifford/webhookd/internal/webhook"
)

// init registers the JSM provider with webhook.DefaultRegistry at
// package load time. main.go resolves cfg.EnabledProviders against
// this registry, so the only thing main.go needs to know about the
// JSM provider is its name string. See ADR-0010 + INV-0003 §F-05.
//
//nolint:gochecknoinits // Standard registration pattern; ADR-0010.
func init() {
	webhook.RegisterProvider("jsm", factory)
}

// factory adapts the global *config.Config into the narrow jsm.Config
// the provider needs. Kept as an unexported function — production
// wiring goes through DefaultRegistry; tests that want a Provider
// directly use jsm.New.
func factory(deps webhook.ProviderDeps) (webhook.Provider, error) {
	cfg := deps.Config
	return New(&Config{
		TriggerStatus:        cfg.JSM.TriggerStatus,
		FieldProviderGroupID: cfg.JSM.FieldProviderGroupID,
		FieldRole:            cfg.JSM.FieldRole,
		FieldProject:         cfg.JSM.FieldProject,
		IdentityProviderID:   cfg.JSM.IdentityProviderID,
		Namespace:            cfg.CR.Namespace,
		Signature: SignatureConfig{
			SecretBytes: cfg.SigningSecret,
			SigHeader:   cfg.SignatureHeader,
			TSHeader:    cfg.TimestampHeader,
			Skew:        cfg.TimestampSkew,
		},
		Metrics: deps.Metrics,
	}), nil
}
