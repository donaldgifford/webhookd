// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/donaldgifford/webhookd/internal/webhook"
)

// signFixture deterministically signs body with secret at ts. We
// compute the canonical bytes here rather than calling an exported
// helper, so the tests pin the format from the outside.
func signFixture(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMAC_TableDriven(t *testing.T) {
	secret := []byte("topsecret")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"event":"test"}`)
	good := signFixture(secret, ts, body)
	canonical := []byte("v0:" + ts + ":" + `{"event":"test"}`)

	tests := []struct {
		name      string
		secret    []byte
		canonical []byte
		received  string
		want      error
	}{
		{"valid", secret, canonical, good, nil},
		{"wrong secret", []byte("other"), canonical, good, webhook.ErrInvalidSignature},
		{
			"tampered body",
			secret,
			[]byte("v0:" + ts + ":" + `{"event":"hax"}`),
			good,
			webhook.ErrInvalidSignature,
		},
		{"missing prefix", secret, canonical, "deadbeef", webhook.ErrMalformed},
		{"wrong algo", secret, canonical, "md5=deadbeef", webhook.ErrMalformed},
		{"empty after prefix", secret, canonical, "sha256=", webhook.ErrMalformed},
		{"non-hex payload", secret, canonical, "sha256=zzzz", webhook.ErrMalformed},
		{"empty header", secret, canonical, "", webhook.ErrMalformed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := webhook.VerifyHMAC(tt.secret, tt.canonical, tt.received)
			if !errors.Is(got, tt.want) {
				t.Errorf("VerifyHMAC = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVerifyTimestamp_TableDriven(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	skew := 5 * time.Minute

	tests := []struct {
		name string
		val  string
		want error
	}{
		{"in-window past", strconv.FormatInt(now.Add(-2*time.Minute).Unix(), 10), nil},
		{"in-window future", strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10), nil},
		{"exactly at skew", strconv.FormatInt(now.Add(-skew).Unix(), 10), nil},
		{"beyond skew past", strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), webhook.ErrTimestampSkewed},
		{"beyond skew future", strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10), webhook.ErrTimestampSkewed},
		{"missing", "", webhook.ErrTimestampMissing},
		{"malformed", "not-a-number", webhook.ErrTimestampMalformed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := webhook.VerifyTimestamp(tt.val, now, skew)
			if !errors.Is(got, tt.want) {
				t.Errorf("VerifyTimestamp = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVerify_Composes covers the full Verify path: a valid timestamp +
// valid signature must succeed; a missing timestamp must short-circuit
// before any HMAC work; a bad signature with a good timestamp must
// surface as ErrInvalidSignature.
func TestVerify_Composes(t *testing.T) {
	secret := []byte("topsecret")
	now := time.Unix(1_700_000_000, 0)
	skew := 5 * time.Minute
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte("payload")
	good := signFixture(secret, ts, body)

	tests := []struct {
		name string
		ts   string
		sig  string
		want error
	}{
		{"valid", ts, good, nil},
		{"missing timestamp short-circuits", "", good, webhook.ErrTimestampMissing},
		{"replay outside skew", strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), good, webhook.ErrTimestampSkewed},
		{"bad signature", ts, "sha256=" + hex.EncodeToString(make([]byte, 32)), webhook.ErrInvalidSignature},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := webhook.Verify(secret, tt.sig, tt.ts, body, now, skew)
			if !errors.Is(got, tt.want) {
				t.Errorf("Verify = %v, want %v", got, tt.want)
			}
		})
	}
}

// FuzzSignatureVerify drives VerifyHMAC with arbitrary received-header
// strings against a fixed secret/canonical pair. The function must
// never panic; every input either matches the precomputed signature
// (legitimate corpus) or returns one of the typed errors.
func FuzzSignatureVerify(f *testing.F) {
	secret := []byte("topsecret")
	canonical := []byte("v0:1700000000:hello")

	mac := hmac.New(sha256.New, secret)
	mac.Write(canonical)
	wantMACSeed := mac.Sum(nil)
	good := "sha256=" + hex.EncodeToString(wantMACSeed)

	seeds := []string{
		good,
		"",
		"sha256=",
		"sha256=zz",
		"md5=deadbeef",
		"sha256=" + hex.EncodeToString([]byte("not-a-real-sig")),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, received string) {
		err := webhook.VerifyHMAC(secret, canonical, received)
		if err == nil {
			// nil is acceptable iff received decodes (case-insensitively)
			// to the canonical MAC — hex.DecodeString accepts both cases.
			hexSig, ok := strings.CutPrefix(received, "sha256=")
			if !ok {
				t.Errorf("nil err with bad prefix: %q", received)
				return
			}
			gotBytes, decErr := hex.DecodeString(hexSig)
			if decErr != nil || !bytes.Equal(gotBytes, wantMACSeed) {
				t.Errorf("nil err but signature does not match: %q", received)
			}
			return
		}
		if !errors.Is(err, webhook.ErrMalformed) &&
			!errors.Is(err, webhook.ErrInvalidSignature) {
			t.Errorf("unexpected error type: %v", err)
		}
	})
}
