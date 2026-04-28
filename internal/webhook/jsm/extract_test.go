// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/donaldgifford/webhookd/internal/webhook/jsm"
)

func TestExtractString_TableDriven(t *testing.T) {
	t.Parallel()

	const fid = "customfield_10201"

	tests := []struct {
		name    string
		raw     map[string]json.RawMessage
		want    string
		wantErr error
	}{
		{
			name: "string_present",
			raw:  map[string]json.RawMessage{fid: json.RawMessage(`"team-platform"`)},
			want: "team-platform",
		},
		{
			name: "trimmed_whitespace",
			raw:  map[string]json.RawMessage{fid: json.RawMessage(`"  team-platform  "`)},
			want: "team-platform",
		},
		{
			name:    "missing_field",
			raw:     map[string]json.RawMessage{},
			wantErr: jsm.ErrFieldMissing,
		},
		{
			name:    "null_field",
			raw:     map[string]json.RawMessage{fid: json.RawMessage(`null`)},
			wantErr: jsm.ErrFieldMissing,
		},
		{
			name:    "empty_string",
			raw:     map[string]json.RawMessage{fid: json.RawMessage(`""`)},
			wantErr: jsm.ErrFieldEmpty,
		},
		{
			name:    "whitespace_only",
			raw:     map[string]json.RawMessage{fid: json.RawMessage(`"   "`)},
			wantErr: jsm.ErrFieldEmpty,
		},
		{
			name:    "number",
			raw:     map[string]json.RawMessage{fid: json.RawMessage(`42`)},
			wantErr: jsm.ErrFieldType,
		},
		{
			name:    "object",
			raw:     map[string]json.RawMessage{fid: json.RawMessage(`{"value":"team"}`)},
			wantErr: jsm.ErrFieldType,
		},
		{
			name:    "array",
			raw:     map[string]json.RawMessage{fid: json.RawMessage(`["team"]`)},
			wantErr: jsm.ErrFieldType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &jsm.Payload{
				Issue: jsm.Issue{
					Fields: jsm.IssueFields{CustomFields: tc.raw},
				},
			}
			got, err := jsm.ExtractString(p, fid)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ExtractString err = %v, want errors.Is(%v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractString err = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("ExtractString = %q, want %q", got, tc.want)
			}
		})
	}
}
