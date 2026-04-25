// Package webhook implements webhookd's webhook intake: signature and
// timestamp verification, the per-provider HTTP handler, and the
// generic envelope parse. The package is the trust boundary for
// inbound webhook traffic — every line here is in the path of an
// untrusted caller.
//
// Signature verification uses HMAC-SHA256 over a canonical message of
// shape `v0:<timestamp>:<body>`. The `v0:` prefix borrows from Slack's
// scheme so we can rev the canonical format later (`v1:`, …) without
// breaking signers.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors for every documented failure mode. Callers compare
// with errors.Is so the handler can map each class to the right metric
// label and HTTP status without scraping error strings.
var (
	ErrMalformed          = errors.New("malformed signature header")
	ErrInvalidSignature   = errors.New("invalid signature")
	ErrTimestampMissing   = errors.New("missing timestamp header")
	ErrTimestampMalformed = errors.New("malformed timestamp header")
	ErrTimestampSkewed    = errors.New("timestamp outside accepted skew window")
)

// signaturePrefix is the only currently-supported signature algorithm
// prefix. Bumping this constant lets us rev the verification scheme
// without breaking signers that pin to "sha256=".
const signaturePrefix = "sha256="

// canonicalVersion namespaces the signed-message format. Slack-style
// versioning means a future v1 can change message shape (e.g., add the
// route) without invalidating existing v0 signers.
const canonicalVersion = "v0"

// VerifyHMAC validates that received is a `sha256=<hex>` signature of
// canonical computed with secret. A timing-safe comparison is used so
// callers cannot infer secret bytes from response timing.
func VerifyHMAC(secret, canonical []byte, received string) error {
	hexSig, ok := strings.CutPrefix(received, signaturePrefix)
	if !ok || hexSig == "" {
		return fmt.Errorf("%w: missing %q prefix",
			ErrMalformed, signaturePrefix)
	}
	want, err := hex.DecodeString(hexSig)
	if err != nil {
		return fmt.Errorf("%w: hex decode: %w", ErrMalformed, err)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(canonical)
	got := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		return ErrInvalidSignature
	}
	return nil
}

// VerifyTimestamp parses headerVal as Unix seconds and rejects any value
// outside [now-skew, now+skew]. An empty header is rejected with
// ErrTimestampMissing — handlers must run VerifyTimestamp before
// VerifyHMAC so the operator can distinguish replay from corruption in
// metrics.
func VerifyTimestamp(headerVal string, now time.Time, skew time.Duration) error {
	if headerVal == "" {
		return ErrTimestampMissing
	}
	secs, err := strconv.ParseInt(headerVal, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrTimestampMalformed, err)
	}
	ts := time.Unix(secs, 0)
	delta := now.Sub(ts)
	if delta < 0 {
		delta = -delta
	}
	if delta > skew {
		return fmt.Errorf("%w: |now-ts|=%s, skew=%s",
			ErrTimestampSkewed, delta, skew)
	}
	return nil
}

// Verify composes timestamp and signature checks against the canonical
// `v0:<timestamp>:<body>` message. We check the timestamp first because
// a bad timestamp is cheaper to detect than a full HMAC round and lets
// us record the right metric (`result="missing"|"replayed"`).
func Verify(
	secret []byte,
	sigHeader, tsHeader string,
	body []byte,
	now time.Time,
	skew time.Duration,
) error {
	if err := VerifyTimestamp(tsHeader, now, skew); err != nil {
		return err
	}
	canonical := canonicalMessage(tsHeader, body)
	return VerifyHMAC(secret, canonical, sigHeader)
}

// canonicalMessage builds the bytes that signers must HMAC. Pulled out
// so future provider implementations and the fuzz target stay in
// lockstep on format.
func canonicalMessage(tsHeader string, body []byte) []byte {
	prefix := canonicalVersion + ":" + tsHeader + ":"
	out := make([]byte, 0, len(prefix)+len(body))
	out = append(out, prefix...)
	out = append(out, body...)
	return out
}
