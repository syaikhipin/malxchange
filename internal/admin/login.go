// The Employee auth surface: login (email + password + mandatory TOTP
// once bound), logout, and me — the session-resolution seam. The
// opaque token rides an httpOnly cookie (no remember-me variant
// exists); failed authentication is refused identically whether the
// email, the password, or the code was wrong. Every outcome is audited
// by the wrapping middleware: login:employee, login:failed, or
// login:lockout.
package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/syaikhipin/malxchange/internal/auth"
)

// RegisterPublic mounts the Employee surface (paths stay unprefixed:
// this mux serves only the admin surface, like the member api's
// /api/uc/* namespace). The permissioned management endpoints mount
// from the registry — the single seam later tickets extend.
func (s *Server) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", s.audited(ActionLogin, s.handleLogin))
	mux.HandleFunc("POST /api/logout", s.withSession(s.audited(ActionLogout, s.handleLogout)))
	mux.HandleFunc("GET /api/me", s.withSession(s.handleMe))
	mux.HandleFunc("POST /api/password", s.withSession(s.audited(ActionRotatePassword, s.handleRotatePassword)))
	mux.HandleFunc("POST /api/totp/enroll", s.withSession(s.audited(ActionEnrollTOTP, s.handleEnrollTOTP)))
	mux.HandleFunc("POST /api/totp/confirm", s.withSession(s.audited(ActionBindTOTP, s.handleConfirmTOTP)))
	s.RegisterEndpoints(mux)
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

// employeePublic is the login/me identity view; loginResponse joins
// the live permission set (the sidebar's source of truth).
type employeePublic struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type loginResponse struct {
	Employee    employeePublic `json:"employee"`
	MustRotate  bool           `json:"must_rotate"`
	TOTPBound   bool           `json:"totp_bound"`
	Permissions []string       `json:"permissions"` // the caller's live grant set from their single Role (#54)
}

// handleLogin answers only email + password + a valid TOTP code — the
// code is mandatory for every provisioned Employee (ADR-0012). The
// first, unprovisioned login (bootstrap credential) authenticates with
// password alone and lands in the restricted state: the session gate
// refuses everything but the forced-setup endpoints.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	entry := auditEntry(r)
	var req loginRequest
	if !decodeBody(w, r, &req) {
		return
	}
	email, ok := normalizeEmail(w, req.Email)
	if !ok {
		return
	}
	if entry != nil {
		entry.Actor = email
	}

	// The lock is checked before anything else and refused attempts
	// record no failure, so a locked email cannot extend its own lock.
	locked, err := auth.LoginLockedOut(r.Context(), s.pool, email, s.env.LockoutThreshold, s.env.LockoutWindow)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	if locked {
		auditRefusal(entry, ActionLockout)
		WriteError(w, http.StatusTooManyRequests,
			"Too many failed sign-in attempts. Try again in a few minutes.")
		return
	}

	emp, err := s.employeeByEmail(r.Context(), email)
	if errors.Is(err, errEmployeeNotFound) {
		// Unknown email and wrong password answer identically (no
		// account oracle), but the failed event is still audited — and
		// NOT counted: a stranger must not lock an address by guessing.
		auditRefusal(entry, ActionLoginFailed)
		WriteError(w, http.StatusUnauthorized, "Email, password, or authenticator code is incorrect.")
		return
	}
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	if !emp.Enabled {
		auditRefusal(entry, ActionLoginFailed)
		WriteError(w, http.StatusForbidden, "This account is disabled.")
		return
	}
	if !auth.VerifyPassword(emp.PasswordHash, req.Password) {
		// Only real Employee emails are counted toward the lockout.
		if err := auth.RecordLoginFailure(r.Context(), s.pool, email); err != nil {
			writeErr(w, s.log, err)
			return
		}
		auditRefusal(entry, ActionLoginFailed)
		WriteError(w, http.StatusUnauthorized, "Email, password, or authenticator code is incorrect.")
		return
	}

	// TOTP: mandatory once bound. A wrong code counts toward the
	// lockout (login is exactly where a stolen secret gets brute
	// forced); a missing code is a client bug, audited but not counted.
	if emp.TOTPSecret != "" {
		if req.Code == "" {
			auditRefusal(entry, ActionLoginFailed)
			WriteError(w, http.StatusUnauthorized, "Your authenticator code is required.")
			return
		}
		if step, ok := auth.VerifyTOTPStep(emp.TOTPSecret, req.Code, time.Now()); !ok || !s.consumeTOTPStep(r.Context(), emp.ID, step) {
			if err := auth.RecordLoginFailure(r.Context(), s.pool, email); err != nil {
				writeErr(w, s.log, err)
				return
			}
			auditRefusal(entry, ActionLoginFailed)
			WriteError(w, http.StatusUnauthorized, "Email, password, or authenticator code is incorrect.")
			return
		}
	}

	token, _, err := auth.MintSession(r.Context(), s.pool, emp.ID, "admin", s.env.SessionPolicy())
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	if err := auth.ClearLoginFailures(r.Context(), s.pool, email); err != nil {
		s.log.Error("admin: clearing login failures", "err", err) // non-fatal
	}
	if entry != nil {
		entry.Action = ActionLogin
		entry.Target = emp.ID
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, loginResponse{
		Employee:    employeePublic{ID: emp.ID, Email: emp.Email, Role: s.roleName(r.Context(), emp)},
		MustRotate:  emp.MustRotate,
		TOTPBound:   emp.TOTPSecret != "",
		Permissions: s.permissionList(r.Context(), emp.RoleID),
	})
}

// auditRefusal retargets the envelope for a refused authentication and
// forces the row: refusals are the events the log must not drop.
func auditRefusal(entry *AuditEntry, action string) {
	if entry == nil {
		return
	}
	entry.Action = action
	entry.Force = true
}

// roleName resolves the Role name for identity views (one join, tiny;
// grants resolve through grantsFor).
func (s *Server) roleName(ctx context.Context, emp Employee) string {
	var name string
	if err := s.pool.QueryRow(ctx,
		`SELECT name FROM roles WHERE id = $1`, emp.RoleID).Scan(&name); err != nil {
		return ""
	}
	return name
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	entry := auditEntry(r)
	if entry != nil {
		entry.Target = sess.ID
	}
	if err := auth.RevokeSession(r.Context(), s.pool, sess.ID); err != nil {
		writeErr(w, s.log, err)
		return
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	emp, _ := employeeFrom(r.Context())
	writeJSON(w, http.StatusOK, loginResponse{
		Employee:    employeePublic{ID: emp.ID, Email: emp.Email, Role: s.roleName(r.Context(), emp)},
		MustRotate:  emp.MustRotate,
		TOTPBound:   emp.TOTPSecret != "",
		Permissions: s.permissionList(r.Context(), emp.RoleID),
	})
}
