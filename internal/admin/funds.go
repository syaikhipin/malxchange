// The finance funds surface (spec #59): the withdrawal audit queue —
// list with member/state filters, detail, approve / reject / retry /
// refund — and the manual balance adjustment, orchestrated over the
// wallet's internal RPCs. The wallet stays the sole Ledger writer
// (ADR-0008): admin-api adds permission + Step-up + the Audit log and
// never touches balances itself. Every action here is a mutating
// endpoint registered in registry.go, so the audited() middleware
// writes one row per request with action == permission string; the
// handlers only fill in Target (the affected id — never the caller)
// and the adjustment's before/after balance views.
package admin

import (
	"net/http"
	"strconv"

	"github.com/syaikhipin/malxchange/internal/money"
)

// stepUpCodeRequest is the body of every Step-up-guarded action: the
// fresh authenticator code of the CALLING Employee (ADR-0012 —
// password re-auth is never accepted).
type stepUpCodeRequest struct {
	Code string `json:"code"`
}

// handleListWithdrawals answers the audit queue: withdrawals filtered
// by member and/or state, newest first (the wallet caps limit at 500,
// defaulting 100; a non-numeric limit parses as 0 and gets the
// default).
func (s *Server) handleListWithdrawals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := s.wallet.withdrawals(r.Context(), q.Get("member"), q.Get("state"), limit)
	if err != nil {
		s.writeWalletErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleWithdrawalDetail answers one full withdrawal record — the
// queue's drill-down.
func (s *Server) handleWithdrawalDetail(w http.ResponseWriter, r *http.Request) {
	wd, err := s.wallet.withdrawal(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeWalletErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wd)
}

// handleApproveWithdrawal sends an audited withdrawal to the signer.
// Step-up is mandatory (ADR-0012: withdrawal approval); without a
// fresh code the refusal is 401 before the wallet is touched.
func (s *Server) handleApproveWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req stepUpCodeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !s.requireStepUp(w, r, req.Code) {
		return
	}
	id := r.PathValue("id")
	if err := s.wallet.approveWithdrawal(r.Context(), id, s.actorOf(r)); err != nil {
		s.writeWalletErr(w, err)
		return
	}
	if entry := auditEntry(r); entry != nil {
		entry.Target = id
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRejectWithdrawal refuses an audited withdrawal and returns the
// funds through the wallet's Journal (idempotent on the withdrawal id).
// ADR-0012 waiver — reject/refund are outside the deciding Step-up list
// (issue #25 Implementation Decisions); they only reverse or return,
// never move funds out.
func (s *Server) handleRejectWithdrawal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.wallet.rejectWithdrawal(r.Context(), id, s.actorOf(r)); err != nil {
		s.writeWalletErr(w, err)
		return
	}
	if entry := auditEntry(r); entry != nil {
		entry.Target = id
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRetryWithdrawal re-arms a failed withdrawal for signing.
// Step-up is mandatory (ADR-0012: failed-broadcast retry) — a retry
// re-broadcasts money on chain.
func (s *Server) handleRetryWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req stepUpCodeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !s.requireStepUp(w, r, req.Code) {
		return
	}
	id := r.PathValue("id")
	if err := s.wallet.retryWithdrawal(r.Context(), id, s.actorOf(r)); err != nil {
		s.writeWalletErr(w, err)
		return
	}
	if entry := auditEntry(r); entry != nil {
		entry.Target = id
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRefundWithdrawal returns a failed withdrawal's funds to the
// member when the Employee decides against a retry.
// ADR-0012 waiver — reject/refund are outside the deciding Step-up list
// (issue #25 Implementation Decisions); they only reverse or return,
// never move funds out.
func (s *Server) handleRefundWithdrawal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.wallet.refundWithdrawal(r.Context(), id, s.actorOf(r)); err != nil {
		s.writeWalletErr(w, err)
		return
	}
	if entry := auditEntry(r); entry != nil {
		entry.Target = id
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// adjustmentRequest is one manual balance movement: a signed decimal
// amount at the Coin's scale, a mandatory reason (the audit log and
// the Journal entry both carry it), and the caller's idempotency key —
// the browser generates one per submission, so a retried request is a
// no-op instead of a second movement.
type adjustmentRequest struct {
	Member string `json:"member"`
	Coin   string `json:"coin"`
	Amount string `json:"amount"`
	Reason string `json:"reason"`
	Key    string `json:"key"`
	Code   string `json:"code"`
}

// handleAdjustment posts a manual balance movement to the wallet,
// which appends the Journal adjustment entry and moves the Ledger in
// one transaction (spec #22 story 15). Step-up is mandatory (ADR-0012:
// manual balance adjust). The audit row carries the member's balance
// rows before and after as JSON.
func (s *Server) handleAdjustment(w http.ResponseWriter, r *http.Request) {
	var req adjustmentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Member == "" || req.Coin == "" || req.Reason == "" || req.Key == "" {
		WriteError(w, http.StatusBadRequest, "Member, coin, reason, and an idempotency key are required.")
		return
	}
	amount, err := money.Parse(req.Amount)
	if err != nil || amount.IsZero() {
		WriteError(w, http.StatusBadRequest, "Enter a non-zero signed amount.")
		return
	}
	if !s.requireStepUp(w, r, req.Code) {
		return
	}
	actor := s.actorOf(r)
	ctx := r.Context()
	before, err := s.wallet.accounts(ctx, req.Member)
	if err != nil {
		s.writeWalletErr(w, err)
		return
	}
	applied, err := s.wallet.adjustBalance(ctx, req.Member, req.Coin, req.Amount, req.Reason, req.Key, actor)
	if err != nil {
		s.writeWalletErr(w, err)
		return
	}
	after, err := s.wallet.accounts(ctx, req.Member)
	if err != nil {
		s.writeWalletErr(w, err)
		return
	}
	if entry := auditEntry(r); entry != nil {
		entry.Target = req.Member
		entry.Before = mustJSON(before)
		entry.After = mustJSON(after)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "applied": applied})
}

// actorOf is the attribution the wallet's Journal records for every
// admin-caused movement: the acting Employee's email — the same
// identifier the audit log's actor column carries.
func (s *Server) actorOf(r *http.Request) string {
	if emp, ok := employeeFrom(r.Context()); ok {
		return emp.Email
	}
	return ""
}
