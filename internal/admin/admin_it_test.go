//go:build integration

// Employee auth core over the real seam (ticket #53, ADR-0012): HTTP
// against the production mux wiring (public surface + /internal
// bootstrap RPC) and Postgres in a throwaway database — per-binary
// unique name, sibling IT suites share the dev Postgres. Assertions go
// through the served surface; the audit_log queries below are the
// black-box read of what the middleware must have written. Admin
// produces no topics, so unlike the api suite this harness needs no
// broker.
package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/syaikhipin/malxchange/internal/auth"
	"github.com/syaikhipin/malxchange/internal/migrations"
	"github.com/syaikhipin/malxchange/internal/pgmig"
)

var (
	itPGHost = os.Getenv("MAL_TEST_PG_HOST")
	itPGPW   = os.Getenv("MAL_TEST_PG_SUPERUSER_PASSWORD")
)

func TestMain(m *testing.M) {
	if itPGHost == "" || itPGPW == "" {
		fmt.Println("skipping admin integration tests: set MAL_TEST_PG_* (make test-integration)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type itStack struct {
	pool *pgxpool.Pool // search_path=admin
	env  *AdminEnv
	rest *httptest.Server
	t    *testing.T
}

func newStack(t *testing.T, tweaks ...func(*AdminEnv)) *itStack {
	t.Helper()
	ctx := context.Background()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	dbName := "malxchange_itad_" + hex.EncodeToString(suffix)

	adminPool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://%s:%s@%s:5432/postgres",
		os.Getenv("MAL_TEST_PG_SUPERUSER"), itPGPW, itPGHost))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName)); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatal(err)
	}
	adminPool.Close()
	t.Cleanup(func() {
		adminPool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://%s:%s@%s:5432/postgres",
			os.Getenv("MAL_TEST_PG_SUPERUSER"), itPGPW, itPGHost))
		if err != nil {
			return
		}
		defer adminPool.Close()
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName))
	})

	pool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable&search_path=admin",
		os.Getenv("MAL_TEST_PG_SUPERUSER"), itPGPW, itPGHost, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS admin`); err != nil {
		t.Fatal(err)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(os.Getenv("MAL_TEST_PG_SUPERUSER"), itPGPW),
		Host:     itPGHost + ":5432",
		Path:     dbName,
		RawQuery: "sslmode=disable&search_path=admin",
	}
	if err := pgmig.Run(u.String(), migrations.FS, "admin"); err != nil {
		t.Fatalf("admin migrations: %v", err)
	}

	// Test-tuned env: real ADR-0012 session windows (expiry legs shift
	// timestamps in SQL instead of sleeping), a tight lockout window so
	// the lock-lifts leg is quick.
	env := &AdminEnv{
		SessionAbsolute:  12 * time.Hour,
		SessionIdle:      30 * time.Minute,
		LockoutThreshold: LockoutThreshold,
		LockoutWindow:    1500 * time.Millisecond,
	}
	for _, tweak := range tweaks {
		tweak(env)
	}
	s := &itStack{pool: pool, env: env, t: t}

	// The boot-harness wiring (internal/service/service.go): public mux
	// at the root, internal mux stripped from /internal/*. Boot order
	// mirrors admin.Run: the RBAC seeding (roles + catalog + matrix
	// grants, idempotent) lands before the surface serves.
	root := http.NewServeMux()
	internal := http.NewServeMux()
	srv := NewServer(pool, env, nil)
	if err := srv.SeedRBAC(ctx); err != nil {
		t.Fatalf("seed rbac: %v", err)
	}
	srv.RegisterPublic(root)
	srv.RegisterInternal(internal)
	// A guarded probe route: proves the restricted-session rule over the
	// production middleware without inventing a #54 endpoint here.
	root.HandleFunc("GET /api/probe", srv.withSession(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	}))
	root.Handle("/internal/", http.StripPrefix("/internal", internal))
	s.rest = httptest.NewServer(root)
	t.Cleanup(s.rest.Close)
	return s
}

// ---- HTTP helpers ----

func (s *itStack) do(t *testing.T, method, path, body string, cookie *http.Cookie) (int, *http.Response, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, s.rest.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := s.rest.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp, string(raw)
}

func (s *itStack) post(t *testing.T, path, body string, cookie *http.Cookie) (int, *http.Response, string) {
	t.Helper()
	return s.do(t, "POST", path, body, cookie)
}

func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", sessionCookie)
	return nil
}

// totpNow computes the current authenticator code for a base32 secret
// (the same RFC 6238 parameters the shared auth package verifies).
func totpNow(secret string) (string, error) {
	return auth.TOTPCode(secret, time.Now())
}

// bootstrap creates the first Employee through the internal RPC the
// installer drives.
func (s *itStack) bootstrap(t *testing.T, email, password string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)
	status, _, raw := s.post(t, "/internal/bootstrap/employee", body, nil)
	return status, raw
}

type auditRow struct {
	Actor    string          `db:"actor"`
	Action   string          `db:"action"`
	Target   string          `db:"target"`
	Before   json.RawMessage `db:"before"`
	After    json.RawMessage `db:"after"`
	OriginIP string          `db:"origin_ip"`
}

func (s *itStack) auditRows(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT actor, action, target, COALESCE(before, 'null'::jsonb), COALESCE(after, 'null'::jsonb), origin_ip
		   FROM audit_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Actor, &r.Action, &r.Target, &r.Before, &r.After, &r.OriginIP); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// shiftSessions moves a sessions timestamp column for every live row —
// the deterministic expiry leg (no sleeps).
func (s *itStack) shiftSessions(t *testing.T, assignment string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `UPDATE sessions SET `+assignment); err != nil {
		t.Fatal(err)
	}
}

// fullyProvisioned runs bootstrap + the whole forced-setup journey and
// leaves the Employee with known credentials + TOTP secret, as the
// installer's operator would end up with.
type provisioned struct {
	Email    string
	Password string // rotated in
	TOTPSecr string // base32, from enrollment
	EmpID    string
}

func (s *itStack) fullyProvisioned(t *testing.T, email string) provisioned {
	t.Helper()
	const oneTime = "one-time-password-1"
	if status, raw := s.bootstrap(t, email, oneTime); status != http.StatusOK {
		t.Fatalf("bootstrap: status %d body %s", status, raw)
	}
	return s.finishSetup(t, email, oneTime)
}

// finishSetup walks the forced first-login journey for an Employee
// created with the one-time password: restricted login → password
// rotation → re-login → TOTP enrollment + confirm. It is the tail of
// the bootstrap journey and, unchanged, the journey a management-side
// create (#54) puts every new Employee on.
func (s *itStack) finishSetup(t *testing.T, email, oneTime string) provisioned {
	t.Helper()
	p := provisioned{Email: email, Password: oneTime}

	// First login: one-time password, no TOTP yet → restricted session.
	status, resp, raw := s.post(t, "/api/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, oneTime), nil)
	if status != http.StatusOK {
		t.Fatalf("first login: status %d body %s", status, raw)
	}
	cookie := sessionCookieOf(t, resp)

	// Rotate (forced): the one-time credential dies here.
	status, _, raw = s.post(t, "/api/password",
		fmt.Sprintf(`{"current_password":%q,"new_password":"rotated-password-2"}`, oneTime), cookie)
	if status != http.StatusOK {
		t.Fatalf("rotate: status %d body %s", status, raw)
	}
	p.Password = "rotated-password-2"

	// Re-login with the rotated password (still TOTP-less → restricted).
	status, resp, raw = s.post(t, "/api/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, p.Password), nil)
	if status != http.StatusOK {
		t.Fatalf("post-rotation login: status %d body %s", status, raw)
	}
	cookie = sessionCookieOf(t, resp)

	// Enroll + confirm TOTP.
	status, _, raw = s.post(t, "/api/totp/enroll", `{}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("enroll: status %d body %s", status, raw)
	}
	var enroll struct {
		Secret     string `json:"secret"`
		OtpauthURI string `json:"otpauth_uri"`
	}
	if err := json.Unmarshal([]byte(raw), &enroll); err != nil || enroll.Secret == "" || enroll.OtpauthURI == "" {
		t.Fatalf("enroll body %s err %v", raw, err)
	}
	p.TOTPSecr = enroll.Secret
	code, err := totpNow(enroll.Secret)
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw = s.post(t, "/api/totp/confirm", fmt.Sprintf(`{"code":%q}`, code), cookie)
	if status != http.StatusOK {
		t.Fatalf("confirm: status %d body %s", status, raw)
	}

	var me struct {
		Employee struct {
			ID string `json:"id"`
		} `json:"employee"`
		TOTPBound bool `json:"totp_bound"`
	}
	status, _, raw = s.do(t, "GET", "/api/me", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("me after setup: status %d body %s", status, raw)
	}
	if err := json.Unmarshal([]byte(raw), &me); err != nil {
		t.Fatal(err)
	}
	p.EmpID = me.Employee.ID
	if !me.TOTPBound {
		t.Fatalf("expected totp_bound after confirm: %s", raw)
	}
	return p
}

func (p provisioned) login(t *testing.T, s *itStack, code string) (int, *http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"password":%q,"code":%q}`, p.Email, p.Password, code)
	return s.post(t, "/api/login", body, nil)
}

// ---- acceptance tests ----

func TestBootstrapCreatesFirstEmployeeAndRefusesMore(t *testing.T) {
	s := newStack(t)

	status, raw := s.bootstrap(t, "root@it.test", "one-time-password-1")
	if status != http.StatusOK {
		t.Fatalf("first bootstrap: status %d body %s", status, raw)
	}
	var created struct {
		ID         string `json:"id"`
		Email      string `json:"email"`
		Role       string `json:"role"`
		MustRotate bool   `json:"must_rotate"`
	}
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Email != "root@it.test" || created.Role != "super-admin" || !created.MustRotate {
		t.Fatalf("unexpected bootstrap body: %s", raw)
	}

	// The refusal: one Employee exists, the RPC is done forever.
	status, raw = s.bootstrap(t, "second@it.test", "another-password-2")
	if status != http.StatusConflict {
		t.Fatalf("refused bootstrap: status %d body %s", status, raw)
	}

	// The middleware wrote exactly one audit row: the creation, as the
	// system actor, with a redacted after-view (no secrets).
	rows := s.auditRows(t, ActionCreateEmployee)
	if len(rows) != 1 {
		t.Fatalf("want 1 create:employee audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Actor != systemActor || row.Target != created.ID {
		t.Fatalf("audit row actor=%q target=%q", row.Actor, row.Target)
	}
	var after map[string]any
	if err := json.Unmarshal(row.After, &after); err != nil {
		t.Fatal(err)
	}
	if after["email"] != "root@it.test" {
		t.Fatalf("audit after.email = %v", after["email"])
	}
	for _, banned := range []string{"password", "password_hash", "totp_secret", "totp_staged"} {
		if _, ok := after[banned]; ok {
			t.Fatalf("audit after carries secret field %q: %v", banned, after)
		}
	}
}

func TestForcedSetupJourney(t *testing.T) {
	s := newStack(t)
	const oneTime = "one-time-password-1"
	status, raw := s.bootstrap(t, "setup@it.test", oneTime)
	if status != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", status, raw)
	}

	// First login: email + one-time password, NO TOTP (none enrolled).
	status, resp, raw := s.post(t, "/api/login",
		fmt.Sprintf(`{"email":"setup@it.test","password":%q}`, oneTime), nil)
	if status != http.StatusOK {
		t.Fatalf("first login: %d %s", status, raw)
	}
	cookie := sessionCookieOf(t, resp)
	var login struct {
		Employee struct {
			Email string `json:"email"`
		} `json:"employee"`
		MustRotate bool `json:"must_rotate"`
		TOTPBound  bool `json:"totp_bound"`
	}
	if err := json.Unmarshal([]byte(raw), &login); err != nil {
		t.Fatal(err)
	}
	if !login.MustRotate || login.TOTPBound {
		t.Fatalf("expected restricted state, got %s", raw)
	}

	// The cookie flags: httpOnly, Secure, SameSite=lax, and NO
	// Max-Age/Expires — no remember-me path exists.
	var sawCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			sawCookie = c
		}
	}
	if sawCookie == nil || !sawCookie.HttpOnly || !sawCookie.Secure || sawCookie.MaxAge != 0 || !sawCookie.Expires.IsZero() {
		t.Fatalf("session cookie flags wrong: %+v", sawCookie)
	}
	if sawCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite = %v", sawCookie.SameSite)
	}

	// Restricted: everything outside the setup allowlist is refused —
	// forced rotation + enrollment come before anything else.
	status, _, raw = s.do(t, "GET", "/api/probe", "", cookie)
	if status != http.StatusForbidden {
		t.Fatalf("restricted probe: %d %s", status, raw)
	}

	// Rotating the password kills every session (this one included).
	status, _, raw = s.post(t, "/api/password",
		fmt.Sprintf(`{"current_password":%q,"new_password":"rotated-password-2"}`, oneTime), cookie)
	if status != http.StatusOK {
		t.Fatalf("rotate: %d %s", status, raw)
	}
	status, _, _ = s.do(t, "GET", "/api/me", "", cookie)
	if status != http.StatusUnauthorized {
		t.Fatalf("old cookie after rotation: %d", status)
	}

	// The one-time password is refused afterwards.
	status, _, raw = s.post(t, "/api/login",
		fmt.Sprintf(`{"email":"setup@it.test","password":%q}`, oneTime), nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("one-time password reused: %d %s", status, raw)
	}

	// Back in with the rotated password (restricted again: TOTP still
	// unbound) to finish enrollment.
	status, resp, raw = s.post(t, "/api/login",
		`{"email":"setup@it.test","password":"rotated-password-2"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("post-rotation login: %d %s", status, raw)
	}
	cookie = sessionCookieOf(t, resp)
	status, _, raw = s.do(t, "GET", "/api/probe", "", cookie)
	if status != http.StatusForbidden {
		t.Fatalf("restricted probe after rotation: %d %s", status, raw)
	}

	status, _, raw = s.post(t, "/api/totp/enroll", `{}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("enroll: %d %s", status, raw)
	}
	var enroll struct {
		Secret     string `json:"secret"`
		OtpauthURI string `json:"otpauth_uri"`
	}
	if err := json.Unmarshal([]byte(raw), &enroll); err != nil || enroll.Secret == "" {
		t.Fatalf("enroll body: %s", raw)
	}
	code, err := totpNow(enroll.Secret)
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw = s.post(t, "/api/totp/confirm", fmt.Sprintf(`{"code":%q}`, code), cookie)
	if status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, raw)
	}

	// Setup complete: the SAME session now reaches the full surface —
	// the restriction is derived state, no second login demanded.
	status, _, raw = s.do(t, "GET", "/api/probe", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("probe after setup: %d %s", status, raw)
	}

	// The middleware wrote the setup mutations: rotation, enrollment
	// begin, binding — with before/after views and no secrets. (The
	// enroll row's {"totp_staged": true} is the boolean status flag,
	// never the staged secret itself.)
	for _, action := range []string{ActionRotatePassword, ActionEnrollTOTP, ActionBindTOTP} {
		rows := s.auditRows(t, action)
		if len(rows) != 1 {
			t.Fatalf("want 1 %s row, got %d", action, len(rows))
		}
		if rows[0].Actor != "setup@it.test" || rows[0].Target == "" {
			t.Fatalf("%s row actor=%q target=%q", action, rows[0].Actor, rows[0].Target)
		}
		var view map[string]any
		if err := json.Unmarshal(rows[0].After, &view); err != nil {
			t.Fatal(err)
		}
		banned := []string{"password_hash", "totp_secret", "password"}
		if action != ActionEnrollTOTP {
			banned = append(banned, "totp_staged")
		}
		for _, key := range banned {
			if v, ok := view[key]; ok {
				t.Fatalf("%s audit after carries %q: %v", action, key, v)
			}
		}
		if action == ActionEnrollTOTP && view["totp_staged"] != true {
			t.Fatalf("enroll after should flag staging, got %v", view)
		}
	}
	// Rotation shape: must_rotate flipped true → false.
	rotateRow := s.auditRows(t, ActionRotatePassword)[0]
	var before, after map[string]any
	if err := json.Unmarshal(rotateRow.Before, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rotateRow.After, &after); err != nil {
		t.Fatal(err)
	}
	if before["must_rotate"] != true || after["must_rotate"] != false {
		t.Fatalf("rotate before/after must_rotate: %v → %v", before["must_rotate"], after["must_rotate"])
	}
}

func TestLoginRequiresTOTPAndLocksAfterFiveFailures(t *testing.T) {
	s := newStack(t)
	p := s.fullyProvisioned(t, "locked@it.test")

	// No code → refused. Wrong code → refused. Correct → in.
	if status, _, _ := p.login(t, s, ""); status != http.StatusUnauthorized {
		t.Fatalf("login without code: %d", status)
	}
	if status, _, _ := p.login(t, s, "000000"); status != http.StatusUnauthorized {
		t.Fatalf("login with wrong code: %d", status)
	}
	good, err := totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}
	status, resp, raw := p.login(t, s, good)
	if status != http.StatusOK {
		t.Fatalf("login with valid code: %d %s", status, raw)
	}
	_ = sessionCookieOf(t, resp)

	// 5 wrong passwords record 5 failures; the 6th attempt — even with
	// fully correct credentials — is refused by the lock.
	for i := range LockoutThreshold {
		body := fmt.Sprintf(`{"email":%q,"password":"definitely-wrong-%d","code":%q}`, p.Email, i, good)
		if status, _, _ := s.post(t, "/api/login", body, nil); status != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, status)
		}
	}
	status, _, raw = p.login(t, s, good)
	if status != http.StatusTooManyRequests {
		t.Fatalf("locked attempt with correct credentials: %d %s", status, raw)
	}

	// The lockout is audited with actor + origin IP.
	rows := s.auditRows(t, ActionLockout)
	if len(rows) != 1 {
		t.Fatalf("want 1 login:lockout row, got %d", len(rows))
	}
	if rows[0].Actor != p.Email || rows[0].OriginIP != "127.0.0.1" {
		t.Fatalf("lockout row actor=%q ip=%q", rows[0].Actor, rows[0].OriginIP)
	}

	// Failures are audited too — every authentication event is.
	if failed := s.auditRows(t, ActionLoginFailed); len(failed) < LockoutThreshold {
		t.Fatalf("want >= %d login:failed rows, got %d", LockoutThreshold, len(failed))
	}

	// The lock lifts when the window slides out (test window 1.5s).
	time.Sleep(s.env.LockoutWindow + 300*time.Millisecond)
	good, err = totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}
	status, resp, raw = p.login(t, s, good)
	if status != http.StatusOK {
		t.Fatalf("login after lock window: %d %s", status, raw)
	}
	_ = sessionCookieOf(t, resp)
}

// TestEmployeeLoginConsumesTOTPStep pins login code consumption (audit
// issue #103): the first login with a code passes, logging in again
// TestEmployeeLoginTOTPReplayProtection pins the monotonic consumption
// semantics (#103): the same code verifies again while its step is
// current (two legitimate actions inside one 30-second step present the
// same code), and a code from an older step than any already used is a
// replay after the window moved on — refused exactly like a wrong code.
// The window advance is simulated deterministically by bumping the
// marker past the used step (no sleeps).
func TestEmployeeLoginTOTPReplayProtection(t *testing.T) {
	s := newStack(t)
	p := s.fullyProvisioned(t, "replay@it.test")

	code, err := totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}
	status, resp, raw := p.login(t, s, code)
	if status != http.StatusOK {
		t.Fatalf("first login with code: %d %s", status, raw)
	}
	_ = sessionCookieOf(t, resp)

	// Same step, same code: a second legitimate action — allowed.
	if status, _, raw := p.login(t, s, code); status != http.StatusOK {
		t.Fatalf("same-step second login: %d %s, want 200", status, raw)
	}

	// The window slides past the used step: the same code is now a
	// replay of an older step and must be refused.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE employees SET last_used_step = last_used_step + 1 WHERE email = $1`, p.Email); err != nil {
		t.Fatal(err)
	}
	if status, _, raw := p.login(t, s, code); status != http.StatusUnauthorized {
		t.Fatalf("older-step replayed code login: %d %s, want 401", status, raw)
	}
}

func TestSessionExpiresIdleAndAbsolute(t *testing.T) {
	s := newStack(t)
	p := s.fullyProvisioned(t, "expiry@it.test")

	// Idle expiry is TestIdleExpirySlidesFromLastUse; this test pins the
	// absolute cap: created_at + 12h is the hard limit even when the
	// session was used a second ago.
	good, err := totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}
	status, resp, _ := p.login(t, s, good)
	cookie := sessionCookieOf(t, resp)
	status, _, _ = s.do(t, "GET", "/api/me", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("fresh session: %d", status)
	}

	s.shiftSessions(t, `created_at = now() - make_interval(secs => 43201), last_used_at = now()`)
	status, _, _ = s.do(t, "GET", "/api/me", "", cookie)
	if status != http.StatusUnauthorized {
		t.Fatalf("absolutely expired session: %d", status)
	}

	// A fresh login still works (the shift expired the session, not the
	// surface).
	good, err = totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := p.login(t, s, good); status != http.StatusOK {
		t.Fatalf("login after expiry test: %d", status)
	}
}

func TestIdleExpirySlidesFromLastUse(t *testing.T) {
	s := newStack(t)
	p := s.fullyProvisioned(t, "idle@it.test")
	good, err := totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := p.login(t, s, good)
	cookie := sessionCookieOf(t, resp)

	// 29 minutes idle: still inside the 30-min window.
	s.shiftSessions(t, `last_used_at = now() - make_interval(secs => 1740)`)
	if status, _, _ := s.do(t, "GET", "/api/me", "", cookie); status != http.StatusOK {
		t.Fatalf("session inside idle window: %d", status)
	}

	// 31 minutes idle: dead.
	s.shiftSessions(t, `last_used_at = now() - make_interval(secs => 1860)`)
	if status, _, _ := s.do(t, "GET", "/api/me", "", cookie); status != http.StatusUnauthorized {
		t.Fatalf("session past idle window: %d", status)
	}
}

func TestLogoutRevokesAndAudits(t *testing.T) {
	s := newStack(t)
	p := s.fullyProvisioned(t, "logout@it.test")
	good, err := totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}
	status, resp, _ := p.login(t, s, good)
	cookie := sessionCookieOf(t, resp)

	status, _, raw := s.post(t, "/api/logout", `{}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("logout: %d %s", status, raw)
	}
	if status, _, _ := s.do(t, "GET", "/api/me", "", cookie); status != http.StatusUnauthorized {
		t.Fatalf("cookie after logout: %d", status)
	}

	rows := s.auditRows(t, ActionLogout)
	if len(rows) != 1 {
		t.Fatalf("want 1 logout row, got %d", len(rows))
	}
	if rows[0].Actor != p.Email || rows[0].Target == "" {
		t.Fatalf("logout row actor=%q target=%q", rows[0].Actor, rows[0].Target)
	}
}

func TestLoginAuditsActorAndForwardedOriginIP(t *testing.T) {
	s := newStack(t)
	p := s.fullyProvisioned(t, "xff@it.test")
	good, err := totpNow(p.TOTPSecr)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", s.rest.URL+"/api/login",
		bytes.NewBufferString(fmt.Sprintf(`{"email":%q,"password":%q,"code":%q}`, p.Email, p.Password, good)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	resp, err := s.rest.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("xff login: %d", resp.StatusCode)
	}

	// The provisioning journey logged earlier successful logins too;
	// the row under test is the one this request originated.
	var row *auditRow
	rows := s.auditRows(t, ActionLogin)
	for i := range rows {
		if rows[i].OriginIP == "203.0.113.7" {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("no login row from the XFF origin among %d", len(rows))
	}
	if row.Actor != p.Email {
		t.Fatalf("login row actor=%q", row.Actor)
	}
}
