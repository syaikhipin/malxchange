package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/syaikhipin/malxchange/internal/auth"
)

// The Code channel surface's always-on coverage (ticket #62): the api
// client's error mapping and the Step-up gate on the credential save,
// exercised against handler with no database. The served-surface
// journeys (permissions, audit rows, write-only rule over HTTP) live
// in channels_it_test.go under the integration tag.

// newChannelTestServer wires a Server whose api dependency points at
// the given stub URL, with no pool: the channel handlers never touch
// the admin schema EXCEPT step-up consumption (#103), which is exactly
// why the valid-code journey lives in channels_it_test.go (real pool)
// and these unit tests pin the refusals and the fail-closed rule.
func newChannelTestServer(apiBase string) *Server {
	env := &AdminEnv{
		SessionAbsolute: 12 * time.Hour,
		SessionIdle:     30 * time.Minute,
		APIBaseURL:      apiBase,
		APITimeout:      2 * time.Second,
	}
	return &Server{env: env, api: newAPIClient(env)}
}

// withEmployee plants the session-gate context the handlers read —
// the middleware's job in production.
func withEmployee(ctx context.Context, e Employee) context.Context {
	return context.WithValue(ctx, ctxEmployee{}, e)
}

func TestChannelClientMapsUpstreamAnswers(t *testing.T) {
	var sawActor, sawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawActor = r.Header.Get("X-Mal-Actor")
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			sawBody = string(raw)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/channels":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"channel":"sms","active":true,"activatedAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/channels/sms/activate":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"channel sms needs credentials endpoint and apiKey"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := newChannelTestServer(srv.URL)

	// A list call decodes the array.
	var states []channelStatus
	if err := s.api.call(context.Background(), http.MethodGet, "/channels", "", nil, &states); err != nil {
		t.Fatalf("list call: %v", err)
	}
	if len(states) != 1 || states[0].Channel != "sms" || !states[0].Active {
		t.Fatalf("unexpected states: %+v", states)
	}

	// A mutating call forwards the actor header and no step-up code.
	in := map[string]json.RawMessage{"credentials": json.RawMessage(`{"endpoint":"http://sms.local/send","apiKey":"k1"}`)}
	err := s.api.call(context.Background(), http.MethodPost, "/channels/sms/activate", "root@it.test", in, nil)
	if err == nil {
		t.Fatal("expected the 400 to surface as an error")
	}
	var upstream *apiUpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("expected *apiUpstreamError, got %T: %v", err, err)
	}
	if upstream.Status != http.StatusBadRequest || upstream.Message != "channel sms needs credentials endpoint and apiKey" {
		t.Fatalf("upstream error = %+v", upstream)
	}
	if sawActor != "root@it.test" {
		t.Fatalf("X-Mal-Actor = %q", sawActor)
	}
	if !strings.Contains(sawBody, `"apiKey":"k1"`) || strings.Contains(sawBody, "code") {
		t.Fatalf("forwarded body must carry the credentials and not the step-up code: %s", sawBody)
	}

	// A transport failure is errAPIDown, not an upstream answer.
	dead := newChannelTestServer("http://127.0.0.1:1")
	if err := dead.api.call(context.Background(), http.MethodGet, "/channels", "", nil, &states); !errors.Is(err, errAPIDown) {
		t.Fatalf("dead upstream err = %v, want errAPIDown", err)
	}
}

func TestChannelActivateHandlerDemandsStepUp(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s := newChannelTestServer(srv.URL)

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	emp := Employee{ID: "e-1", Email: "root@it.test", TOTPSecret: secret}
	activate := func(body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/channels/sms/activate", strings.NewReader(body))
		req.SetPathValue("name", "sms")
		req = req.WithContext(withEmployee(req.Context(), emp))
		rec := httptest.NewRecorder()
		s.handleActivateChannel(rec, req)
		return rec.Code, rec.Body.String()
	}

	// No code: the refusal is step-up's, before api is ever called.
	status, raw := activate(`{"credentials":{"endpoint":"http://sms.local/send","apiKey":"k1"}}`)
	if status != http.StatusUnauthorized || !strings.Contains(raw, errStepUpRequired) {
		t.Fatalf("missing code: %d %s", status, raw)
	}
	// A wrong code: same fate.
	status, raw = activate(`{"credentials":{"endpoint":"http://sms.local/send","apiKey":"k1"},"code":"000000"}`)
	if status != http.StatusUnauthorized || !strings.Contains(raw, errStepUpInvalid) {
		t.Fatalf("wrong code: %d %s", status, raw)
	}
	if calls != 0 {
		t.Fatalf("refused activations reached api %d times", calls)
	}

	// The valid-code path now records consumption before reaching api
	// (#103), so it needs a real pool: the journey lives in
	// channels_it_test.go (TestChannelActivateForwardsCredentialsAndAudits),
	// and the no-storage fail-closed rule in
	// TestChannelActivateFailsClosedWithoutStorage below.
}

func TestChannelActivateFailsClosedWithoutStorage(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/channels/sms/activate":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"channel sms needs credentials endpoint and apiKey"}`))
		case "/channels/pigeon/activate":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":` + `"unknown code channel: \"pigeon\""` + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newChannelTestServer(srv.URL)

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	emp := Employee{ID: "e-1", Email: "root@it.test", TOTPSecret: secret}
	call := func(name, body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/channels/"+name+"/activate", strings.NewReader(body))
		req.SetPathValue("name", name)
		req = req.WithContext(withEmployee(req.Context(), emp))
		rec := httptest.NewRecorder()
		s.handleActivateChannel(rec, req)
		return rec.Code, rec.Body.String()
	}

	// Step-up consumption (#103) needs storage: even a valid code must
	// refuse when the step cannot be recorded (fail closed), and the
	// api is never reached. The upstream-refusal passthrough this test
	// used to pin needs the real pool and lives in channels_it_test.go
	// (TestChannelActivateSurfacesAPIRefusal).
	status, raw := call("sms", `{"credentials":{"apiKey":"k1"},"code":"`+code+`"}`)
	if status != http.StatusUnauthorized || !strings.Contains(raw, errStepUpInvalid) {
		t.Fatalf("fail-closed step-up: %d %s", status, raw)
	}
	if calls != 0 {
		t.Fatalf("activation reached api %d times without storage", calls)
	}
}
