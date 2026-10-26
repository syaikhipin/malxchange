// Step-up (ADR-0012): the fresh-TOTP-at-action-time challenge for
// fund- and credential-moving actions. Password re-auth is NEVER
// Step-up — a session thief already has the password — so this helper
// reads only the request's code field and never any password field.
// It is the seam every later surface calls: manual balance adjust,
// member TOTP reset, withdrawal approval, hot→cold transfer,
// broadcast/sweep retry, and Code channel credential saves (#58-#63);
// employee create and employee password/TOTP resets (#54) are the
// first callers.
package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/syaikhipin/malxchange/internal/auth"
)

const (
	errStepUpRequired = "A fresh authenticator code is required for this action."
	errStepUpInvalid  = "That code is not valid. Check your authenticator app and try again."
)

// consumeTOTPStep advances the consumption marker monotonically: false
// means an OLDER step than any already used — a replay after the window
// moved on — or the storage failed; both refuse the action (fail
// closed). A code from the SAME step verifies again on purpose: two
// legitimate actions inside one 30-second step present the same code,
// and rejecting them would lock every burst workflow (onboarding a
// batch of employees, bind-then-withdraw) until the next tick. The
// residual exposure — the same still-valid code used twice inside its
// window — is inherent to TOTP at step granularity and is bounded by
// the rate limits (audit issue #104).
func (s *Server) consumeTOTPStep(ctx context.Context, employeeID string, step uint64) bool {
	if s.pool == nil { // no storage to record the claim in: refuse (fail closed)
		return false
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE employees SET last_used_step = $2 WHERE id = $1 AND last_used_step <= $2`,
		employeeID, int64(step))
	if err != nil {
		s.log.Error("admin: consume authenticator step", "err", err)
		return false
	}
	return tag.RowsAffected() == 1
}

// requireStepUp verifies a fresh authenticator code against the
// CALLER's bound TOTP secret at action time and answers the refusal
// itself. Callers hand it the code field of their own request body.
func (s *Server) requireStepUp(w http.ResponseWriter, r *http.Request, code string) bool {
	emp, _ := employeeFrom(r.Context())
	if code == "" {
		WriteError(w, http.StatusUnauthorized, errStepUpRequired)
		return false
	}
	if emp.TOTPSecret == "" {
		WriteError(w, http.StatusUnauthorized, errStepUpInvalid)
		return false
	}
	step, ok := auth.VerifyTOTPStep(emp.TOTPSecret, code, time.Now())
	if !ok || !s.consumeTOTPStep(r.Context(), emp.ID, step) {
		WriteError(w, http.StatusUnauthorized, errStepUpInvalid)
		return false
	}
	return true
}
