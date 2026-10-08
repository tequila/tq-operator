package report_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tequila/tq-operator/internal/report"
)

const projectedToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6dHEtb3BlcmF0b3I6dHEtb3BlcmF0b3IifQ.c2lnbmF0dXJl"

func iamToken(sub string, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"sub": sub, "exp": exp.Unix(), "aud": "tenancy"})
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// fakeTequila is IAM and the console in one TLS server, with switchable answers.
type fakeTequila struct {
	t *testing.T
	*httptest.Server

	mu            sync.Mutex
	exchanges     int
	puts          int
	lastForm      url.Values
	lastAuth      string
	lastPath      string
	lastBody      []byte
	tokenAnswer   func(w http.ResponseWriter)
	consoleAnswer func(w http.ResponseWriter)
	issuedSubject string
}

func newFake(t *testing.T) *fakeTequila {
	f := &fakeTequila{t: t, issuedSubject: "estate:acme/prod"}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTequila) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
		f.exchanges++
		_ = r.ParseForm()
		f.lastForm = r.PostForm
		if f.tokenAnswer != nil {
			f.tokenAnswer(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": iamToken(f.issuedSubject, time.Now().Add(10*time.Minute)), "token_type": "Bearer",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 600,
		})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/estates/"):
		f.puts++
		f.lastAuth = r.Header.Get("Authorization")
		f.lastPath = r.URL.Path
		f.lastBody, _ = io.ReadAll(r.Body)
		if f.consoleAnswer != nil {
			f.consoleAnswer(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"reportId":"0192","receivedAt":"2026-10-05T14:05:00Z"}`))
	default:
		http.NotFound(w, r)
	}
}

func answer(status int, body string, headers ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(projectedToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newReporter(t *testing.T, f *fakeTequila, now *time.Time) *report.Reporter {
	u, _ := url.Parse(f.URL)
	return &report.Reporter{
		Client:  &report.Client{IAM: u, Console: u, TokenFile: tokenFile(t), HTTP: f.Client(), Now: func() time.Time { return *now }},
		Started: *now,
		Now:     func() time.Time { return *now },
	}
}

func payload() report.Report {
	return report.Assemble(nil, minimalStatus(), report.Operator{Version: "0.1.0", Image: "registry.tequila.dev/platform/tq-operator:v0.1.0", Capabilities: nil})
}

func TestAcceptedReportFollowsC1AndC4(t *testing.T) {
	f := newFake(t)
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	r := newReporter(t, f, &now)

	a, next := r.Report(context.Background(), "tq-operator/prod", payload(), "prod", 15*time.Minute)
	if !a.Attempted || a.Result.Outcome != report.OutcomeAccepted || a.Result.Status != 202 || a.Result.Reason() != "Accepted" {
		t.Fatalf("attempt = %+v", a)
	}
	if !next.Equal(now.Add(15 * time.Minute)) {
		t.Errorf("next due %v, want the heartbeat %v", next, now.Add(15*time.Minute))
	}
	// The exchange — the projected token as a federated subject, no client authentication.
	want := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {projectedToken},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
		"audience":           {"tenancy"},
	}
	if fmt.Sprint(f.lastForm) != fmt.Sprint(want) {
		t.Errorf("exchange form = %v, want %v", f.lastForm, want)
	}
	// The door, addressed by the identity the token carries.
	if f.lastPath != "/api/v1/estates/acme/prod/report" {
		t.Errorf("PUT path %q", f.lastPath)
	}
	if !strings.HasPrefix(f.lastAuth, "Bearer ") || strings.Contains(f.lastAuth, projectedToken) {
		t.Errorf("the console must get the IAM token, never the projected one")
	}
	var sent map[string]any
	if err := json.Unmarshal(f.lastBody, &sent); err != nil {
		t.Fatal(err)
	}
	estate := sent["estate"].(map[string]any)
	if estate["account"] != "acme" || estate["environment"] != "prod" || sent["reportedAt"] != "2026-10-05T14:00:00Z" {
		t.Errorf("estate %v reportedAt %v", estate, sent["reportedAt"])
	}
	assertValid(t, f.lastBody)

	// Unchanged and inside the heartbeat: not due, nothing sent.
	now = now.Add(5 * time.Minute)
	if a, _ := r.Report(context.Background(), "tq-operator/prod", payload(), "prod", 15*time.Minute); a.Attempted {
		t.Error("an unchanged report was re-sent before the heartbeat")
	}
	// The heartbeat: re-sent, with the cached token (one exchange so far).
	now = now.Add(10 * time.Minute)
	if a, _ := r.Report(context.Background(), "tq-operator/prod", payload(), "prod", 15*time.Minute); !a.Attempted {
		t.Error("the heartbeat did not re-send")
	}
	// The IAM token lives 600 s: the heartbeat 15 minutes later needs a fresh exchange.
	if f.exchanges != 2 {
		t.Errorf("exchanges = %d, want 2", f.exchanges)
	}
}

func TestInvalidGrantIsUnboundWithEarlyRetryBackoff(t *testing.T) {
	f := newFake(t)
	f.tokenAnswer = answer(400, `{"error":"invalid_grant"}`)
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	r := newReporter(t, f, &now)

	a, next := r.Report(context.Background(), "k", payload(), "", 15*time.Minute)
	if a.Result.Outcome != report.OutcomeUnbound || a.Result.Reason() != "Unbound" || f.puts != 0 {
		t.Fatalf("attempt = %+v, puts %d", a, f.puts)
	}
	if got := next.Sub(now); got != 5*time.Minute {
		t.Errorf("first hour after start: retry in %v, want 5m (the early-retry window)", got)
	}
	// Still unbound after the first hour: one hour.
	now = now.Add(61 * time.Minute)
	_, next = r.Report(context.Background(), "k", payload(), "", 15*time.Minute)
	if got := next.Sub(now); got != time.Hour {
		t.Errorf("after the first hour: retry in %v, want 1h", got)
	}
	// The binding appears: the next attempt is accepted and the backoff is gone.
	f.tokenAnswer = nil
	now = now.Add(time.Hour)
	if a, _ := r.Report(context.Background(), "k", payload(), "", 15*time.Minute); a.Result.Outcome != report.OutcomeAccepted {
		t.Errorf("after the binding: %+v", a.Result)
	}
}

func TestConsole401IsUnboundAndForgetsTheToken(t *testing.T) {
	f := newFake(t)
	f.consoleAnswer = answer(401, `{"error":"unauthorized"}`)
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	r := newReporter(t, f, &now)

	a, _ := r.Report(context.Background(), "k", payload(), "", 15*time.Minute)
	if a.Result.Outcome != report.OutcomeUnbound || a.Result.Status != 401 {
		t.Fatalf("attempt = %+v", a.Result)
	}
	f.consoleAnswer = nil
	now = now.Add(5 * time.Minute)
	if a, _ := r.Report(context.Background(), "k", payload(), "", 15*time.Minute); a.Result.Outcome != report.OutcomeAccepted {
		t.Fatalf("retry: %+v", a.Result)
	}
	if f.exchanges != 2 {
		t.Errorf("exchanges = %d: a 401 must drop the cached token", f.exchanges)
	}
}

func TestUnavailableIsUnreachableWithExponentialBackoff(t *testing.T) {
	f := newFake(t)
	f.tokenAnswer = answer(503, `{"error":"temporarily_unavailable"}`, "Retry-After", "30")
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	r := newReporter(t, f, &now)

	a, next := r.Report(context.Background(), "k", payload(), "", 15*time.Minute)
	if a.Result.Outcome != report.OutcomeUnreachable || a.Result.Status != 503 || a.Result.Reason() != "Unreachable" {
		t.Fatalf("IAM 503: %+v", a.Result)
	}
	if got := next.Sub(now); got != time.Minute {
		t.Errorf("first failure: retry in %v, want 1m", got)
	}

	f.tokenAnswer = nil
	f.consoleAnswer = answer(503, `{"type":"https://tequila.dev/problems/unavailable"}`, "Retry-After", "600")
	var waits []time.Duration
	for range 8 {
		now = next
		a, next = r.Report(context.Background(), "k", payload(), "", 15*time.Minute)
		if a.Result.Outcome != report.OutcomeUnreachable {
			t.Fatalf("console 503: %+v", a.Result)
		}
		waits = append(waits, next.Sub(now))
	}
	want := []time.Duration{10 * time.Minute, 10 * time.Minute, 10 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour, time.Hour}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("backoff %v, want %v (exponential from 1m, Retry-After honoured, capped at 1h)", waits, want)
	}
}

func TestRefusalsAreClassified(t *testing.T) {
	cases := []struct {
		name    string
		token   func(http.ResponseWriter)
		console func(http.ResponseWriter)
		outcome string
		reason  string
		code    string
	}{
		{"estate mismatch", nil, answer(403, `{"error":"estate_mismatch"}`), "refused", "Refused", "estate_mismatch"},
		{"offboarded", nil, answer(410, `{"error":"account_offboarded"}`), "refused", "Refused", "account_offboarded"},
		{"invalid report", nil, answer(422, `{"error":"invalid_report","errors":[{"pointer":"/x","message":"m"}]}`), "invalid", "Refused", "invalid_report"},
		{"too large", nil, answer(413, ``), "invalid", "Refused", ""},
		{"invalid target", answer(400, `{"error":"invalid_target"}`), nil, "refused", "Refused", "invalid_target"},
		{"redirect never followed", nil, answer(307, ``, "Location", "https://example.com/elsewhere"), "refused", "Refused", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.tokenAnswer, f.consoleAnswer = tc.token, tc.console
			now := time.Now()
			a, _ := newReporter(t, f, &now).Report(context.Background(), "k", payload(), "", time.Minute)
			if string(a.Result.Outcome) != tc.outcome || a.Result.Reason() != tc.reason || a.Result.Code != tc.code {
				t.Errorf("got %+v", a.Result)
			}
			if strings.Contains(a.Result.Message, projectedToken) || strings.Contains(a.Result.Message, "eyJ") {
				t.Error("a token reached the message")
			}
		})
	}
}

func TestEnvironmentMismatchIsNeverSent(t *testing.T) {
	f := newFake(t)
	now := time.Now()
	a, _ := newReporter(t, f, &now).Report(context.Background(), "k", payload(), "staging", time.Minute)
	if a.Result.Outcome != report.OutcomeRefused || a.Result.Code != "environment_mismatch" || f.puts != 0 {
		t.Errorf("got %+v, puts %d", a.Result, f.puts)
	}
}

func TestUnexpectedSubjectIsRefused(t *testing.T) {
	f := newFake(t)
	f.issuedSubject = "svc:tenant-acme"
	now := time.Now()
	a, _ := newReporter(t, f, &now).Report(context.Background(), "k", payload(), "", time.Minute)
	if a.Result.Outcome != report.OutcomeRefused || a.Result.Code != "unexpected_subject" || f.puts != 0 {
		t.Errorf("got %+v", a.Result)
	}
}

func TestNothingAnswersIsUnreachable(t *testing.T) {
	f := newFake(t)
	f.Close()
	now := time.Now()
	a, _ := newReporter(t, f, &now).Report(context.Background(), "k", payload(), "", time.Minute)
	if a.Result.Outcome != report.OutcomeUnreachable || a.Result.Status != 0 {
		t.Errorf("got %+v", a.Result)
	}
}

func TestNoProjectedTokenIsRefused(t *testing.T) {
	f := newFake(t)
	now := time.Now()
	r := newReporter(t, f, &now)
	r.Client.TokenFile = filepath.Join(t.TempDir(), "absent")
	a, _ := r.Report(context.Background(), "k", payload(), "", time.Minute)
	if a.Result.Outcome != report.OutcomeRefused || a.Result.Code != "no_projected_token" || f.exchanges != 0 {
		t.Errorf("got %+v", a.Result)
	}
}

func TestHTTPClientIsBounded(t *testing.T) {
	c := report.NewHTTPClient()
	if c.Timeout != 30*time.Second {
		t.Errorf("total timeout %v", c.Timeout)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("the client must not take a proxy from the environment — a proxy is a third host")
	}
	if c.CheckRedirect == nil {
		t.Error("redirects must not be followed")
	}
}

func TestParseSubject(t *testing.T) {
	for sub, ok := range map[string]bool{
		"estate:acme-health/prod": true, "estate:example-org/dev": true,
		"svc:tenant-acme": false, "estate:acme": false, "estate:/prod": false, "estate:acme/prod/x": false, "estate:ACME/prod": false,
	} {
		_, err := report.ParseSubject(sub)
		if (err == nil) != ok {
			t.Errorf("%s: err %v", sub, err)
		}
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := report.Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}
