package auth

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors (SHA-1, secret "12345678901234567890"
// in base32 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ).
func TestTOTPCodeMatchesRFC6238Vectors(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1234567890, "005924"},
	}
	for _, tc := range cases {
		got, err := TOTPCode(secret, time.Unix(tc.unix, 0).UTC())
		if err != nil {
			t.Fatalf("TOTPCode(%d): %v", tc.unix, err)
		}
		if got != tc.want {
			t.Errorf("TOTPCode(%d) = %s, want %s", tc.unix, got, tc.want)
		}
	}
}

func TestVerifyTOTPAcceptsNeighboringSteps(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	now := time.Unix(1234567890, 0).UTC()
	code, err := TOTPCode(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyTOTP(secret, code, now) {
		t.Fatal("current step rejected")
	}
	if !VerifyTOTP(secret, code, now.Add(20*time.Second)) {
		t.Fatal("same-step later second rejected")
	}
	if VerifyTOTP(secret, "000000", now) && code != "000000" {
		t.Fatal("wrong code accepted")
	}
	if VerifyTOTP(secret, "12345", now) {
		t.Fatal("short code accepted")
	}
}

// stepOf is the TOTP step counter of t (30-second step).
func stepOf(t time.Time) uint64 { return uint64(t.Unix()) / 30 }

func TestVerifyTOTPStepReturnsMatchedStep(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // gitleaks:allow - published RFC 6238 Appendix B vector, not a credential
	now := time.Unix(1234567890, 0).UTC()

	// The previous step's code matches through the -1 window slot and
	// reports THAT slot's counter — the value consumption persists.
	prev, err := TOTPCode(secret, now.Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	step, ok := VerifyTOTPStep(secret, prev, now)
	if !ok || step != stepOf(now)-1 {
		t.Fatalf("previous-step code: got (%d, %v), want (%d, true)", step, ok, stepOf(now)-1)
	}

	cur, err := TOTPCode(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	step, ok = VerifyTOTPStep(secret, cur, now)
	if !ok || step != stepOf(now) {
		t.Fatalf("current-step code: got (%d, %v), want (%d, true)", step, ok, stepOf(now))
	}

	if _, ok := VerifyTOTPStep(secret, "000000", now); ok {
		t.Fatal("garbage 6-char code accepted")
	}
}

func TestGenerateTOTPSecretIsBase32(t *testing.T) {
	a, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated secrets are identical")
	}
	if _, err := TOTPCode(a, time.Now()); err != nil {
		t.Fatalf("generated secret undecodable: %v", err)
	}
}
