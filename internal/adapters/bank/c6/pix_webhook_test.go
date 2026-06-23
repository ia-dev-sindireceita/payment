package c6

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// webhookTestServer is a configurable C6 webhook-registration + OAuth2 double backed
// by httptest TLS. It records the last PUT so tests can assert the chave is in the
// path, the URL is only in the body, and the auth header is attached.
type webhookTestServer struct {
	*httptest.Server

	mu          sync.Mutex
	putHits     int
	lastMethod  string
	lastPath    string
	lastAuth    string
	lastIdemKey string
	lastBody    pixWebhookRequestBody

	handler http.HandlerFunc
}

func newWebhookTestServer(t *testing.T) *webhookTestServer {
	t.Helper()
	ts := &webhookTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-` + user + `","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/v2/webhook/", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.putHits++
		ts.lastMethod = r.Method
		ts.lastPath = r.URL.Path
		ts.lastAuth = r.Header.Get("Authorization")
		ts.lastIdemKey = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ts.lastBody)
		h := ts.handler
		ts.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		// Default: empty 200 (no body) — the path doNoContent must tolerate.
		w.WriteHeader(http.StatusOK)
	})
	ts.Server = httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func (ts *webhookTestServer) provider(t *testing.T, creds ports.CredentialStore) *Provider {
	t.Helper()
	p, err := New(Config{
		BaseURL:    ts.URL,
		TokenURL:   ts.URL + "/oauth/token",
		HTTPClient: ts.Client(),
		Now:        func() time.Time { return time.Unix(0, 0) },
	}, creds)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestRegisterPixWebhookSuccess(t *testing.T) {
	t.Parallel()
	ts := newWebhookTestServer(t)
	p := ts.provider(t, oneTenant("t1", "client-1", "secret-1"))

	const url = "https://hooks.acme.example/pix"
	if err := p.RegisterPixWebhook(context.Background(), "t1", "acme-chave", url); err != nil {
		t.Fatalf("RegisterPixWebhook: %v", err)
	}
	if ts.lastMethod != http.MethodPut {
		t.Fatalf("expected PUT, got %s", ts.lastMethod)
	}
	if ts.lastAuth != "Bearer tok-client-1" {
		t.Fatalf("bearer token not attached: %q", ts.lastAuth)
	}
	// The chave is in the path; the webhook URL is NOT (it travels only in the body).
	if !strings.HasSuffix(ts.lastPath, "/v2/webhook/acme-chave") {
		t.Fatalf("chave not in path: %q", ts.lastPath)
	}
	if strings.Contains(ts.lastPath, "hooks.acme.example") {
		t.Fatalf("webhook url leaked into the request path: %q", ts.lastPath)
	}
	if ts.lastBody.WebhookURL != url {
		t.Fatalf("webhook url not sent in body: %q", ts.lastBody.WebhookURL)
	}
	// The chave doubles as the idempotency anchor.
	if ts.lastIdemKey != "acme-chave" {
		t.Fatalf("chave should be forwarded as Idempotency-Key, got %q", ts.lastIdemKey)
	}
}

// The PSP may answer 204 No Content; doNoContent must treat it as success.
func TestRegisterPixWebhookNoContent(t *testing.T) {
	t.Parallel()
	ts := newWebhookTestServer(t)
	ts.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	p := ts.provider(t, oneTenant("t1", "c", "s"))
	if err := p.RegisterPixWebhook(context.Background(), "t1", "chave", "https://hooks.example/pix"); err != nil {
		t.Fatalf("204 should be success, got %v", err)
	}
}

func TestRegisterPixWebhookErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		want   error
	}{
		{400, shared.ErrValidation},
		{401, shared.ErrUnauthorized},
		{404, shared.ErrNotFound},
		{503, shared.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			ts := newWebhookTestServer(t)
			ts.handler = func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tc.status, map[string]string{"code": "X"})
			}
			p := ts.provider(t, oneTenant("t1", "c", "s"))
			err := p.RegisterPixWebhook(context.Background(), "t1", "chave", "https://hooks.example/pix")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: want %v, got %v", tc.status, tc.want, err)
			}
			// Defense-in-depth: an upstream error must never carry the webhook URL.
			if err != nil && strings.Contains(err.Error(), "hooks.example") {
				t.Fatalf("error leaked the webhook url: %v", err)
			}
		})
	}
}

func TestRegisterPixWebhookMissingCredential(t *testing.T) {
	t.Parallel()
	ts := newWebhookTestServer(t)
	p := ts.provider(t, &fakeCreds{creds: map[string]ports.BankCredential{}})
	if err := p.RegisterPixWebhook(context.Background(), "unknown", "chave", "https://hooks.example/pix"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("missing credential should propagate ErrNotFound, got %v", err)
	}
	ts.mu.Lock()
	hits := ts.putHits
	ts.mu.Unlock()
	if hits != 0 {
		t.Fatalf("webhook endpoint must not be hit without a credential, hits=%d", hits)
	}
}

func TestRegisterPixWebhookContextCancelled(t *testing.T) {
	t.Parallel()
	ts := newWebhookTestServer(t)
	p := ts.provider(t, oneTenant("t1", "c", "s"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.RegisterPixWebhook(ctx, "t1", "chave", "https://hooks.example/pix"); err == nil {
		t.Fatal("expected error with a cancelled context")
	}
}
