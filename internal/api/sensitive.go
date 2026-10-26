// The sensitive-flow matrix (ticket #39, ADR-0006 — binding):
//
//	password change   = old password + email code, sessions revoked
//	forgot password   = email code, sessions revoked
//	email change      = login password + codes to BOTH addresses
//	phone bind        = login password + code to the new phone
//	Authenticator bind   = session + email code (then a live code proves possession)
//	Authenticator unbind = login password + live Authenticator code
//	Authenticator recovery = admin-mediated, KYC-verified only,
//	                         audit-logged, all active channels notified
//
// KYC submission/review and the admin member RPCs (view/lock/review/
// reset) live here too — admin-api (#25) fronts them with Employee RBAC.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/syaikhipin/malxchange/internal/auth"
)

// Code purposes for the sensitive flows. Codes only ever validate
// against the purpose they were issued for (codes.go).
const (
	purposePasswordChange    = "password-change"
	purposePasswordReset     = "password-reset"
	purposeEmailChangeOld    = "email-change-old"
	purposeEmailChangeNew    = "email-change-new"
	purposeAuthenticatorBind = "authenticator-bind"
)

// RegisterSensitive mounts the member security routes on the public mux.
func (a *memberAPI) RegisterSensitive(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/uc/password/change/code", a.withSession(a.handlePasswordChangeCode))
	mux.HandleFunc("POST /api/uc/password/change", a.withSession(a.handlePasswordChange))
	mux.HandleFunc("POST /api/uc/password/forgot/code", a.handleForgotCode)
	mux.HandleFunc("POST /api/uc/password/forgot/reset", a.handleForgotReset)
	mux.HandleFunc("POST /api/uc/email/change/request", a.withSession(a.handleEmailChangeRequest))
	mux.HandleFunc("POST /api/uc/email/change/confirm", a.withSession(a.handleEmailChangeConfirm))
	mux.HandleFunc("POST /api/uc/phone/bind", a.withSession(a.handlePhoneBind))
	mux.HandleFunc("POST /api/uc/authenticator/code", a.withSession(a.handleAuthenticatorCode))
	mux.HandleFunc("POST /api/uc/authenticator/bind/begin", a.withSession(a.handleAuthenticatorBegin))
	mux.HandleFunc("POST /api/uc/authenticator/bind/confirm", a.withSession(a.handleAuthenticatorConfirm))
	mux.HandleFunc("POST /api/uc/authenticator/unbind", a.withSession(a.handleAuthenticatorUnbind))
	mux.HandleFunc("GET /api/uc/security", a.withSession(a.handleSecurity))
	mux.HandleFunc("POST /api/uc/kyc", a.withSession(a.handleKYCSubmit))
	mux.HandleFunc("GET /api/uc/kyc", a.withSession(a.handleKYCState))
}

// RegisterSensitiveAdmin mounts the internal admin RPCs admin-api calls
// (member view/lock, KYC review, Authenticator reset) plus the search
// and KYC-queue reads (#58, member_admin.go). Registered without the
// /internal prefix — the boot harness strips it.
func (a *memberAPI) RegisterSensitiveAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /members", a.handleAdminMemberSearch)
	mux.HandleFunc("GET /members/kyc/pending", a.handleAdminKYCPending)
	mux.HandleFunc("GET /members/{id}", a.handleAdminMemberView)
	mux.HandleFunc("GET /members/{id}/kyc/evidence", a.handleAdminKYCEvidence)
	mux.HandleFunc("POST /members/{id}/lock", a.handleAdminMemberLock)
	mux.HandleFunc("POST /members/{id}/unlock", a.handleAdminMemberUnlock)
	mux.HandleFunc("POST /members/{id}/kyc/review", a.handleAdminKYCReview)
	mux.HandleFunc("POST /members/{id}/authenticator/reset", a.handleAdminAuthenticatorReset)
}

// ---- member record helpers ----

type memberRecord struct {
	ID           string
	Email        string
	Status       string
	Phone        string
	PasswordHash string
	CreatedAt    time.Time
}

func (a *memberAPI) loadMember(ctx context.Context, id string) (memberRecord, error) {
	var m memberRecord
	err := a.pool.QueryRow(ctx,
		`SELECT id, email, status, COALESCE(phone, ''), password_hash, created_at
		   FROM members WHERE id = $1`, id).
		Scan(&m.ID, &m.Email, &m.Status, &m.Phone, &m.PasswordHash, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, errMemberNotFound
	}
	if err != nil {
		return m, fmt.Errorf("api: load member: %w", err)
	}
	return m, nil
}

var errMemberNotFound = errors.New("member not found")

// sendEmailCode issues a purpose-scoped code to the member's email and
// delivers it, honoring the per-target cooldown. tooSoon means the
// caller should answer 429 without having sent anything new.
func (a *memberAPI) sendEmailCode(ctx context.Context, email, purpose, action string) (tooSoon bool, err error) {
	var code string
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		recent, err := recentCode(ctx, tx, purpose, email, a.env.CodeCooldown)
		if err != nil {
			return err
		}
		tooSoon = recent
		if recent {
			return nil
		}
		code, err = issueCode(ctx, tx, purpose, email, a.env.CodeTTL)
		return err
	})
	if err != nil || tooSoon {
		return tooSoon, err
	}
	subject, body, err := SecurityCodeEmail(code, a.env.CodeTTL, action)
	if err != nil {
		return false, err
	}
	if err := a.mailer.Send(ctx, email, subject, body); err != nil {
		a.log.Error("security code email failed", "purpose", purpose, "err", err)
		return false, fmt.Errorf("api: send security code: %w", err)
	}
	return false, nil
}

func (a *memberAPI) requirePassword(w http.ResponseWriter, r *http.Request, m memberRecord, password string) bool {
	if !auth.VerifyPassword(m.PasswordHash, password) {
		WriteError(w, http.StatusUnauthorized, "The password is incorrect.")
		return false
	}
	return true
}

// ---- password change / forgot password ----

func (a *memberAPI) handlePasswordChangeCode(w http.ResponseWriter, r *http.Request, s auth.Session) {
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	tooSoon, err := a.sendEmailCode(r.Context(), m.Email, purposePasswordChange, "change your password")
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "We could not send the verification email right now. Try again shortly.")
		return
	}
	if tooSoon {
		WriteError(w, http.StatusTooManyRequests, "A verification code was just sent. Check your inbox before requesting another.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "code sent"})
}

type passwordChangeRequest struct {
	OldPassword string `json:"oldPassword"`
	Code        string `json:"code"`
	NewPassword string `json:"newPassword"`
}

func (a *memberAPI) handlePasswordChange(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req passwordChangeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if !a.requirePassword(w, r, m, req.OldPassword) {
		return
	}
	if err := auth.CheckPasswordPolicy(req.NewPassword); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := verifyCode(r.Context(), a.pool, purposePasswordChange, m.Email, req.Code, a.env.MaxCodeAttempts); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.setPasswordAndRevokeAll(r.Context(), m.ID, req.NewPassword); err != nil {
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "password changed; all sessions revoked"})
}

// setPasswordAndRevokeAll is the shared tail of password change and
// forgot-password reset: a password event revokes every session.
func (a *memberAPI) setPasswordAndRevokeAll(ctx context.Context, memberID, newPassword string) error {
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE members SET password_hash = $2 WHERE id = $1`, memberID, hash); err != nil {
			return fmt.Errorf("api: update password: %w", err)
		}
		_, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now() WHERE member_id = $1 AND revoked_at IS NULL`, memberID)
		if err != nil {
			return fmt.Errorf("api: revoke sessions: %w", err)
		}
		return nil
	})
}

func (a *memberAPI) handleForgotCode(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if !decodeBody(w, r, &req) {
		return
	}
	email, ok := normalizeEmail(w, req.Email)
	if !ok {
		return
	}
	// Anti-enumeration: unknown addresses answer identically and get no
	// mail; the response never reveals whether an account exists.
	var exists bool
	err := a.pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM members WHERE email = $1)`, email).Scan(&exists)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if exists {
		tooSoon, err := a.sendEmailCode(r.Context(), email, purposePasswordReset, "reset your password")
		if err != nil {
			WriteError(w, http.StatusServiceUnavailable, "We could not send the verification email right now. Try again shortly.")
			return
		}
		if tooSoon {
			WriteError(w, http.StatusTooManyRequests, "A verification code was just sent. Check your inbox before requesting another.")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "code sent"})
}

type forgotResetRequest struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	NewPassword string `json:"newPassword"`
}

func (a *memberAPI) handleForgotReset(w http.ResponseWriter, r *http.Request) {
	var req forgotResetRequest
	if !decodeBody(w, r, &req) {
		return
	}
	email, ok := normalizeEmail(w, req.Email)
	if !ok {
		return
	}
	if err := auth.CheckPasswordPolicy(req.NewPassword); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := verifyCode(r.Context(), a.pool, purposePasswordReset, email, req.Code, a.env.MaxCodeAttempts); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	var memberID string
	err := a.pool.QueryRow(r.Context(),
		`SELECT id FROM members WHERE email = $1`, email).Scan(&memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The code verified against a target with no member — treat as a
		// bad code (no oracle).
		WriteError(w, http.StatusBadRequest, errCodeInvalid.Error())
		return
	}
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if err := a.setPasswordAndRevokeAll(r.Context(), memberID, req.NewPassword); err != nil {
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "password reset; all sessions revoked"})
}

// ---- email change ----

type emailChangeRequest struct {
	Password string `json:"password"`
	NewEmail string `json:"newEmail"`
}

func (a *memberAPI) handleEmailChangeRequest(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req emailChangeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	newEmail, ok := normalizeEmail(w, req.NewEmail)
	if !ok {
		return
	}
	if newEmail == m.Email {
		WriteError(w, http.StatusBadRequest, "Enter a new email address different from your current one.")
		return
	}
	if !a.requirePassword(w, r, m, req.Password) {
		return
	}
	var taken bool
	if err := a.pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM members WHERE email = $1)`, newEmail).Scan(&taken); err != nil {
		writeErr(w, a.log, err)
		return
	}
	if taken {
		WriteError(w, http.StatusConflict, "An account with this email already exists.")
		return
	}
	// Codes to BOTH addresses in one step: the old proves control of the
	// account's current contact, the new proves control of the target.
	var oldCode, newCode string
	var tooSoon bool
	err = pgx.BeginFunc(r.Context(), a.pool, func(tx pgx.Tx) error {
		for _, step := range []struct {
			purpose, target string
			out             *string
		}{
			{purposeEmailChangeOld, m.Email, &oldCode},
			{purposeEmailChangeNew, newEmail, &newCode},
		} {
			recent, err := recentCode(r.Context(), tx, step.purpose, step.target, a.env.CodeCooldown)
			if err != nil {
				return err
			}
			if recent {
				tooSoon = true
				return nil
			}
			code, err := issueCode(r.Context(), tx, step.purpose, step.target, a.env.CodeTTL)
			if err != nil {
				return err
			}
			*step.out = code
		}
		return nil
	})
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if tooSoon {
		WriteError(w, http.StatusTooManyRequests, "A verification code was just sent. Check your inbox before requesting another.")
		return
	}
	for _, delivery := range []struct{ to, code string }{{m.Email, oldCode}, {newEmail, newCode}} {
		subject, body, err := SecurityCodeEmail(delivery.code, a.env.CodeTTL, "confirm your email change")
		if err != nil {
			writeErr(w, a.log, err)
			return
		}
		if err := a.mailer.Send(r.Context(), delivery.to, subject, body); err != nil {
			a.log.Error("email change code delivery failed", "err", err)
			WriteError(w, http.StatusServiceUnavailable, "We could not send the verification email right now. Try again shortly.")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "codes sent to both addresses"})
}

type emailChangeConfirm struct {
	NewEmail string `json:"newEmail"`
	OldCode  string `json:"oldCode"`
	NewCode  string `json:"newCode"`
}

func (a *memberAPI) handleEmailChangeConfirm(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req emailChangeConfirm
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	newEmail, ok := normalizeEmail(w, req.NewEmail)
	if !ok {
		return
	}
	if err := verifyCode(r.Context(), a.pool, purposeEmailChangeOld, m.Email, req.OldCode, a.env.MaxCodeAttempts); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := verifyCode(r.Context(), a.pool, purposeEmailChangeNew, newEmail, req.NewCode, a.env.MaxCodeAttempts); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err = a.pool.Exec(r.Context(),
		`UPDATE members SET email = $2 WHERE id = $1`, m.ID, newEmail)
	if err != nil {
		if isUniqueViolation(err) {
			WriteError(w, http.StatusConflict, "An account with this email already exists.")
			return
		}
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "email changed", "email": newEmail})
}

// ---- phone bind ----

type phoneBindRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (a *memberAPI) handlePhoneBind(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req phoneBindRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validPhone(req.Phone) {
		WriteError(w, http.StatusBadRequest, "Enter the phone number in international format, for example +15551234567.")
		return
	}
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if !a.requirePassword(w, r, m, req.Password) {
		return
	}
	if err := verifyCode(r.Context(), a.pool, purposePhoneBind, req.Phone, req.Code, a.env.MaxCodeAttempts); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err = a.pool.Exec(r.Context(),
		`UPDATE members SET phone = $2 WHERE id = $1`, m.ID, req.Phone)
	if err != nil {
		if isUniqueViolation(err) {
			WriteError(w, http.StatusConflict, "This phone number is already bound to another account.")
			return
		}
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "phone bound", "phone": req.Phone})
}

// ---- Authenticator (TOTP) ----

func (a *memberAPI) handleAuthenticatorCode(w http.ResponseWriter, r *http.Request, s auth.Session) {
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	tooSoon, err := a.sendEmailCode(r.Context(), m.Email, purposeAuthenticatorBind, "bind your authenticator")
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, "We could not send the verification email right now. Try again shortly.")
		return
	}
	if tooSoon {
		WriteError(w, http.StatusTooManyRequests, "A verification code was just sent. Check your inbox before requesting another.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "code sent"})
}

type authenticatorBeginRequest struct {
	Code string `json:"code"` // the emailed code (session + email code)
}

func (a *memberAPI) handleAuthenticatorBegin(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req authenticatorBeginRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	var enabled bool
	err = a.pool.QueryRow(r.Context(),
		`SELECT enabled FROM authenticator_bindings WHERE member_id = $1`, m.ID).Scan(&enabled)
	if err == nil && enabled {
		WriteError(w, http.StatusConflict, "An authenticator is already bound. Unbind it before binding a new one.")
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, a.log, err)
		return
	}
	if err := verifyCode(r.Context(), a.pool, purposeAuthenticatorBind, m.Email, req.Code, a.env.MaxCodeAttempts); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	_, err = a.pool.Exec(r.Context(),
		`INSERT INTO authenticator_bindings (member_id, secret, enabled)
		 VALUES ($1, $2, FALSE)
		 ON CONFLICT (member_id) DO UPDATE SET secret = EXCLUDED.secret, enabled = FALSE, confirmed_at = NULL`,
		m.ID, secret)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	// The secret is shown exactly once, here — the member enters it in
	// their authenticator app and proves possession with a live code.
	writeJSON(w, http.StatusOK, map[string]string{
		"secret":     secret,
		"otpauthUri": auth.OTPAuthURI(m.Email, secret),
	})
}

type authenticatorConfirmRequest struct {
	Code string `json:"code"` // a live code from the authenticator app
}

func (a *memberAPI) handleAuthenticatorConfirm(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req authenticatorConfirmRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var secret string
	err := a.pool.QueryRow(r.Context(),
		`SELECT secret FROM authenticator_bindings WHERE member_id = $1 AND enabled = FALSE`, s.MemberID).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		WriteError(w, http.StatusBadRequest, "Start the binding first so we can show you a secret.")
		return
	}
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if !auth.VerifyTOTP(secret, req.Code, time.Now()) {
		WriteError(w, http.StatusBadRequest, "That authenticator code is not correct. Check the time on your phone and try again.")
		return
	}
	if _, err := a.pool.Exec(r.Context(),
		`UPDATE authenticator_bindings SET enabled = TRUE, confirmed_at = now() WHERE member_id = $1`,
		s.MemberID); err != nil {
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "authenticator bound"})
}

type authenticatorUnbindRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"` // a live Authenticator code
}

func (a *memberAPI) handleAuthenticatorUnbind(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req authenticatorUnbindRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if !a.requirePassword(w, r, m, req.Password) {
		return
	}
	var secret string
	err = a.pool.QueryRow(r.Context(),
		`SELECT secret FROM authenticator_bindings WHERE member_id = $1 AND enabled = TRUE`, m.ID).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		WriteError(w, http.StatusBadRequest, "No authenticator is bound to this account.")
		return
	}
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	if !auth.VerifyTOTP(secret, req.Code, time.Now()) {
		WriteError(w, http.StatusBadRequest, "That authenticator code is not correct. Check the time on your phone and try again.")
		return
	}
	if _, err := a.pool.Exec(r.Context(),
		`DELETE FROM authenticator_bindings WHERE member_id = $1`, m.ID); err != nil {
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "authenticator unbound"})
}

// ---- security summary + KYC ----

func (a *memberAPI) handleSecurity(w http.ResponseWriter, r *http.Request, s auth.Session) {
	m, err := a.loadMember(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	gate := securityGate{pool: a.pool}
	bound, err := gate.AuthenticatorBound(r.Context(), m.ID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	kycStatus, err := a.kycStatus(r.Context(), m.ID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"email":              m.Email,
		"phone":              m.Phone,
		"authenticatorBound": bound,
		"kycStatus":          kycStatus,
	})
}

func (a *memberAPI) kycStatus(ctx context.Context, memberID string) (string, error) {
	var status string
	err := a.pool.QueryRow(ctx,
		`SELECT status FROM kyc_applications WHERE member_id = $1`, memberID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "unverified", nil
	}
	if err != nil {
		return "", fmt.Errorf("api: kyc status: %w", err)
	}
	return status, nil
}

type kycSubmitRequest struct {
	FullName       string `json:"fullName"`
	DocumentNumber string `json:"documentNumber"`
	FrontImage     string `json:"frontImage"` // base64
	BackImage      string `json:"backImage"`  // base64
}

func (a *memberAPI) handleKYCSubmit(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var req kycSubmitRequest
	if !decodeBodyLarge(w, r, &req, 12<<20) {
		return
	}
	if strings.TrimSpace(req.FullName) == "" || strings.TrimSpace(req.DocumentNumber) == "" {
		WriteError(w, http.StatusBadRequest, "Enter your full name and document number exactly as they appear on the document.")
		return
	}
	front, err := decodeKYCImage(req.FrontImage)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "The front document image is not valid base64 data.")
		return
	}
	back, err := decodeKYCImage(req.BackImage)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "The back document image is not valid base64 data.")
		return
	}
	status, err := a.kycStatus(r.Context(), s.MemberID)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	switch status {
	case "verified":
		WriteError(w, http.StatusConflict, "Your identity is already verified.")
		return
	case "pending":
		WriteError(w, http.StatusConflict, "Your identity check is already being reviewed.")
		return
	}
	_, err = a.pool.Exec(r.Context(),
		`INSERT INTO kyc_applications (member_id, full_name, document_number, front_image, back_image, status)
		 VALUES ($1, $2, $3, $4, $5, 'pending')
		 ON CONFLICT (member_id) DO UPDATE SET
		   full_name = EXCLUDED.full_name, document_number = EXCLUDED.document_number,
		   front_image = EXCLUDED.front_image, back_image = EXCLUDED.back_image,
		   status = 'pending', review_reason = '', reviewed_by = '', reviewed_at = NULL,
		   submitted_at = now()`,
		s.MemberID, strings.TrimSpace(req.FullName), strings.TrimSpace(req.DocumentNumber), front, back)
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
}

func decodeKYCImage(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 5<<20 {
		return nil, errors.New("image size out of range")
	}
	return raw, nil
}

func (a *memberAPI) handleKYCState(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var out struct {
		Status         string `json:"status"`
		FullName       string `json:"fullName,omitempty"`
		DocumentNumber string `json:"documentNumber,omitempty"`
		ReviewReason   string `json:"reviewReason,omitempty"`
		SubmittedAt    string `json:"submittedAt,omitempty"`
	}
	var submittedAt time.Time
	err := a.pool.QueryRow(r.Context(),
		`SELECT status, full_name, document_number, review_reason, submitted_at
		   FROM kyc_applications WHERE member_id = $1`, s.MemberID).
		Scan(&out.Status, &out.FullName, &out.DocumentNumber, &out.ReviewReason, &submittedAt)
	out.SubmittedAt = formatTime(submittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "unverified"})
		return
	}
	if err != nil {
		writeErr(w, a.log, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- the withdrawal gate (ticket #40's memberGate) ----

// securityGate is the production memberGate: KYC state, Authenticator
// binding, and live Authenticator verification from the api schema.
type securityGate struct{ pool *pgxpool.Pool }

func (g securityGate) KYCVerified(ctx context.Context, memberID string) (bool, error) {
	var ok bool
	err := g.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM kyc_applications WHERE member_id = $1 AND status = 'verified')`,
		memberID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("api: gate kyc: %w", err)
	}
	return ok, nil
}

func (g securityGate) AuthenticatorBound(ctx context.Context, memberID string) (bool, error) {
	var ok bool
	err := g.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM authenticator_bindings WHERE member_id = $1 AND enabled = TRUE)`,
		memberID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("api: gate authenticator: %w", err)
	}
	return ok, nil
}

func (g securityGate) VerifyAuthenticatorCode(ctx context.Context, memberID, code string) (bool, error) {
	var secret string
	err := g.pool.QueryRow(ctx,
		`SELECT secret FROM authenticator_bindings WHERE member_id = $1 AND enabled = TRUE`, memberID).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("api: gate authenticator secret: %w", err)
	}
	step, ok := auth.VerifyTOTPStep(secret, code, time.Now())
	if !ok {
		return false, nil
	}
	// Advance the consumption marker monotonically: a code from an
	// OLDER step than any already used is a replay after the window
	// moved on and is refused; the same step's code verifies again —
	// two legitimate actions inside one 30-second step present the same
	// code (#103).
	tag, err := g.pool.Exec(ctx,
		`UPDATE authenticator_bindings SET last_used_step = $2 WHERE member_id = $1 AND last_used_step <= $2`,
		memberID, int64(step))
	if err != nil {
		return false, fmt.Errorf("api: gate authenticator consume: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ---- admin internal RPCs ----

func writeInternalErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func adminActor(r *http.Request, bodyActor string) string {
	if bodyActor != "" {
		return bodyActor
	}
	return r.Header.Get("X-Mal-Actor")
}

func (a *memberAPI) handleAdminMemberView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := a.loadMember(r.Context(), id)
	if errors.Is(err, errMemberNotFound) {
		writeInternalErr(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		writeInternalErr(w, http.StatusInternalServerError, "member view failed")
		return
	}
	gate := securityGate{pool: a.pool}
	bound, err := gate.AuthenticatorBound(r.Context(), id)
	if err != nil {
		writeInternalErr(w, http.StatusInternalServerError, "member view failed")
		return
	}
	kycStatus, err := a.kycStatus(r.Context(), id)
	if err != nil {
		writeInternalErr(w, http.StatusInternalServerError, "member view failed")
		return
	}
	var telegramBound bool
	_ = a.pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM member_telegram_chats WHERE member_id = $1)`, id).Scan(&telegramBound)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": m.ID, "email": m.Email, "status": m.Status, "phone": m.Phone,
		"authenticatorBound": bound, "kycStatus": kycStatus,
		"telegramBound": telegramBound, "createdAt": formatTime(m.CreatedAt),
	})
}

func (a *memberAPI) handleAdminMemberLock(w http.ResponseWriter, r *http.Request) {
	a.adminSetStatus(w, r, "locked")
}

func (a *memberAPI) handleAdminMemberUnlock(w http.ResponseWriter, r *http.Request) {
	a.adminSetStatus(w, r, "active")
}

func (a *memberAPI) adminSetStatus(w http.ResponseWriter, r *http.Request, status string) {
	id := r.PathValue("id")
	err := pgx.BeginFunc(r.Context(), a.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`UPDATE members SET status = $2 WHERE id = $1`, id, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errMemberNotFound
		}
		if status == "locked" {
			// Locking kills live sessions in the same transaction.
			_, err = tx.Exec(r.Context(),
				`UPDATE sessions SET revoked_at = now() WHERE member_id = $1 AND revoked_at IS NULL`, id)
			return err
		}
		return nil
	})
	if errors.Is(err, errMemberNotFound) {
		writeInternalErr(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		a.log.Error("admin set member status", "err", err)
		writeInternalErr(w, http.StatusInternalServerError, "member status update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": status})
}

type kycReviewRequest struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"`
	Actor    string `json:"actor"`
}

func (a *memberAPI) handleAdminKYCReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req kycReviewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	status := "rejected"
	if req.Approved {
		status = "verified"
	}
	tag, err := a.pool.Exec(r.Context(),
		`UPDATE kyc_applications
		    SET status = $2, review_reason = $3, reviewed_by = $4, reviewed_at = now()
		  WHERE member_id = $1 AND status = 'pending'`,
		id, status, req.Reason, adminActor(r, req.Actor))
	if err != nil {
		a.log.Error("admin kyc review", "err", err)
		writeInternalErr(w, http.StatusInternalServerError, "kyc review failed")
		return
	}
	if tag.RowsAffected() == 0 {
		writeInternalErr(w, http.StatusNotFound, "no pending kyc application for this member")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"memberId": id, "kycStatus": status})
}

type authenticatorResetRequest struct {
	Actor string `json:"actor"`
}

func (a *memberAPI) handleAdminAuthenticatorReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req authenticatorResetRequest
	_ = decodeBody(w, r, &req) // actor is optional in the body; header works too
	actor := adminActor(r, req.Actor)

	m, err := a.loadMember(r.Context(), id)
	if errors.Is(err, errMemberNotFound) {
		writeInternalErr(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		writeInternalErr(w, http.StatusInternalServerError, "authenticator reset failed")
		return
	}
	// Recovery is for KYC-verified members only: the human compared a
	// face against the verified documents (ADR-0006).
	verified, err := securityGate{pool: a.pool}.KYCVerified(r.Context(), id)
	if err != nil {
		writeInternalErr(w, http.StatusInternalServerError, "authenticator reset failed")
		return
	}
	if !verified {
		writeInternalErr(w, http.StatusConflict, "authenticator reset requires a KYC-verified member")
		return
	}
	err = pgx.BeginFunc(r.Context(), a.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(),
			`DELETE FROM authenticator_bindings WHERE member_id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(),
			`INSERT INTO security_audit (member_id, action, actor, detail)
			 VALUES ($1, 'authenticator_reset', $2, 'admin-mediated authenticator recovery')`,
			id, actor)
		return err
	})
	if err != nil {
		a.log.Error("admin authenticator reset", "err", err)
		writeInternalErr(w, http.StatusInternalServerError, "authenticator reset failed")
		return
	}
	a.notifyAuthenticatorReset(r.Context(), m)
	writeJSON(w, http.StatusOK, map[string]string{"memberId": id, "status": "authenticator reset"})
}

// notifyAuthenticatorReset tells the member on every active channel:
// the email always, plus a bound phone (SMS/WhatsApp) or Telegram chat
// when those channels are active. Delivery failures are logged, never
// fatal — the reset already happened and is audit-logged.
func (a *memberAPI) notifyAuthenticatorReset(ctx context.Context, m memberRecord) {
	subject, body, err := AuthenticatorResetEmail()
	if err != nil {
		a.log.Error("authenticator reset template", "err", err)
		return
	}
	if err := a.mailer.Send(ctx, m.Email, subject, body); err != nil {
		a.log.Error("authenticator reset email failed", "err", err)
	}
	message := "Your authenticator was reset by our support team after an identity check. Withdrawals stay blocked until you bind a new authenticator. If you did not ask for this reset, contact support immediately."
	_ = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if m.Phone != "" {
			for _, name := range []string{ChannelSMS, ChannelWhatsApp} {
				creds, active, err := loadChannel(ctx, tx, name)
				if err != nil || !active {
					continue
				}
				if err := channelSenders[name].Send(ctx, creds, m.Phone, message); err != nil {
					a.log.Error("authenticator reset phone notice failed", "channel", name, "err", err)
				}
				break
			}
		}
		var chatID int64
		err := tx.QueryRow(ctx,
			`SELECT chat_id FROM member_telegram_chats WHERE member_id = $1`, m.ID).Scan(&chatID)
		if err == nil {
			if creds, active, lerr := loadChannel(ctx, tx, ChannelTelegram); lerr == nil && active {
				if err := (telegramSender{}).Send(ctx, creds, fmt.Sprintf("%d", chatID), message); err != nil {
					a.log.Error("authenticator reset telegram notice failed", "err", err)
				}
			}
		}
		return nil
	})
}

// decodeBodyLarge is decodeBody with a caller-chosen size cap (KYC
// document images travel as base64 JSON and exceed the 64 KiB default).
func decodeBodyLarge(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		WriteError(w, http.StatusBadRequest, "The request body is not valid JSON.")
		return false
	}
	return true
}
