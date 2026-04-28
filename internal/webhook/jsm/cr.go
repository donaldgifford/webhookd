// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm

import (
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// BuildSpec assembles the SAMLGroupMappingSpec from the four
// JSM-derived strings plus the static identity provider id. The
// project ref list is always single-element in Phase 2 (one ticket =
// one CR with one project + one role); the slice shape is preserved
// because the operator's CRD takes a list.
//
// Pure constructor — no defaults, no fallbacks. Callers that need a
// derived description should use BuildDescription before calling here.
func BuildSpec(providerGroupID, role, project, identityProviderID, description string) wizapi.SAMLGroupMappingSpec {
	return wizapi.SAMLGroupMappingSpec{
		IdentityProviderID: identityProviderID,
		ProviderGroupID:    providerGroupID,
		Description:        description,
		RoleRef:            wizapi.RoleRef{Name: role},
		ProjectRefs:        []wizapi.ProjectRef{{Name: project}},
	}
}

// BuildDescription returns the human-readable Description field
// stamped onto every CR webhookd creates. The format is stable so
// operators can grep Wiz audit logs back to the originating ticket.
func BuildDescription(issueKey string) string {
	return "Provisioned from JSM " + issueKey
}
