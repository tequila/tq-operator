package report

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
)

// The token exchange (RFC 8693): the projected ServiceAccount token as a federated subject.
const (
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: an RFC 8693 URN
	tokenTypeJWT           = "urn:ietf:params:oauth:token-type:jwt"            //nolint:gosec // G101: an RFC 8693 URN
	// Audience is the console's audience at IAM.
	Audience = "tenancy"
	// tokenRefreshMargin: a cached token is re-exchanged this long before it expires.
	tokenRefreshMargin = 60 * time.Second
	// maxAnswerBytes caps what is read of any answer.
	maxAnswerBytes = 64 * 1024
	// maxTokenFileBytes caps the projected token file.
	maxTokenFileBytes = 16 * 1024
)

// Bounds of every request: connect 10 s, total 30 s — a hung console or IAM never stalls a
// reconcile.
const (
	ConnectTimeout = 10 * time.Second
	TotalTimeout   = 30 * time.Second
)

// NewHTTPClient is the operator's one HTTP client: TLS verified against the system roots, no
// proxy taken from the environment (the operator speaks to its two configured hosts and no
// third), redirects never followed (a redirect to another host would be a third), bounded.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: TotalTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   ConnectTimeout,
			ResponseHeaderTimeout: TotalTimeout,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Identity is who the estate is to Tequila: the exchanged token's sub estate:<account>/<env>.
type Identity struct {
	Account     string
	Environment string
}

// Subject renders the identity as IAM's subject.
func (i Identity) Subject() string { return "estate:" + i.Account + "/" + i.Environment }

// Result is how one attempt ended — the outcome, the HTTP status when something answered, the
// error code of the answer, and a message fit for a condition (never a token, never a body).
type Result struct {
	Outcome    estatev1alpha1.ReportOutcome
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

// The outcomes (status.lastReport.outcome).
const (
	OutcomeAccepted    estatev1alpha1.ReportOutcome = "accepted"
	OutcomeUnreachable estatev1alpha1.ReportOutcome = "unreachable"
	OutcomeUnbound     estatev1alpha1.ReportOutcome = "unbound"
	OutcomeRefused     estatev1alpha1.ReportOutcome = "refused"
	OutcomeInvalid     estatev1alpha1.ReportOutcome = "invalid"
)

// Reason maps an outcome to the Reported condition's reason.
func (r Result) Reason() string {
	switch r.Outcome {
	case OutcomeAccepted:
		return "Accepted"
	case OutcomeUnreachable:
		return "Unreachable"
	case OutcomeUnbound:
		return "Unbound"
	default:
		return "Refused"
	}
}

// Client exchanges the projected token at IAM and puts reports at the console.
type Client struct {
	IAM       *url.URL
	Console   *url.URL
	TokenFile string
	HTTP      *http.Client
	Now       func() time.Time

	mu       sync.Mutex
	token    string
	expires  time.Time
	identity Identity
}

// Identity returns the estate identity and a token for the console, exchanging the projected
// ServiceAccount token at IAM when no cached token has more than a minute left.
func (c *Client) Identity(ctx context.Context) (Identity, string, *Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Add(tokenRefreshMargin).Before(c.expires) {
		return c.identity, c.token, nil
	}
	c.token, c.expires, c.identity = "", time.Time{}, Identity{}

	subject, err := readTokenFile(c.TokenFile)
	if err != nil {
		return Identity{}, "", &Result{Outcome: OutcomeRefused, Code: "no_projected_token", Message: err.Error()}
	}
	form := url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {subject},
		"subject_token_type": {tokenTypeJWT},
		"audience":           {Audience},
	}
	endpoint := c.IAM.JoinPath("oauth", "token")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, "", &Result{Outcome: OutcomeRefused, Code: "bad_request", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Identity{}, "", &Result{Outcome: OutcomeUnreachable, Message: "IAM: " + transportError(err)}
	}
	defer drain(resp)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))

	if resp.StatusCode == http.StatusOK {
		var answer struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &answer); err != nil || answer.AccessToken == "" {
			return Identity{}, "", &Result{Outcome: OutcomeRefused, Status: resp.StatusCode, Code: "invalid_answer", Message: "IAM answered 200 without an access token"}
		}
		id, exp, err := parseEstateToken(answer.AccessToken)
		if err != nil {
			return Identity{}, "", &Result{Outcome: OutcomeRefused, Status: resp.StatusCode, Code: "unexpected_subject", Message: "IAM: " + err.Error()}
		}
		expires := now.Add(time.Duration(answer.ExpiresIn) * time.Second)
		if answer.ExpiresIn <= 0 || (!exp.IsZero() && exp.Before(expires)) {
			expires = exp
		}
		c.token, c.expires, c.identity = answer.AccessToken, expires, id
		return id, c.token, nil
	}

	code, description := oauthError(body)
	r := &Result{Status: resp.StatusCode, Code: code, RetryAfter: retryAfter(resp)}
	switch {
	case code == "invalid_grant":
		r.Outcome = OutcomeUnbound
		r.Message = "IAM refused the estate's token (invalid_grant): no federation binding admits it"
		if description != "" {
			r.Message += " — " + description
		}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		r.Outcome = OutcomeUnreachable
		r.Message = fmt.Sprintf("IAM answered %d %s", resp.StatusCode, code)
	default:
		r.Outcome = OutcomeRefused
		r.Message = fmt.Sprintf("IAM answered %d %s", resp.StatusCode, code)
		if description != "" {
			r.Message += " — " + description
		}
	}
	r.Message = strings.TrimSpace(r.Message)
	return Identity{}, "", r
}

// Put sends one report to the console's report door.
func (c *Client) Put(ctx context.Context, id Identity, token string, body []byte) Result {
	endpoint := c.Console.JoinPath("api", "v1", "estates", id.Account, id.Environment, "report")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Result{Outcome: OutcomeRefused, Code: "bad_request", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{Outcome: OutcomeUnreachable, Message: "console: " + transportError(err)}
	}
	defer drain(resp)
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	code := problemCode(answer)
	r := Result{Status: resp.StatusCode, Code: code, RetryAfter: retryAfter(resp)}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		r.Outcome = OutcomeAccepted
		r.Message = "the console accepted the report"
	case resp.StatusCode == http.StatusUnauthorized:
		c.forgetToken()
		r.Outcome = OutcomeUnbound
		r.Message = "the console answered 401: it does not accept the estate's IAM token"
	case resp.StatusCode == http.StatusRequestEntityTooLarge || resp.StatusCode == http.StatusUnprocessableEntity:
		r.Outcome = OutcomeInvalid
		r.Message = fmt.Sprintf("the console answered %d %s", resp.StatusCode, code)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		r.Outcome = OutcomeUnreachable
		r.Message = fmt.Sprintf("the console answered %d %s", resp.StatusCode, code)
	default:
		r.Outcome = OutcomeRefused
		r.Message = fmt.Sprintf("the console answered %d %s", resp.StatusCode, code)
	}
	r.Message = strings.TrimSpace(r.Message)
	return r
}

func (c *Client) forgetToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.expires, c.identity = "", time.Time{}, Identity{}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func readTokenFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("no projected ServiceAccount token at %s", path)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes))
	if err != nil {
		return "", fmt.Errorf("the projected token at %s cannot be read", path)
	}
	token := strings.TrimSpace(string(raw))
	if strings.Count(token, ".") != 2 {
		return "", fmt.Errorf("the file at %s is not a JWT", path)
	}
	return token, nil
}

// parseEstateToken reads the exchanged token's sub and exp. The token is IAM's and the
// console verifies it; the operator only reads where to address the report.
func parseEstateToken(token string) (Identity, time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, time.Time{}, errors.New("the access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, time.Time{}, errors.New("the access token's payload is not base64url")
	}
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, time.Time{}, errors.New("the access token's payload is not JSON")
	}
	id, err := ParseSubject(claims.Sub)
	if err != nil {
		return Identity{}, time.Time{}, err
	}
	var exp time.Time
	if claims.Exp > 0 {
		exp = time.Unix(claims.Exp, 0)
	}
	return id, exp, nil
}

// ParseSubject parses estate:<account>/<environment>.
func ParseSubject(sub string) (Identity, error) {
	rest, ok := strings.CutPrefix(sub, "estate:")
	account, env, found := strings.Cut(rest, "/")
	if !ok || !found || !validSegment(account) || !validSegment(env) {
		return Identity{}, fmt.Errorf("the token's subject is %q, not estate:<account>/<environment>", truncate(sub, 80))
	}
	return Identity{Account: account, Environment: env}, nil
}

func validSegment(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func oauthError(body []byte) (code, description string) {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "", ""
	}
	return truncate(e.Error, 64), truncate(e.Description, 200)
}

// problemCode reads an answer's error code: `error` or `code`, else the last segment of an RFC
// 7807 `type`.
func problemCode(body []byte) string {
	var p struct {
		Error string `json:"error"`
		Code  string `json:"code"`
		Type  string `json:"type"`
	}
	if json.Unmarshal(body, &p) != nil {
		return ""
	}
	switch {
	case p.Error != "":
		return truncate(p.Error, 64)
	case p.Code != "":
		return truncate(p.Code, 64)
	case p.Type != "" && p.Type != "about:blank":
		return truncate(p.Type[strings.LastIndex(p.Type, "/")+1:], 64)
	}
	return ""
}

func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func transportError(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the name does not resolve"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "the connection failed (" + opErr.Op + ")"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "the TLS certificate does not verify"
	}
	return truncate(err.Error(), 160)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerBytes))
	_ = resp.Body.Close()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
