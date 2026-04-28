// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package jsm_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/donaldgifford/webhookd/internal/webhook"
	"github.com/donaldgifford/webhookd/internal/webhook/jsm"
)

// signRequest computes a v0:<ts>:<body> HMAC and returns the matching
// (sigHeaderValue, tsHeaderValue) pair. Mirrors the contract enforced
// by webhook.Verify; copied here rather than imported so the test
// fails loudly if the canonical format ever changes.
func signRequest(t *testing.T, secret []byte, ts time.Time, body []byte) (sig, tsHeader string) {
	t.Helper()
	tsHeader = strconv.FormatInt(ts.Unix(), 10)
	canonical := []byte("v0:" + tsHeader + ":")
	canonical = append(canonical, body...)
	mac := hmac.New(sha256.New, secret)
	mac.Write(canonical)
	sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return sig, tsHeader
}

func TestVerifySignature_TableDriven(t *testing.T) {
	t.Parallel()

	const sigHeader = "X-Webhook-Signature"
	const tsHeader = "X-Webhook-Timestamp"
	secret := []byte("topsecret")
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"issue":{"key":"SEC-1"}}`)

	tests := []struct {
		name         string
		mutate       func(req *httptest.ResponseRecorder, sig, ts string) (newSig, newTS string)
		bodyOverride []byte
		wantErr      error
	}{
		{
			name: "valid",
		},
		{
			name: "wrong_secret",
			mutate: func(_ *httptest.ResponseRecorder, _, ts string) (string, string) {
				canonical := []byte("v0:" + ts + ":")
				canonical = append(canonical, body...)
				mac := hmac.New(sha256.New, []byte("wrongsecret"))
				mac.Write(canonical)
				return "sha256=" + hex.EncodeToString(mac.Sum(nil)), ts
			},
			wantErr: webhook.ErrInvalidSignature,
		},
		{
			name: "missing_timestamp",
			mutate: func(_ *httptest.ResponseRecorder, sig, _ string) (string, string) {
				return sig, ""
			},
			wantErr: webhook.ErrTimestampMissing,
		},
		{
			name: "replayed_outside_skew",
			mutate: func(_ *httptest.ResponseRecorder, _, _ string) (string, string) {
				old := now.Add(-2 * time.Hour)
				sig, ts := signRequest(t, secret, old, body)
				return sig, ts
			},
			wantErr: webhook.ErrTimestampSkewed,
		},
		{
			name: "malformed_signature_prefix",
			mutate: func(_ *httptest.ResponseRecorder, sig, ts string) (string, string) {
				return "md5=" + sig, ts
			},
			wantErr: webhook.ErrMalformed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			useBody := body
			if tc.bodyOverride != nil {
				useBody = tc.bodyOverride
			}
			sig, ts := signRequest(t, secret, now, useBody)
			if tc.mutate != nil {
				sig, ts = tc.mutate(nil, sig, ts)
			}
			req := httptest.NewRequestWithContext(t.Context(), "POST", "/webhook/jsm", http.NoBody)
			req.Header.Set(sigHeader, sig)
			if ts != "" {
				req.Header.Set(tsHeader, ts)
			}
			err := jsm.VerifySignature(req, useBody, jsm.SignatureConfig{
				SecretBytes: secret,
				SigHeader:   sigHeader,
				TSHeader:    tsHeader,
				Skew:        5 * time.Minute,
				Now:         func() time.Time { return now },
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("VerifySignature err = %v, want errors.Is(%v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifySignature err = %v, want nil", err)
			}
		})
	}
}
