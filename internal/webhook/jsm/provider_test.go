// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/donaldgifford/webhookd/internal/webhook"
	"github.com/donaldgifford/webhookd/internal/webhook/jsm"
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

const (
	fidProvider = "customfield_10201"
	fidRole     = "customfield_10202"
	fidProject  = "customfield_10203"
)

func newTestProvider(_ *testing.T) *jsm.Provider {
	return jsm.New(&jsm.Config{
		TriggerStatus:        "Ready to Provision",
		FieldProviderGroupID: fidProvider,
		FieldRole:            fidRole,
		FieldProject:         fidProject,
		IdentityProviderID:   "okta-prod",
		Namespace:            "wiz-operator",
		Signature: jsm.SignatureConfig{
			SecretBytes: []byte("topsecret"),
			SigHeader:   "X-Webhook-Signature",
			TSHeader:    "X-Webhook-Timestamp",
			Skew:        5 * time.Minute,
			Now:         func() time.Time { return time.Unix(1_700_000_000, 0) },
		},
	})
}

func TestProvider_Name(t *testing.T) {
	t.Parallel()
	if got := newTestProvider(t).Name(); got != "jsm" {
		t.Errorf("Name() = %q, want jsm", got)
	}
}

func TestProvider_Handle_NoopOnNonTriggerStatus(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"issue": {
			"key": "SEC-100",
			"fields": {
				"status": {"name": "In Progress"},
				"customfield_10201": "team-platform",
				"customfield_10202": "admin",
				"customfield_10203": "core"
			}
		}
	}`)
	act, err := newTestProvider(t).Handle(t.Context(), body)
	if err != nil {
		t.Fatalf("Handle err = %v, want nil", err)
	}
	noop, ok := act.(webhook.NoopAction)
	if !ok {
		t.Fatalf("Handle = %T, want NoopAction", act)
	}
	if !strings.Contains(noop.Reason, "SEC-100") || !strings.Contains(noop.Reason, "In Progress") {
		t.Errorf("Reason = %q, want context about the ticket and status", noop.Reason)
	}
}

func TestProvider_Handle_ApplyOnTriggerStatus(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"issue": {
			"key": "SEC-100",
			"fields": {
				"status": {"name": "Ready to Provision"},
				"customfield_10201": "team-platform",
				"customfield_10202": "admin",
				"customfield_10203": "core"
			}
		}
	}`)
	act, err := newTestProvider(t).Handle(t.Context(), body)
	if err != nil {
		t.Fatalf("Handle err = %v, want nil", err)
	}
	apply, ok := act.(webhook.ApplyAction)
	if !ok {
		t.Fatalf("Handle = %T, want ApplyAction", act)
	}
	if apply.Kind != "SAMLGroupMapping" {
		t.Errorf("Kind = %q, want SAMLGroupMapping", apply.Kind)
	}
	if apply.Source != "jsm" {
		t.Errorf("Source = %q, want jsm", apply.Source)
	}
	if got := apply.Annotations[jsm.AnnotationIssue]; got != "SEC-100" {
		t.Errorf("Annotations[%s] = %q, want SEC-100", jsm.AnnotationIssue, got)
	}
	if apply.Object.GetName() != "jsm-sec-100" {
		t.Errorf("Object.Name = %q, want jsm-sec-100", apply.Object.GetName())
	}
	if apply.Object.GetNamespace() != "wiz-operator" {
		t.Errorf("Object.Namespace = %q, want wiz-operator", apply.Object.GetNamespace())
	}
	cr, ok := apply.Object.(*wizapi.SAMLGroupMapping)
	if !ok {
		t.Fatalf("Object = %T, want *wizapi.SAMLGroupMapping", apply.Object)
	}
	if cr.Spec.IdentityProviderID != "okta-prod" {
		t.Errorf("IdentityProviderID = %q, want okta-prod", cr.Spec.IdentityProviderID)
	}
	if cr.Spec.ProviderGroupID != "team-platform" {
		t.Errorf("ProviderGroupID = %q, want team-platform", cr.Spec.ProviderGroupID)
	}
	if cr.Spec.RoleRef.Name != "admin" {
		t.Errorf("RoleRef.Name = %q, want admin", cr.Spec.RoleRef.Name)
	}
	if len(cr.Spec.ProjectRefs) != 1 || cr.Spec.ProjectRefs[0].Name != "core" {
		t.Errorf("ProjectRefs = %+v, want [{core}]", cr.Spec.ProjectRefs)
	}
	if cr.Spec.Description != "Provisioned from JSM SEC-100" {
		t.Errorf("Description = %q, want %q", cr.Spec.Description, "Provisioned from JSM SEC-100")
	}
	if _, ok := apply.ListObject.(*wizapi.SAMLGroupMappingList); !ok {
		t.Errorf("ListObject = %T, want *wizapi.SAMLGroupMappingList", apply.ListObject)
	}
	if apply.ReadyCheck == nil {
		t.Error("ReadyCheck = nil, want non-nil closure")
	}
}

func TestProvider_Handle_BadRequestErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{"malformed_json", []byte(`{not json`)},
		{"missing_issue", []byte(`{}`)},
		{"missing_status", []byte(`{"issue": {"key": "SEC-1", "fields": {}}}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := newTestProvider(t).Handle(t.Context(), tc.body)
			if !errors.Is(err, webhook.ErrBadRequest) {
				t.Errorf("err = %v, want errors.Is(webhook.ErrBadRequest)", err)
			}
		})
	}
}

func TestProvider_Handle_MissingFieldsAreBadRequest(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"issue": {
			"key": "SEC-100",
			"fields": {
				"status": {"name": "Ready to Provision"},
				"customfield_10201": "team-platform"
			}
		}
	}`)
	_, err := newTestProvider(t).Handle(t.Context(), body)
	if !errors.Is(err, webhook.ErrBadRequest) {
		t.Errorf("missing role/project: err = %v, want errors.Is(webhook.ErrBadRequest)", err)
	}
}

func TestProvider_Handle_WrongFieldTypeIsUnprocessable(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"issue": {
			"key": "SEC-100",
			"fields": {
				"status": {"name": "Ready to Provision"},
				"customfield_10201": 42,
				"customfield_10202": "admin",
				"customfield_10203": "core"
			}
		}
	}`)
	_, err := newTestProvider(t).Handle(t.Context(), body)
	if !errors.Is(err, webhook.ErrUnprocessable) {
		t.Errorf("err = %v, want errors.Is(webhook.ErrUnprocessable)", err)
	}
}
