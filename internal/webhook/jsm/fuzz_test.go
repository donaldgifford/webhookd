// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/donaldgifford/webhookd/internal/webhook/jsm"
)

// FuzzJSMDecode exercises Decode with arbitrary inputs. The contract:
// every call returns either (non-nil, nil) or (nil, non-nil) — never
// panics, never (nil, nil). Seeded with the canonical JSM sample plus
// a handful of malformed variants so the corpus exercises every
// sentinel error class.
func FuzzJSMDecode(f *testing.F) {
	sample, err := os.ReadFile(filepath.Join("testdata", "sample.json"))
	if err != nil {
		f.Fatalf("read sample.json: %v", err)
	}
	seeds := [][]byte{
		sample,
		[]byte(`{}`),              // missing issue
		[]byte(`{"issue": null}`), // null issue
		[]byte(`{"issue": {}}`),   // missing key + status
		[]byte(`{"issue": {"key": "SEC-1", "fields": {}}}`),                       // missing status
		[]byte(`{"issue": {"key": "", "fields": {"status": {"name": "Foo"}}}}`),   // empty key
		[]byte(`{"issue": {"key": "SEC-1", "fields": {"status": {"name": ""}}}}`), // empty status
		[]byte(`{not json`),    // malformed
		[]byte(``),             // empty
		[]byte(`null`),         // top-level null
		[]byte(`{"issue":42}`), // non-object issue
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := jsm.Decode(body)
		switch {
		case err == nil && got == nil:
			t.Errorf("Decode returned nil, nil for body=%q", body)
		case err != nil && got != nil:
			t.Errorf("Decode returned non-nil result with error: got=%+v err=%v", got, err)
		case err != nil:
			// Every error should match one of the documented sentinels.
			if !errors.Is(err, jsm.ErrInvalidJSON) &&
				!errors.Is(err, jsm.ErrMissingIssue) &&
				!errors.Is(err, jsm.ErrMissingIssueKey) &&
				!errors.Is(err, jsm.ErrMissingStatus) {
				t.Errorf("Decode err = %v, expected wrap of a documented sentinel", err)
			}
		}
	})
}
