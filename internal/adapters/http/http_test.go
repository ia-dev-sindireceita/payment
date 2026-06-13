package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/bank"
	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/messaging/inmemory"
	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/system"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

const (
	tenantToken = "ttok"
	adminToken  = "atok"
	webhookSec  = "whsec"
)

type fixture struct {
	handler  http.Handler
	tenantID string
	bank     *bank.StubProvider
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := persistence.NewStore()
	creds := secret.NewStore(nil)
	stub := bank.NewStubProvider(creds)
	deps := app.Deps{
		Payments:    store,
		Tenants:     store,
		Pricing:     store,
		Ledger:      store,
		Processed:   store,
		Bus:         inmemory.NewBus(),
		Bank:        stub,
		Credentials: creds,
		Clock:       system.Clock{},
		IDs:         system.IDProvider{},
	}
	admin := app.NewAdminService(deps)
	tn, err := admin.CreateTenant(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	creds.Set(tn.ID(), ports.BankCredential{ClientID: "c", Secret: "s"})
	if _, err := admin.SetEndpointPrice(context.Background(), tn.ID(), "pix.create", 50); err != nil {
		t.Fatalf("seed price: %v", err)
	}

	auth := httpadapter.NewStaticTokenAuth(map[string]string{tenantToken: tn.ID()}, []string{adminToken}, webhookSec)
	srv := httpadapter.NewServer(httpadapter.Config{
		Charges:     app.NewChargeService(deps),
		Admin:       admin,
		Webhooks:    app.NewWebhookService(deps),
		TenantAuth:  auth,
		AdminAuth:   auth,
		WebhookAuth: auth,
	})
	return &fixture{handler: srv.Router(), tenantID: tn.ID(), bank: stub}
}

func do(t *testing.T, h http.Handler, method, path, token string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := do(t, f.handler, http.MethodGet, "/healthz", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestAuthDenyByDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Tenant route without token.
	if rec := do(t, f.handler, http.MethodPost, "/v1/charges", "", map[string]string{"Idempotency-Key": "k"}, map[string]any{"endpoint": "pix.create", "amount_cents": 10, "currency": "BRL"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	// Tenant route with admin token (wrong audience).
	if rec := do(t, f.handler, http.MethodPost, "/v1/charges", adminToken, map[string]string{"Idempotency-Key": "k"}, map[string]any{"endpoint": "pix.create", "amount_cents": 10, "currency": "BRL"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for wrong token, got %d", rec.Code)
	}
	// Admin route without admin token.
	if rec := do(t, f.handler, http.MethodPost, "/admin/tenants", tenantToken, nil, map[string]any{"name": "X"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 admin, got %d", rec.Code)
	}
}

func TestCreateAndGetCharge(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/charges", tenantToken, map[string]string{"Idempotency-Key": "k1"}, map[string]any{"endpoint": "pix.create", "amount_cents": 2500, "currency": "BRL"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}
	var pv struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		TenantID string `json:"tenant_id"`
		TxID     string `json:"tx_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pv.ID == "" || pv.Status != "pending" || pv.TenantID != f.tenantID || pv.TxID == "" {
		t.Fatalf("unexpected payment view: %+v", pv)
	}

	// Read it back.
	rec = do(t, f.handler, http.MethodGet, "/v1/charges/"+pv.ID, tenantToken, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	// Unknown id → 404.
	rec = do(t, f.handler, http.MethodGet, "/v1/charges/missing", tenantToken, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for missing, got %d", rec.Code)
	}
}

func TestCreateChargeValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Missing Idempotency-Key.
	if rec := do(t, f.handler, http.MethodPost, "/v1/charges", tenantToken, nil, map[string]any{"endpoint": "pix.create", "amount_cents": 10, "currency": "BRL"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 missing idem, got %d", rec.Code)
	}
	// Unknown field (mass-assignment guard).
	if rec := do(t, f.handler, http.MethodPost, "/v1/charges", tenantToken, map[string]string{"Idempotency-Key": "k"}, map[string]any{"endpoint": "pix.create", "amount_cents": 10, "currency": "BRL", "tenant_id": "evil"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 unknown field, got %d", rec.Code)
	}
	// Unpriced endpoint → 404.
	if rec := do(t, f.handler, http.MethodPost, "/v1/charges", tenantToken, map[string]string{"Idempotency-Key": "k2"}, map[string]any{"endpoint": "unpriced", "amount_cents": 10, "currency": "BRL"}); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 unpriced, got %d", rec.Code)
	}
	// Bad amount → 400.
	if rec := do(t, f.handler, http.MethodPost, "/v1/charges", tenantToken, map[string]string{"Idempotency-Key": "k3"}, map[string]any{"endpoint": "pix.create", "amount_cents": 0, "currency": "BRL"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 amount, got %d", rec.Code)
	}
}

func TestAdminEndpoints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Create tenant.
	rec := do(t, f.handler, http.MethodPost, "/admin/tenants", adminToken, nil, map[string]any{"name": "New Co"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tenant: %d", rec.Code)
	}
	var tv struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tv)
	if tv.ID == "" {
		t.Fatal("no tenant id")
	}
	// Set price for it.
	rec = do(t, f.handler, http.MethodPost, "/admin/tenants/"+tv.ID+"/pricing", adminToken, nil, map[string]any{"endpoint": "pix.create", "price_cents": 99})
	if rec.Code != http.StatusOK {
		t.Fatalf("set price: %d body=%s", rec.Code, rec.Body.String())
	}
	// Invalid name → 400.
	rec = do(t, f.handler, http.MethodPost, "/admin/tenants", adminToken, nil, map[string]any{"name": " "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 invalid name, got %d", rec.Code)
	}
}

func TestWebhookFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// First create a charge to settle.
	rec := do(t, f.handler, http.MethodPost, "/v1/charges", tenantToken, map[string]string{"Idempotency-Key": "k1"}, map[string]any{"endpoint": "pix.create", "amount_cents": 2500, "currency": "BRL"})
	var pv struct {
		ID   string `json:"id"`
		TxID string `json:"tx_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pv)

	// Webhook without secret → 401 (failure-closed).
	if rec := do(t, f.handler, http.MethodPost, "/webhooks/bank", "", nil, map[string]any{"tenant_id": f.tenantID, "tx_id": pv.TxID, "event_key": "e1"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 webhook, got %d", rec.Code)
	}
	// Webhook with secret, bank not yet settled → 202 but still pending.
	if rec := do(t, f.handler, http.MethodPost, "/webhooks/bank", "", map[string]string{"X-Webhook-Secret": webhookSec}, map[string]any{"tenant_id": f.tenantID, "tx_id": pv.TxID, "event_key": "e1"}); rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	// Settle at bank, then webhook → 202 and payment becomes paid.
	f.bank.MarkSettled(f.tenantID, pv.TxID)
	if rec := do(t, f.handler, http.MethodPost, "/webhooks/bank", "", map[string]string{"X-Webhook-Secret": webhookSec}, map[string]any{"tenant_id": f.tenantID, "tx_id": pv.TxID, "event_key": "e2"}); rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 settle, got %d", rec.Code)
	}
	rec = do(t, f.handler, http.MethodGet, "/v1/charges/"+pv.ID, tenantToken, nil, nil)
	var got struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != "paid" {
		t.Fatalf("expected paid, got %s", got.Status)
	}
}

func TestRateLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var limited bool
	for i := 0; i < 60; i++ {
		rec := do(t, f.handler, http.MethodGet, "/v1/charges/none", tenantToken, nil, nil)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("expected a 429 within 60 rapid requests")
	}
}
