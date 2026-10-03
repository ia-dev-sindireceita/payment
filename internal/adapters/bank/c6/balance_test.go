package c6

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// balTestServer is a C6 + OAuth2 double exposing the /v1/balance endpoint so the
// balance read path can be exercised.
type balTestServer struct {
	*httptest.Server
	lastAuthHeader string
	statusCode     int
	body           string
}

func newBalTestServer(t *testing.T) *balTestServer {
	t.Helper()
	ts := &balTestServer{statusCode: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-` + user + `","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/v1/balance", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		ts.lastAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ts.statusCode)
		_, _ = w.Write([]byte(ts.body))
	})
	ts.Server = httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func (ts *balTestServer) provider(t *testing.T, creds ports.CredentialStore) *Provider {
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

func TestGetBalanceSuccess(t *testing.T) {
	t.Parallel()
	ts := newBalTestServer(t)
	ts.body = `{"available_cents":150000,"blocked_cents":2500,"currency":"BRL","as_of":"2026-10-02T12:00:00Z"}`
	p := ts.provider(t, oneTenant("t1", "client-1", "s"))

	bal, err := p.GetBalance(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if ts.lastAuthHeader != "Bearer tok-client-1" {
		t.Fatalf("per-tenant bearer not attached: %q", ts.lastAuthHeader)
	}
	if bal.AvailableCents != 150000 || bal.BlockedCents != 2500 || bal.Currency != "BRL" {
		t.Fatalf("unexpected balance: %+v", bal)
	}
	if bal.AsOf.IsZero() {
		t.Fatalf("as_of not parsed: %+v", bal)
	}
}

func TestGetBalanceUpstreamError(t *testing.T) {
	t.Parallel()
	ts := newBalTestServer(t)
	ts.statusCode = http.StatusInternalServerError
	ts.body = `{}`
	p := ts.provider(t, oneTenant("t1", "c", "s"))

	if _, err := p.GetBalance(context.Background(), "t1"); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable on 5xx, got %v", err)
	}
}

func TestGetBalanceMalformedBody(t *testing.T) {
	t.Parallel()
	ts := newBalTestServer(t)
	ts.body = `not-json`
	p := ts.provider(t, oneTenant("t1", "c", "s"))

	if _, err := p.GetBalance(context.Background(), "t1"); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable on malformed body, got %v", err)
	}
}

func TestGetBalanceUnknownTenantCredential(t *testing.T) {
	t.Parallel()
	ts := newBalTestServer(t)
	ts.body = `{"available_cents":0,"blocked_cents":0,"currency":"BRL","as_of":"2026-10-02T12:00:00Z"}`
	p := ts.provider(t, oneTenant("t1", "c", "s"))

	// A tenant with no credential cannot obtain a token → not-found from the store.
	if _, err := p.GetBalance(context.Background(), "other"); err == nil {
		t.Fatal("missing credential must error (isolation)")
	}
}
