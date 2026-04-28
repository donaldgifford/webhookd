// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Errors returned by ExtractString. They wrap the package's
// `ErrInvalidJSON` / sentinel-marker pattern so callers using errors.Is
// can distinguish missing from empty from wrong-type without scraping
// strings — useful for both the dispatcher's response shaping and
// metric labels.
var (
	// ErrFieldMissing — custom field absent from the payload, or
	// JSON null. The JSM tenant either didn't fill it in or the
	// configured field id is wrong.
	ErrFieldMissing = errors.New("custom field missing")

	// ErrFieldEmpty — present but empty after trimming whitespace.
	ErrFieldEmpty = errors.New("custom field empty")

	// ErrFieldType — present but not a JSON string. JSM dropdowns
	// occasionally render as objects with a `value` key; we still
	// require a flat string here. The Phase 2 design only needs
	// strings; richer extraction lives in a follow-up if the tenant
	// uses non-string fields.
	ErrFieldType = errors.New("custom field is not a string")
)

// ExtractString returns the trimmed string value of the custom field
// with id fieldID. Returns sentinel-wrapped errors for missing /
// empty / wrong-type — callers errors.Is them rather than parsing the
// message.
//
// fieldID is the JSM custom-field id (e.g., "customfield_10201"); it's
// supplied by `config.JSMConfig.FieldProviderGroupID` etc., never
// hard-coded.
func ExtractString(p *Payload, fieldID string) (string, error) {
	raw, ok := p.Issue.Fields.CustomFields[fieldID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrFieldMissing, fieldID)
	}
	if string(raw) == "null" {
		return "", fmt.Errorf("%w: %s (null)", ErrFieldMissing, fieldID)
	}

	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrFieldType, fieldID, err)
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", fmt.Errorf("%w: %s", ErrFieldEmpty, fieldID)
	}
	return trimmed, nil
}
