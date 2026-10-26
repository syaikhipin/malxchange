//go:build integration

// Sensitive-flow matrix over the real seam (ticket #39, ADR-0006):
// password change, forgot password, email change, phone bind,
// Authenticator bind/unbind, KYC submit/review, member lock, and
// admin-mediated Authenticator recovery — all through the served HTTP
// surface (member REST + the internal admin RPC), with the capture
// mailer and stub channel providers standing in for delivery.
package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/syaikhipin/malxchange/internal/auth"
)

var secCodeRe = regexp.MustCompile(`\b(\d{6})\b`)

// lastCodeFor returns the newest 6-digit code mailed to an address.
func (s *itStack) lastCodeFor(email string) string {
	s.t.Helper()
	mail := s.mailer.Mail()
	for i := len(mail) - 1; i >= 0; i-- {
		if mail[i].To == email {
			if m := secCodeRe.FindStringSubmatch(mail[i].Body); m != nil {
				return m[1]
			}
		}
	}
	s.t.Fatalf("no code mailed to %s", email)
	return ""
}

func (s *itStack) security(token string) map[string]any {
	s.t.Helper()
	status, body := s.get("/api/uc/security", token)
	if status != http.StatusOK {
		s.t.Fatalf("security: status %d body %v", status, body)
	}
	return body
}

// register2 is register plus the member id (admin tests address members
// by id).
func (s *itStack) register2(email, password string) (token, memberID string) {
	s.t.Helper()
	token = s.register(email, password)
	status, body := s.get("/api/uc/me", token)
	if status != http.StatusOK {
		s.t.Fatalf("me: status %d body %v", status, body)
	}
	id, _ := body["id"].(string)
	return token, id
}

// bindAuthenticator drives the full bind flow (email code → secret →
// live code) the way the member's phone would.
func (s *itStack) bindAuthenticator(token, email string) string {
	s.t.Helper()
	if status, body := s.post("/api/uc/authenticator/code", nil, token); status != http.StatusOK {
		s.t.Fatalf("authenticator code: status %d body %v", status, body)
	}
	code := s.lastCodeFor(email)
	status, body := s.post("/api/uc/authenticator/bind/begin", map[string]string{"code": code}, token)
	if status != http.StatusOK {
		s.t.Fatalf("authenticator begin: status %d body %v", status, body)
	}
	secret, _ := body["secret"].(string)
	if secret == "" {
		s.t.Fatalf("authenticator begin returned no secret: %v", body)
	}
	live, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		s.t.Fatal(err)
	}
	if status, body := s.post("/api/uc/authenticator/bind/confirm", map[string]string{"code": live}, token); status != http.StatusOK {
		s.t.Fatalf("authenticator confirm: status %d body %v", status, body)
	}
	return secret
}

func TestPasswordChangeRevokesAllSessions(t *testing.T) {
	s := newStack(t)
	email := "pwchange@example.com"
	token := s.register(email, "old-password-1")

	if status, body := s.post("/api/uc/password/change/code", nil, token); status != http.StatusOK {
		t.Fatalf("change code: status %d body %v", status, body)
	}
	code := s.lastCodeFor(email)

	// Wrong old password refuses before any code is consumed.
	if status, _ := s.post("/api/uc/password/change", map[string]string{
		"oldPassword": "not-the-password", "code": code, "newPassword": "new-password-2"}, token); status != http.StatusUnauthorized {
		t.Fatalf("wrong old password: status %d", status)
	}
	if status, body := s.post("/api/uc/password/change", map[string]string{
		"oldPassword": "old-password-1", "code": code, "newPassword": "new-password-2"}, token); status != http.StatusOK {
		t.Fatalf("change: status %d body %v", status, body)
	}
	// Every session died with the password event.
	if status, _ := s.get("/api/uc/me", token); status != http.StatusUnauthorized {
		t.Fatalf("old token after password change: %d", status)
	}
	if status, _ := s.post("/api/uc/login", map[string]string{
		"email": email, "password": "old-password-1"}, ""); status != http.StatusUnauthorized {
		t.Fatalf("login with old password: %d", status)
	}
	if status, _ := s.post("/api/uc/login", map[string]string{
		"email": email, "password": "new-password-2"}, ""); status != http.StatusOK {
		t.Fatalf("login with new password: %d", status)
	}
}

func TestForgotPasswordResetRevokesSessions(t *testing.T) {
	s := newStack(t)
	email := "forgot@example.com"
	token := s.register(email, "first-password")

	// Unknown addresses answer identically (no account oracle).
	if status, body := s.post("/api/uc/password/forgot/code", map[string]string{"email": "ghost@example.com"}, ""); status != http.StatusOK {
		t.Fatalf("forgot code unknown email: status %d body %v", status, body)
	}
	if status, _ := s.post("/api/uc/password/forgot/code", map[string]string{"email": email}, ""); status != http.StatusOK {
		t.Fatalf("forgot code: status %d", status)
	}
	code := s.lastCodeFor(email)
	if status, body := s.post("/api/uc/password/forgot/reset", map[string]string{
		"email": email, "code": code, "newPassword": "second-password"}, ""); status != http.StatusOK {
		t.Fatalf("forgot reset: status %d body %v", status, body)
	}
	if status, _ := s.get("/api/uc/me", token); status != http.StatusUnauthorized {
		t.Fatalf("old token after reset: %d", status)
	}
}

func TestEmailChangeNeedsBothCodes(t *testing.T) {
	s := newStack(t)
	oldEmail, newEmail := "old@example.com", "new@example.com"
	token := s.register(oldEmail, "the-password")

	if status, body := s.post("/api/uc/email/change/request", map[string]string{
		"password": "the-password", "newEmail": newEmail}, token); status != http.StatusOK {
		t.Fatalf("email change request: status %d body %v", status, body)
	}
	oldCode := s.lastCodeFor(oldEmail)
	newCode := s.lastCodeFor(newEmail)

	// A wrong new-address code refuses.
	if status, _ := s.post("/api/uc/email/change/confirm", map[string]string{
		"newEmail": newEmail, "oldCode": oldCode, "newCode": "000000"}, token); status != http.StatusBadRequest {
		t.Fatalf("wrong new code: status %d", status)
	}
	// That attempt consumed the old-address code (single-use), so the
	// member restarts the request for a fresh pair.
	time.Sleep(300 * time.Millisecond)
	if status, body := s.post("/api/uc/email/change/request", map[string]string{
		"password": "the-password", "newEmail": newEmail}, token); status != http.StatusOK {
		t.Fatalf("email change re-request: status %d body %v", status, body)
	}
	oldCode = s.lastCodeFor(oldEmail)
	newCode = s.lastCodeFor(newEmail)
	if status, body := s.post("/api/uc/email/change/confirm", map[string]string{
		"newEmail": newEmail, "oldCode": oldCode, "newCode": newCode}, token); status != http.StatusOK {
		t.Fatalf("email change confirm: status %d body %v", status, body)
	}
	if status, body := s.get("/api/uc/me", token); status != http.StatusOK || body["email"] != newEmail {
		t.Fatalf("me after email change: status %d body %v", status, body)
	}
	if status, _ := s.post("/api/uc/login", map[string]string{
		"email": newEmail, "password": "the-password"}, ""); status != http.StatusOK {
		t.Fatalf("login with new email: %d", status)
	}
}

func TestAuthenticatorBindUnbindAndGate(t *testing.T) {
	s := newStack(t)
	email := "totp@example.com"
	token := s.register(email, "the-password")

	if got := s.security(token)["authenticatorBound"]; got != false {
		t.Fatalf("authenticatorBound before bind = %v", got)
	}
	secret := s.bindAuthenticator(token, email)
	if got := s.security(token)["authenticatorBound"]; got != true {
		t.Fatalf("authenticatorBound after bind = %v", got)
	}
	// Unbind demands password + live code; a wrong code refuses.
	if status, _ := s.post("/api/uc/authenticator/unbind", map[string]string{
		"password": "the-password", "code": "000000"}, token); status != http.StatusBadRequest {
		t.Fatalf("unbind wrong code: status %d", status)
	}
	live, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status, body := s.post("/api/uc/authenticator/unbind", map[string]string{
		"password": "the-password", "code": live}, token); status != http.StatusOK {
		t.Fatalf("unbind: status %d body %v", status, body)
	}
	if got := s.security(token)["authenticatorBound"]; got != false {
		t.Fatalf("authenticatorBound after unbind = %v", got)
	}
}

// TestWithdrawalRejectsReplayedTOTPCode pins the consuming withdrawal
// gate (audit issue #103): the first verification of a code passes and
// records the step; replaying the SAME code — still inside the +/-1
// -step window — finds the step consumed and is refused. The gated
// withdrawal endpoint is only wired with the stub gate in the funds IT
// harness, so the production securityGate is exercised directly over
// the real api schema and binding row.
// TestWithdrawalGateTOTPReplayProtection pins the member gate's
// monotonic consumption (#103): the same code verifies again while its
// step is current, and a code from an older step than any already used
// is refused. The window advance is simulated deterministically by
// bumping the marker past the used step (no sleeps).
func TestWithdrawalGateTOTPReplayProtection(t *testing.T) {
	s := newStack(t)
	email := "withdraw-gate@example.com"
	token, memberID := s.register2(email, "the-password")
	secret := s.bindAuthenticator(token, email)

	gate := securityGate{pool: s.pool}
	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	ok, err := gate.VerifyAuthenticatorCode(context.Background(), memberID, code)
	if err != nil || !ok {
		t.Fatalf("first verification: ok=%v err=%v, want true/nil", ok, err)
	}

	// Same step, same code: a second legitimate action — allowed.
	ok, err = gate.VerifyAuthenticatorCode(context.Background(), memberID, code)
	if err != nil || !ok {
		t.Fatalf("same-step second verification: ok=%v err=%v, want true/nil", ok, err)
	}

	// The window slides past the used step: the same code is a replay
	// of an older step and must be refused.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE authenticator_bindings SET last_used_step = last_used_step + 1 WHERE member_id = $1`, memberID); err != nil {
		t.Fatal(err)
	}
	ok, err = gate.VerifyAuthenticatorCode(context.Background(), memberID, code)
	if err != nil || ok {
		t.Fatalf("older-step replayed verification: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestKYCReviewAndMemberLock(t *testing.T) {
	s := newStack(t)
	email := "kyc@example.com"
	token, memberID := s.register2(email, "the-password")

	if status, body := s.get("/api/uc/kyc", token); status != http.StatusOK || body["status"] != "unverified" {
		t.Fatalf("kyc before submit: status %d body %v", status, body)
	}
	img := base64.StdEncoding.EncodeToString([]byte("fake-image-bytes"))
	if status, body := s.post("/api/uc/kyc", map[string]string{
		"fullName": "Kyc Member", "documentNumber": "X1234567",
		"frontImage": img, "backImage": img}, token); status != http.StatusOK {
		t.Fatalf("kyc submit: status %d body %v", status, body)
	}
	if status, body := s.get("/api/uc/kyc", token); status != http.StatusOK || body["status"] != "pending" {
		t.Fatalf("kyc after submit: status %d body %v", status, body)
	}
	// Admin view + review through the internal RPC.
	if status, raw := s.adminDo(http.MethodGet, "/internal/members/"+memberID, nil); status != http.StatusOK {
		t.Fatalf("admin member view: status %d body %s", status, raw)
	}
	if status, raw := s.adminDo(http.MethodPost, "/internal/members/"+memberID+"/kyc/review",
		map[string]any{"approved": true, "actor": "support-1"}); status != http.StatusOK {
		t.Fatalf("kyc review: status %d body %s", status, raw)
	}
	if got := s.security(token)["kycStatus"]; got != "verified" {
		t.Fatalf("kycStatus after approve = %v", got)
	}
	// Lock kills the session and the login; unlock restores login.
	if status, raw := s.adminDo(http.MethodPost, "/internal/members/"+memberID+"/lock", nil); status != http.StatusOK {
		t.Fatalf("lock: status %d body %s", status, raw)
	}
	if status, _ := s.get("/api/uc/me", token); status != http.StatusUnauthorized {
		t.Fatalf("token after lock: %d", status)
	}
	if status, _ := s.post("/api/uc/login", map[string]string{
		"email": email, "password": "the-password"}, ""); status != http.StatusForbidden {
		t.Fatalf("login while locked: %d", status)
	}
	if status, raw := s.adminDo(http.MethodPost, "/internal/members/"+memberID+"/unlock", nil); status != http.StatusOK {
		t.Fatalf("unlock: status %d body %s", status, raw)
	}
	if status, _ := s.post("/api/uc/login", map[string]string{
		"email": email, "password": "the-password"}, ""); status != http.StatusOK {
		t.Fatalf("login after unlock: %d", status)
	}
}

func TestAuthenticatorResetRequiresKYCAndAudits(t *testing.T) {
	s := newStack(t)
	email := "reset@example.com"
	token, memberID := s.register2(email, "the-password")
	s.bindAuthenticator(token, email)

	// Not KYC-verified: recovery is refused.
	if status, raw := s.adminDo(http.MethodPost, "/internal/members/"+memberID+"/authenticator/reset",
		map[string]any{"actor": "support-1"}); status != http.StatusConflict {
		t.Fatalf("reset without kyc: status %d body %s", status, raw)
	}
	// Verify KYC, then the reset lands, is audit-logged, and notifies.
	img := base64.StdEncoding.EncodeToString([]byte("img"))
	if status, body := s.post("/api/uc/kyc", map[string]string{
		"fullName": "Reset Member", "documentNumber": "Y1",
		"frontImage": img, "backImage": img}, token); status != http.StatusOK {
		t.Fatalf("kyc submit: status %d body %v", status, body)
	}
	if status, raw := s.adminDo(http.MethodPost, "/internal/members/"+memberID+"/kyc/review",
		map[string]any{"approved": true, "actor": "support-1"}); status != http.StatusOK {
		t.Fatalf("kyc review: status %d body %s", status, raw)
	}
	if status, raw := s.adminDo(http.MethodPost, "/internal/members/"+memberID+"/authenticator/reset",
		map[string]any{"actor": "support-1"}); status != http.StatusOK {
		t.Fatalf("reset: status %d body %s", status, raw)
	}
	if got := s.security(token)["authenticatorBound"]; got != false {
		t.Fatalf("authenticatorBound after reset = %v", got)
	}
	var audits int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM security_audit WHERE member_id = $1 AND action = 'authenticator_reset' AND actor = 'support-1'`,
		memberID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("security_audit rows = %d, want 1", audits)
	}
	// The member was notified by email (capture mailer).
	found := false
	for _, m := range s.mailer.Mail() {
		if m.To == email && m.Subject == "Your authenticator was reset" {
			found = true
		}
	}
	if !found {
		t.Fatal("no authenticator-reset notification email captured")
	}
}

func TestPhoneBindCompletesWithPasswordAndCode(t *testing.T) {
	s := newStack(t)
	email := "phone@example.com"
	token := s.register(email, "the-password")

	sms := newSMSStub(t)
	if status, raw := s.adminDo(http.MethodPost, "/internal/channels/sms/activate",
		map[string]any{"credentials": map[string]string{"endpoint": sms.URL, "apiKey": "test-key"}}); status != http.StatusNoContent {
		t.Fatalf("activate sms: status %d body %s", status, raw)
	}
	if status, body := s.post("/api/uc/phone/code", map[string]string{"phone": "+15551234567"}, token); status != http.StatusOK {
		t.Fatalf("phone code: status %d body %v", status, body)
	}
	hits := sms.recorded()
	if len(hits) != 1 {
		t.Fatalf("sms stub hits = %d, want 1", len(hits))
	}
	msg, _ := hits[0].Body["message"].(string)
	m := secCodeRe.FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("sms message carries no code: %q", msg)
	}
	// Wrong password refuses; password + code binds.
	if status, _ := s.post("/api/uc/phone/bind", map[string]string{
		"phone": "+15551234567", "password": "wrong", "code": m[1]}, token); status != http.StatusUnauthorized {
		t.Fatalf("bind wrong password: status %d", status)
	}
	if status, body := s.post("/api/uc/phone/bind", map[string]string{
		"phone": "+15551234567", "password": "the-password", "code": m[1]}, token); status != http.StatusOK {
		t.Fatalf("phone bind: status %d body %v", status, body)
	}
	if got := s.security(token)["phone"]; got != "+15551234567" {
		t.Fatalf("security phone = %v", got)
	}
}
