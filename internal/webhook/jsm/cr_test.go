// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm_test

import (
	"testing"

	"github.com/donaldgifford/webhookd/internal/webhook/jsm"
)

func TestBuildSpec(t *testing.T) {
	t.Parallel()

	got := jsm.BuildSpec("team-platform", "admin", "core", "okta-prod", "Provisioned from JSM SEC-100")

	if got.IdentityProviderID != "okta-prod" {
		t.Errorf("IdentityProviderID = %q, want okta-prod", got.IdentityProviderID)
	}
	if got.ProviderGroupID != "team-platform" {
		t.Errorf("ProviderGroupID = %q, want team-platform", got.ProviderGroupID)
	}
	if got.Description != "Provisioned from JSM SEC-100" {
		t.Errorf("Description = %q, unexpected", got.Description)
	}
	if got.RoleRef.Name != "admin" {
		t.Errorf("RoleRef.Name = %q, want admin", got.RoleRef.Name)
	}
	if len(got.ProjectRefs) != 1 {
		t.Fatalf("ProjectRefs len = %d, want 1", len(got.ProjectRefs))
	}
	if got.ProjectRefs[0].Name != "core" {
		t.Errorf("ProjectRefs[0].Name = %q, want core", got.ProjectRefs[0].Name)
	}
}

func TestBuildDescription(t *testing.T) {
	t.Parallel()
	if got := jsm.BuildDescription("SEC-100"); got != "Provisioned from JSM SEC-100" {
		t.Errorf("BuildDescription = %q, want %q", got, "Provisioned from JSM SEC-100")
	}
}
