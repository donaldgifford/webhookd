// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package wizapi

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionTypeReady is the standard condition surfaced by the
// operator. webhookd treats Ready=True as success; anything else is
// still-pending until the watch deadline (per IMPL-0002 §4).
const ConditionTypeReady = "Ready"

// SAMLGroupMapping maps an SSO provider group to a Wiz role on a set
// of Wiz projects. webhookd writes one CR per JSM ticket; the Wiz
// operator reconciles it against the Wiz API.
//
// Cardinality in Phase 2 is one ticket = one CR with one project and
// one role; ProjectRefs always carries a single element. The list
// shape is preserved to match the operator CRD.
type SAMLGroupMapping struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SAMLGroupMappingSpec   `json:"spec,omitempty"`
	Status SAMLGroupMappingStatus `json:"status,omitempty"`
}

// SAMLGroupMappingSpec is the desired state of a SAML-to-Wiz mapping.
type SAMLGroupMappingSpec struct {
	// IdentityProviderID is the Wiz-side identity provider this group
	// belongs to. Static per webhookd install.
	IdentityProviderID string `json:"identityProviderId"`

	// ProviderGroupID is the SSO group name (e.g. an Okta group) the
	// requesting team owns. Extracted from JSM.
	ProviderGroupID string `json:"providerGroupId"`

	// Description is human-readable context surfaced in Wiz audit
	// logs. webhookd derives this from the JSM issue key.
	Description string `json:"description,omitempty"`

	// RoleRef references a UserRole CR by name; the operator
	// resolves the name to a Wiz role.
	RoleRef RoleRef `json:"roleRef"`

	// ProjectRefs reference Project CRs by name; the operator
	// resolves names to Wiz projects.
	ProjectRefs []ProjectRef `json:"projectRefs"`
}

// RoleRef references a UserRole CR by name.
type RoleRef struct {
	Name string `json:"name"`
}

// ProjectRef references a Project CR by name.
type ProjectRef struct {
	Name string `json:"name"`
}

// SAMLGroupMappingStatus is the observed state populated by the
// operator. ObservedGeneration must catch up to metadata.generation
// before webhookd trusts the Ready condition.
type SAMLGroupMappingStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// SAMLGroupMappingList satisfies client-go's List/Watch contract.
type SAMLGroupMappingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SAMLGroupMapping `json:"items"`
}

// Project mirrors the Wiz operator's Project CR. webhookd does not
// write Projects in Phase 2 — only references them by name from a
// SAMLGroupMapping. Fields beyond Name are omitted from the stub
// because webhookd does not consume them; the operator owns the
// canonical schema.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

// ProjectSpec is a placeholder for the operator's Project schema.
type ProjectSpec struct {
	Name string `json:"name,omitempty"`
}

// ProjectStatus mirrors the operator's Ready/observedGeneration
// pattern.
type ProjectStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// ProjectList satisfies client-go's List/Watch contract.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

// UserRole mirrors the Wiz operator's UserRole CR. Same minimal-stub
// rationale as Project.
type UserRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserRoleSpec   `json:"spec,omitempty"`
	Status UserRoleStatus `json:"status,omitempty"`
}

// UserRoleSpec is a placeholder for the operator's UserRole schema.
type UserRoleSpec struct {
	Name string `json:"name,omitempty"`
}

// UserRoleStatus mirrors the operator's Ready/observedGeneration
// pattern.
type UserRoleStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// UserRoleList satisfies client-go's List/Watch contract.
type UserRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UserRole `json:"items"`
}
