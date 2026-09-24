package auth

import (
	"encoding/base64"
	"testing"
)

// TestNewSessionCodecSubstitutesWeakKeys (SU-6): a hash key shorter than 32 bytes
// (previously only an EMPTY key was caught) is substituted with a random key and
// flagged ephemeral, instead of being passed to HMAC as a weak short key. A valid
// 32-byte hash + block pair is used as-is.
func TestNewSessionCodecSubstitutesWeakKeys(t *testing.T) {
	valid := make([]byte, 32)

	if _, ephemeral := newSessionCodec(make([]byte, 16), valid, false); !ephemeral {
		t.Errorf("a 16-byte hash key should be substituted (ephemeral), was accepted")
	}
	if _, ephemeral := newSessionCodec(nil, valid, false); !ephemeral {
		t.Errorf("an empty hash key should be substituted (ephemeral)")
	}
	if _, ephemeral := newSessionCodec(valid, make([]byte, 20), false); !ephemeral {
		t.Errorf("an invalid-size block key should be substituted (ephemeral)")
	}
	if _, ephemeral := newSessionCodec(valid, valid, false); ephemeral {
		t.Errorf("a valid 32-byte hash+block pair must NOT be ephemeral")
	}
}

// TestDecodeKeyRejectsRawInProd (SU-6): a non-base64 session key is tolerated as
// raw bytes only in dev (allowRaw=true); in a production auth mode it is rejected
// (nil) so a typo'd passphrase can't silently become a short raw key. Valid base64
// decodes on both paths.
func TestDecodeKeyRejectsRawInProd(t *testing.T) {
	raw := "this-is-not-base64!!!"

	if got := decodeKey(raw, false); got != nil {
		t.Errorf("non-base64 key in prod mode must be rejected (nil), got %v", got)
	}
	if got := decodeKey(raw, true); string(got) != raw {
		t.Errorf("non-base64 key in dev mode should pass through raw, got %q", got)
	}

	valid := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if got := decodeKey(valid, false); len(got) != 32 {
		t.Errorf("valid base64 key must decode to 32 bytes in prod, got %d", len(got))
	}
	if got := decodeKey(valid, true); len(got) != 32 {
		t.Errorf("valid base64 key must decode to 32 bytes in dev, got %d", len(got))
	}
	if got := decodeKey("", false); got != nil {
		t.Errorf("empty key must decode to nil")
	}
}
