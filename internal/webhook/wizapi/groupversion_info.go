// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

// Package wizapi is a temporary stub of the Wiz operator's API types.
//
// This package mirrors the YAML samples in docs/examples/samples/
// until github.com/donaldgifford/wiz-operator/api/v1alpha1 is published
// as a Go module. Once the operator's API is consumable, this package
// becomes a one-line re-export and the hand-written types and DeepCopy
// methods are deleted. Every consumer imports through this package so
// the swap is mechanical.
//
// The shape is intentionally minimal: webhookd writes SAMLGroupMapping
// and reads its status, but only references Project and UserRole by
// name — those types are stubbed to satisfy scheme registration today
// and pre-validation lookups in a future phase.
package wizapi

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group/version for the Wiz operator's CRDs.
// The group is the runtime config default; if WEBHOOK_CR_API_GROUP is
// overridden at startup, NewClient compares the two and fails fast on
// disagreement (typed clients can't honor a runtime GVK override).
var GroupVersion = schema.GroupVersion{
	Group:   "wiz.webhookd.io",
	Version: "v1alpha1",
}

// SchemeBuilder collects the type registrations for AddToScheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme registers wizapi types with the supplied scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&SAMLGroupMapping{}, &SAMLGroupMappingList{},
		&Project{}, &ProjectList{},
		&UserRole{}, &UserRoleList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
