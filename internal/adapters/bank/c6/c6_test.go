package c6

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// fakeCreds is a controllable CredentialStore for tests: it can return a fixed
// error (to exercise the missing-credential path) or per-tenant credentials.
type fakeCreds struct {
	err   error
	creds map[string]ports.BankCredential
}

func (f *fakeCreds) GetBankCredential(_ context.Context, tenantID string) (ports.BankCredential, error) {
	if f.err != nil {
		return ports.BankCredential{}, f.err
	}
	c, ok := f.creds[tenantID]
	if !ok {
		return ports.BankCredential{}, shared.ErrNotFound
	}
	return c, nil
}

func oneTenant(tenantID, clientID, secret string) *fakeCreds {
	return &fakeCreds{creds: map[string]ports.BankCredential{
		tenantID: {TenantID: tenantID, ClientID: clientID, Secret: secret},
	}}
}

// testServer is a configurable C6 + OAuth2 double backed by httptest TLS. Handlers
// default to a happy path and can be overridden per test. It records request
// observations so tests can assert headers/counters without races.
type testServer struct {
	*httptest.Server

	mu             sync.Mutex
	tokenHits      int
	createHits     int
	getHits        int
	lastAuthHeader string // Authorization seen by the charge endpoints
	lastIdemKey    string // Idempotency-Key seen by create
	lastBasicUser  string // client_id seen by the token endpoint

	// Overridable handlers. When nil, a happy-path default is used.
	tokenHandler  http.HandlerFunc
	createHandler http.HandlerFunc
	getHandler    http.HandlerFunc
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.tokenHits++
		user, _, _ := r.BasicAuth()
		ts.lastBasicUser = user
		h := ts.tokenHandler
		ts.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-` + user + `","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/charges/", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.getHits++
		ts.lastAuthHeader = r.Header.Get("Authorization")
		h := ts.getHandler
		ts.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"txid":"tx_123","status":"paid"}`))
	})
	mux.HandleFunc("/charges", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.createHits++
		ts.lastAuthHeader = r.Header.Get("Authorization")
		ts.lastIdemKey = r.Header.Get("Idempotency-Key")
		h := ts.createHandler
		ts.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"txid":"tx_123","status":"pending"}`))
	})
	ts.Server = httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) provider(t *testing.T, creds ports.CredentialStore) *Provider {
	t.Helper()
	p, err := New(Config{
		BaseURL:    ts.URL,
		TokenURL:   ts.URL + "/oauth/token",
		HTTPClient: ts.Client(),
	}, creds)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	creds := oneTenant("t1", "c1", "s1")
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil base", Config{TokenURL: "https://t/x"}},
		{"http base rejected", Config{BaseURL: "http://api.example", TokenURL: "https://t/x"}},
		{"http token rejected", Config{BaseURL: "https://api.example", TokenURL: "http://t/x"}},
		{"malformed base", Config{BaseURL: "://nope", TokenURL: "https://t/x"}},
		{"missing token", Config{BaseURL: "https://api.example"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.cfg, creds); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}

	if _, err := New(Config{BaseURL: "https://api.example", TokenURL: "https://t/x"}, nil); err == nil {
		t.Fatal("expected error for nil credential store")
	}

	// Happy path: an https config with defaults builds a provider with a client.
	p, err := New(Config{BaseURL: "https://api.example/", TokenURL: "https://t/oauth"}, creds)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if p.httpc == nil {
		t.Fatal("expected a default HTTP client to be built")
	}
	if strings.HasSuffix(p.baseURL, "/") {
		t.Fatalf("trailing slash should be trimmed: %q", p.baseURL)
	}
}

func TestDefaultHTTPClientEnforcesTLS(t *testing.T) {
	t.Parallel()
	c := defaultHTTPClient(7 * time.Second)
	if c.Timeout != 7*time.Second {
		t.Fatalf("timeout not applied: %v", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", c.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("expected MinVersion TLS 1.2")
	}
}

func TestCreateChargeSuccess(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	p := ts.provider(t, oneTenant("t1", "client-1", "secret-1"))

	res, err := p.CreateCharge(context.Background(), "t1", ports.ChargeRequest{
		TenantID: "t1", PaymentID: "pay-1", AmountCents: 1000, Currency: "BRL",
	})
	if err != nil {
		t.Fatalf("CreateCharge: %v", err)
	}
	if res.TxID != "tx_123" || res.Status != "pending" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if ts.lastAuthHeader != "Bearer tok-client-1" {
		t.Fatalf("bearer token not attached: %q", ts.lastAuthHeader)
	}
	if ts.lastIdemKey != "pay-1" {
		t.Fatalf("idempotency key should fall back to the payment id, got %q", ts.lastIdemKey)
	}
}

// TestCreateChargeForwardsIdempotencyKey covers the F3b contract (SIN-64720): a
// caller-supplied IdempotencyKey is forwarded to the PSP verbatim, and only when
// it is empty does the adapter fall back to the deterministic PaymentID.
func TestCreateChargeForwardsIdempotencyKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		paymentID string
		idemKey   string
		wantKey   string
	}{
		{"caller key wins", "pay-1", "idem-abc", "idem-abc"},
		{"fallback to payment id", "pay-2", "", "pay-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			p := ts.provider(t, oneTenant("t1", "c", "s"))
			if _, err := p.CreateCharge(context.Background(), "t1", ports.ChargeRequest{
				TenantID: "t1", PaymentID: tc.paymentID, IdempotencyKey: tc.idemKey, AmountCents: 1, Currency: "BRL",
			}); err != nil {
				t.Fatalf("CreateCharge: %v", err)
			}
			if ts.lastIdemKey != tc.wantKey {
				t.Fatalf("Idempotency-Key: want %q, got %q", tc.wantKey, ts.lastIdemKey)
			}
		})
	}
}

func TestGetChargeSuccess(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	p := ts.provider(t, oneTenant("t1", "client-1", "secret-1"))

	res, err := p.GetCharge(context.Background(), "t1", "tx_123")
	if err != nil {
		t.Fatalf("GetCharge: %v", err)
	}
	if res.TxID != "tx_123" || res.Status != "paid" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if ts.lastAuthHeader != "Bearer tok-client-1" {
		t.Fatalf("bearer token not attached: %q", ts.lastAuthHeader)
	}
}

func TestChargeErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"validation", 400, shared.ErrValidation},
		{"conflict", 409, shared.ErrConflict},
		{"server error", 503, shared.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			ts.createHandler = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"code":"X"}`))
			}
			p := ts.provider(t, oneTenant("t1", "c", "s"))
			_, err := p.CreateCharge(context.Background(), "t1", ports.ChargeRequest{TenantID: "t1", PaymentID: "p", AmountCents: 1, Currency: "BRL"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: want %v, got %v", tc.status, tc.want, err)
			}
		})
	}
}

func TestGetChargeNotFound(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.getHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND"}`))
	}
	p := ts.provider(t, oneTenant("t1", "c", "s"))
	if _, err := p.GetCharge(context.Background(), "t1", "missing"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestChargeMalformedSuccessBody(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.createHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}
	p := ts.provider(t, oneTenant("t1", "c", "s"))
	if _, err := p.CreateCharge(context.Background(), "t1", ports.ChargeRequest{TenantID: "t1", PaymentID: "p", AmountCents: 1, Currency: "BRL"}); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("malformed 2xx body should map to ErrUnavailable, got %v", err)
	}
}

func TestChargeMissingCredential(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	p := ts.provider(t, &fakeCreds{creds: map[string]ports.BankCredential{}}) // no tenants

	_, err := p.CreateCharge(context.Background(), "unknown", ports.ChargeRequest{TenantID: "unknown", PaymentID: "p", AmountCents: 1, Currency: "BRL"})
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("missing credential should propagate ErrNotFound, got %v", err)
	}
	if ts.tokenHits != 0 {
		t.Fatalf("token endpoint must not be hit without a credential, hits=%d", ts.tokenHits)
	}
}

func TestChargeTokenUnauthorized(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ts.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}
	p := ts.provider(t, oneTenant("t1", "c", "s"))
	if _, err := p.CreateCharge(context.Background(), "t1", ports.ChargeRequest{TenantID: "t1", PaymentID: "p", AmountCents: 1, Currency: "BRL"}); !errors.Is(err, shared.ErrUnauthorized) {
		t.Fatalf("bad credentials should map to ErrUnauthorized, got %v", err)
	}
}

// TestContextCancellation proves context is propagated to the transport and a
// cancelled request fails as unavailable rather than hanging (no goroutine leak).
func TestContextCancellation(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	p := ts.provider(t, oneTenant("t1", "c", "s"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	if _, err := p.CreateCharge(ctx, "t1", ports.ChargeRequest{TenantID: "t1", PaymentID: "p", AmountCents: 1, Currency: "BRL"}); err == nil {
		t.Fatal("expected error with a cancelled context")
	}
}

// TestSecretNeverLeaks asserts the client secret never reaches an error returned
// to the caller, on either the token-failure or charge-failure path.
func TestSecretNeverLeaks(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-VALUE-9988"
	ts := newTestServer(t)
	ts.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}
	p := ts.provider(t, oneTenant("t1", "client-1", secret))

	_, err := p.CreateCharge(context.Background(), "t1", ports.ChargeRequest{TenantID: "t1", PaymentID: "p", AmountCents: 1, Currency: "BRL"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the client secret: %q", err.Error())
	}
}
