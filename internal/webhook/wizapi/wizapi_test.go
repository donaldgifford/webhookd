// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package wizapi_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// TestAddToScheme_RegistersAllKnownTypes is the substantive scheme
// test: AddToScheme must register every kind the controller-runtime
// client will dispatch on, otherwise typed Patch / Get fails at
// runtime with "no kind registered." A regression here would not
// surface until envtest in Phase 4.
func TestAddToScheme_RegistersAllKnownTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := wizapi.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme = %v, want nil", err)
	}

	want := []string{"SAMLGroupMapping", "SAMLGroupMappingList", "Project", "ProjectList", "UserRole", "UserRoleList"}
	for _, kind := range want {
		gvk := wizapi.GroupVersion.WithKind(kind)
		if !scheme.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}

// TestGroupVersion_MatchesYAMLSamples pins the API group/version
// against docs/examples/samples/. If the operator ever decides to
// rev to v1beta1 (or back away from wiz.webhookd.io), this test
// fails loudly and the wizapi stub stays in lockstep.
func TestGroupVersion_MatchesYAMLSamples(t *testing.T) {
	want := schema.GroupVersion{Group: "wiz.webhookd.io", Version: "v1alpha1"}
	if wizapi.GroupVersion != want {
		t.Errorf("GroupVersion = %v, want %v", wizapi.GroupVersion, want)
	}
}

// TestSAMLGroupMapping_DeepCopy_PreservesProjectRefs covers the only
// non-trivial DeepCopyInto path in the stub (the others are scalar
// or call into apimachinery helpers). Mutating the deep copy must
// not alias back into the original.
func TestSAMLGroupMapping_DeepCopy_PreservesProjectRefs(t *testing.T) {
	original := &wizapi.SAMLGroupMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "jsm-sec-1234", Namespace: "wiz-operator"},
		Spec: wizapi.SAMLGroupMappingSpec{
			IdentityProviderID: "saml-idp-1",
			ProviderGroupID:    "okta-platform",
			RoleRef:            wizapi.RoleRef{Name: "platform-engineer"},
			ProjectRefs:        []wizapi.ProjectRef{{Name: "platform-team"}},
		},
		Status: wizapi.SAMLGroupMappingStatus{
			ObservedGeneration: 1,
			Conditions: []metav1.Condition{
				{Type: wizapi.ConditionTypeReady, Status: metav1.ConditionTrue, Reason: "Synced"},
			},
		},
	}

	clone := original.DeepCopy()
	clone.Spec.ProjectRefs[0].Name = "mutated"
	clone.Status.Conditions[0].Reason = "Mutated"

	if got := original.Spec.ProjectRefs[0].Name; got != "platform-team" {
		t.Errorf("original.Spec.ProjectRefs[0].Name = %q after DeepCopy mutation, want %q", got, "platform-team")
	}
	if got := original.Status.Conditions[0].Reason; got != "Synced" {
		t.Errorf("original.Status.Conditions[0].Reason = %q after DeepCopy mutation, want %q", got, "Synced")
	}
}
