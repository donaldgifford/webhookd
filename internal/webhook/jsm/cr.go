// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// AnnotationIssue is the metadata.annotations key carrying the JSM
// issue key on every CR webhookd applies. JSM-specific and so lives
// here rather than in the executor; see INV-0003 §F-03.
const AnnotationIssue = "webhookd.io/jsm-issue-key"

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

// BuildSAMLGroupMapping wraps spec + naming + namespace into a fully
// formed typed CR ready to hand to webhook.ApplyAction.Object. The
// generic executor is provider-agnostic, so construction of the
// concrete client.Object now lives here in the provider package.
//
// spec is taken by pointer because SAMLGroupMappingSpec carries a
// slice + several strings and trips the gocritic hugeParam threshold
// otherwise; callers hand in &spec from a local literal.
func BuildSAMLGroupMapping(name, namespace string, spec *wizapi.SAMLGroupMappingSpec) *wizapi.SAMLGroupMapping {
	return &wizapi.SAMLGroupMapping{
		TypeMeta: metav1.TypeMeta{
			APIVersion: wizapi.GroupVersion.String(),
			Kind:       "SAMLGroupMapping",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: *spec,
	}
}

// CRName derives the K8s CR name from a JSM issue key. JSM issue
// keys (e.g. "SEC-100") are uppercase by convention; K8s names must
// be lowercase RFC 1123 labels, so we lower-case and prefix with
// "jsm-" so operators can grep by source.
func CRName(issueKey string) string {
	return "jsm-" + strings.ToLower(issueKey)
}

// IsReady is the ReadyCheck closure passed to webhook.ApplyAction.
// It inspects a SAMLGroupMapping (delivered by the executor's Watch
// loop as a client.Object) and reports whether the operator marked
// it Ready=True and observed the apply-time generation. Returns
// (false, 0) for any non-SAMLGroupMapping payload, which leaves the
// executor's loop waiting for the right event.
func IsReady(obj client.Object, applyGen int64) (bool, int64) {
	cr, ok := obj.(*wizapi.SAMLGroupMapping)
	if !ok {
		return false, 0
	}
	if cr.Status.ObservedGeneration < applyGen {
		return false, cr.Status.ObservedGeneration
	}
	for _, c := range cr.Status.Conditions {
		if c.Type == wizapi.ConditionTypeReady && c.Status == metav1.ConditionTrue {
			return true, cr.Status.ObservedGeneration
		}
	}
	return false, cr.Status.ObservedGeneration
}
