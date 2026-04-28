// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm_test

import (
	"errors"
	"testing"

	"github.com/donaldgifford/webhookd/internal/webhook/jsm"
)

func TestDecode_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantErr   error
		wantKey   string
		wantState string
		// wantCustomFields lists field IDs that must be present in the
		// decoded payload. Their values aren't asserted here — that's
		// the extract layer's job.
		wantCustomFields []string
	}{
		{
			name: "valid",
			body: `{
				"issue": {
					"key": "SEC-100",
					"fields": {
						"status": {"name": "Ready to Provision"},
						"customfield_10201": "team-platform",
						"customfield_10202": "admin",
						"customfield_10203": "core"
					}
				}
			}`,
			wantKey:          "SEC-100",
			wantState:        "Ready to Provision",
			wantCustomFields: []string{"customfield_10201", "customfield_10202", "customfield_10203"},
		},
		{
			name:    "missing_issue",
			body:    `{}`,
			wantErr: jsm.ErrMissingIssue,
		},
		{
			name:    "null_issue",
			body:    `{"issue": null}`,
			wantErr: jsm.ErrMissingIssue,
		},
		{
			name:    "empty_issue_key",
			body:    `{"issue": {"key": "", "fields": {"status": {"name": "Ready"}}}}`,
			wantErr: jsm.ErrMissingIssueKey,
		},
		{
			name:    "missing_status",
			body:    `{"issue": {"key": "SEC-1", "fields": {}}}`,
			wantErr: jsm.ErrMissingStatus,
		},
		{
			name:    "empty_status_name",
			body:    `{"issue": {"key": "SEC-1", "fields": {"status": {"name": ""}}}}`,
			wantErr: jsm.ErrMissingStatus,
		},
		{
			name:    "malformed_json",
			body:    `{not json`,
			wantErr: jsm.ErrInvalidJSON,
		},
		{
			name:    "empty_body",
			body:    ``,
			wantErr: jsm.ErrInvalidJSON,
		},
		{
			name: "ignores_extra_top_level_fields",
			body: `{
				"timestamp": 1234,
				"webhookEvent": "jira:issue_updated",
				"user": {"displayName": "alice"},
				"issue": {
					"key": "SEC-200",
					"fields": {
						"status": {"name": "In Progress"}
					}
				}
			}`,
			wantKey:   "SEC-200",
			wantState: "In Progress",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := jsm.Decode([]byte(tc.body))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Decode err = %v, want errors.Is(%v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode err = %v, want nil", err)
			}
			if got.IssueKey() != tc.wantKey {
				t.Errorf("IssueKey() = %q, want %q", got.IssueKey(), tc.wantKey)
			}
			if got.Status() != tc.wantState {
				t.Errorf("Status() = %q, want %q", got.Status(), tc.wantState)
			}
			for _, fid := range tc.wantCustomFields {
				if _, ok := got.Issue.Fields.CustomFields[fid]; !ok {
					t.Errorf("CustomFields missing %q", fid)
				}
			}
		})
	}
}
