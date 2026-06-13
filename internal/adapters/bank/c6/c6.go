// Package c6 implements ports.BankProvider against the C6 bank (PSP). It owns all
// HTTP and OAuth2 concerns so the domain and use-cases never import net/http or a
// vendor SDK (Hexagonal). It is the foundation seam for the later C6 slices (PIX,
// PIX Automático, BolePix, Checkout, Webhook).
//
// Security posture (secure-by-default): HTTPS-only endpoints (non-HTTPS is
// rejected at construction), TLS >= 1.2, per-request timeouts and propagated
// context (no goroutine leak), per-tenant OAuth2 client_credentials tokens held
// only in memory, and errors that never leak the client secret or the raw PSP
// response body.
package c6

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// maxResponseBytes caps how much of an upstream response is read, bounding memory
// on a hostile or buggy PSP response.
const maxResponseBytes = 1 << 20 // 1 MiB

// defaultTimeout is the per-request timeout when Config.Timeout is unset.
const defaultTimeout = 15 * time.Second

// Config configures the C6 provider's transport. Endpoints are per-environment
// and supplied by the caller (from process config) — never hard-coded.
type Config struct {
	// BaseURL is the C6 REST API base (e.g. https://api.c6bank.example). Required.
	BaseURL string
	// TokenURL is the OAuth2 client_credentials token endpoint. Required.
	TokenURL string
	// Scope is an optional OAuth2 scope requested with the token.
	Scope string
	// Timeout is the per-request timeout. Defaults to defaultTimeout when zero.
	Timeout time.Duration
	// HTTPClient overrides the HTTP client (used in tests against httptest TLS
	// servers, or to inject an mTLS transport when the C6 docs require client
	// certificates). When nil a TLS-1.2+ client with Timeout is built.
	HTTPClient *http.Client
	// Now overrides the clock (token expiry). Defaults to time.Now.
	Now func() time.Time
}

// Provider implements ports.BankProvider against C6.
type Provider struct {
	baseURL string
	httpc   *http.Client
	tokens  *tokenManager
}

// compile-time assertion that Provider satisfies the port.
var _ ports.BankProvider = (*Provider)(nil)

// New validates the config and builds a Provider. Both endpoints must be absolute
// HTTPS URLs; an http:// or malformed endpoint is rejected (secure-by-default,
// TLS-only). creds resolves per-tenant OAuth2 credentials at token-fetch time.
func New(cfg Config, creds ports.CredentialStore) (*Provider, error) {
	if creds == nil {
		return nil, fmt.Errorf("c6: credential store is required")
	}
	if err := requireHTTPS("base_url", cfg.BaseURL); err != nil {
		return nil, err
	}
	if err := requireHTTPS("token_url", cfg.TokenURL); err != nil {
		return nil, err
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = defaultHTTPClient(timeout)
	}

	return &Provider{
		baseURL: trimTrailingSlash(cfg.BaseURL),
		httpc:   httpc,
		tokens:  newTokenManager(creds, cfg.TokenURL, cfg.Scope, httpc, now),
	}, nil
}

// requireHTTPS rejects any endpoint that is not an absolute https:// URL. A
// non-TLS PSP endpoint would expose the bearer token and charge data in transit.
func requireHTTPS(field, raw string) error {
	if raw == "" {
		return fmt.Errorf("c6: %s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("c6: %s is not a valid URL", field)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("c6: %s must be https (got %q)", field, u.Scheme)
	}
	return nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// defaultHTTPClient builds an HTTP client that enforces TLS >= 1.2 and a total
// per-request timeout. A bounded transport keeps idle connections in check so the
// adapter does not leak goroutines/sockets.
func defaultHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}

// idempotencyKey returns the key to forward to the PSP: the caller's
// IdempotencyKey when present, else the PaymentID as a deterministic fallback.
func idempotencyKey(req ports.ChargeRequest) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return req.PaymentID
}

// chargeRequestBody is the JSON sent to C6 to create a charge.
type chargeRequestBody struct {
	PaymentID   string `json:"payment_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// chargeResponseBody is the subset of C6's charge representation we consume.
type chargeResponseBody struct {
	TxID   string `json:"txid"`
	Status string `json:"status"`
}

// CreateCharge creates a charge at C6. It forwards the PaymentID as the
// Idempotency-Key header so the PSP collapses retried/concurrent creations into a
// single charge — defense-in-depth against double-charging that does not depend on
// the local reservation. The OAuth2 bearer token is attached per tenant.
func (p *Provider) CreateCharge(ctx context.Context, tenantID string, req ports.ChargeRequest) (ports.ChargeResult, error) {
	token, err := p.tokens.token(ctx, tenantID)
	if err != nil {
		return ports.ChargeResult{}, err
	}

	payload, err := json.Marshal(chargeRequestBody{
		PaymentID:   req.PaymentID,
		AmountCents: req.AmountCents,
		Currency:    req.Currency,
	})
	if err != nil {
		return ports.ChargeResult{}, &Error{Op: "create_charge", sentinel: shared.ErrValidation}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/charges", bytes.NewReader(payload))
	if err != nil {
		return ports.ChargeResult{}, transportError("create_charge")
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// Forward the idempotency key so the PSP collapses retried/concurrent creations
	// (F3b defense-in-depth, SIN-64720). Honor the caller's key; fall back to the
	// PaymentID — a deterministic key — when none was supplied, never silently
	// dropping idempotency (the ports.ChargeRequest contract).
	if idem := idempotencyKey(req); idem != "" {
		httpReq.Header.Set("Idempotency-Key", idem)
	}

	var out chargeResponseBody
	if err := p.do(httpReq, "create_charge", &out); err != nil {
		return ports.ChargeResult{}, err
	}
	return ports.ChargeResult{TxID: out.TxID, Status: out.Status}, nil
}

// GetCharge reconciles the authoritative state of a charge from C6 (never trust a
// raw webhook — threat W3).
func (p *Provider) GetCharge(ctx context.Context, tenantID, txID string) (ports.ChargeResult, error) {
	token, err := p.tokens.token(ctx, tenantID)
	if err != nil {
		return ports.ChargeResult{}, err
	}

	endpoint := p.baseURL + "/charges/" + url.PathEscape(txID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ports.ChargeResult{}, transportError("get_charge")
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/json")

	var out chargeResponseBody
	if err := p.do(httpReq, "get_charge", &out); err != nil {
		return ports.ChargeResult{}, err
	}
	return ports.ChargeResult{TxID: out.TxID, Status: out.Status}, nil
}

// do executes an authenticated request, maps a non-2xx into a domain error, and
// decodes a 2xx body into dst. The response body is always drained and closed and
// read under a size cap; raw body bytes never escape into an error.
func (p *Provider) do(req *http.Request, op string, dst any) error {
	resp, err := p.httpc.Do(req)
	if err != nil {
		return transportError(op)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode/100 != 2 {
		return mapError(op, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return &Error{Op: op, StatusCode: resp.StatusCode, sentinel: shared.ErrUnavailable}
	}
	return nil
}
