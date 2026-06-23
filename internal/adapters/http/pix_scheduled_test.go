package http_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/bank"
	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/messaging/inmemory"
	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/system"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// scheduledPixFixture wires a Server with the PIX service backed by the in-memory stub
// (scheduled-charge + webhook ports wired), plus a seeded, priced, credentialed tenant.
// The stub clock is pinned so the date window in list tests is deterministic.
type scheduledPixFixture struct {
	handler  http.Handler
	tenantID string
	bank     *bank.StubProvider
	now      time.Time
}

func newScheduledPixFixture(t *testing.T) *scheduledPixFixture {
	t.Helper()
	store := persistence.NewStore()
	creds := secret.NewStore(nil)
	stub := bank.NewStubProvider(creds)
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	stub.SetClock(func() time.Time { return now })
	bus := inmemory.NewBus()
	deps := app.Deps{
		Payments:     store,
		Tenants:      store,
		Pricing:      store,
		Ledger:       store,
		Processed:    store,
		Bus:          bus,
		Bank:         stub,
		Pix:          stub,
		PixScheduled: stub,
		PixWebhook:   stub,
		Credentials:  creds,
		Clock:        system.Clock{},
		IDs:          system.IDProvider{},
	}
	admin := app.NewAdminService(deps)
	tn, err := admin.CreateTenant(t.Context(), "Acme")
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	creds.Set(tn.ID(), ports.BankCredential{ClientID: "c6-acme", Secret: "s"})
	if _, err := admin.SetEndpointPrice(t.Context(), tn.ID(), app.PixScheduledCreateEndpoint, 70); err != nil {
		t.Fatalf("seed price: %v", err)
	}
	auth := httpadapter.NewStaticTokenAuth(map[string]string{tenantToken: tn.ID()}, []string{adminToken}, nil)
	srv := httpadapter.NewServer(httpadapter.Config{
		Pix:        app.NewPixService(deps),
		Admin:      admin,
		TenantAuth: auth,
		AdminAuth:  auth,
	})
	return &scheduledPixFixture{handler: srv.Router(), tenantID: tn.ID(), bank: stub, now: now}
}

// dueDate is a fixed RFC3339 due date used by the scheduled-charge tests.
func dueDate() string { return "2026-07-15T00:00:00Z" }

func devedorBody() map[string]any {
	return map[string]any{"tax_id": "12345678901", "name": "Maria Silva"}
}

// roteiro 7.5 + 7.7: create a scheduled charge (201), then read it back by txid (200).
func TestScheduledPixCreateAndGet(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)

	rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", tenantToken,
		map[string]string{"Idempotency-Key": "k1"},
		map[string]any{
			"amount_cents": 2500, "currency": "BRL",
			"due_date": dueDate(), "validity_after_due_days": 30,
			"devedor": devedorBody(),
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body.String())
	}
	body := decodePix(t, rec)
	txid, _ := body["txid"].(string)
	if txid == "" {
		t.Fatalf("missing txid: %v", body)
	}
	if body["status"] != "ATIVA" {
		t.Fatalf("status: %v", body["status"])
	}
	if body["qr_code"] == "" || body["qr_code_location"] == "" {
		t.Fatalf("missing QR material: %v", body)
	}
	if body["amount_cents"].(float64) != 2500 {
		t.Fatalf("amount: %v", body["amount_cents"])
	}

	// 7.7: GET by txid.
	rec = do(t, f.handler, http.MethodGet, "/v1/pix/scheduled/"+txid, tenantToken, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d body %s", rec.Code, rec.Body.String())
	}
	got := decodePix(t, rec)
	if got["txid"] != txid {
		t.Fatalf("get txid mismatch: %v", got["txid"])
	}
}

// roteiro 7.5: a scheduled charge requires a devedor (name + tax id).
func TestScheduledPixCreateMissingDevedor(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", tenantToken,
		map[string]string{"Idempotency-Key": "k1"},
		map[string]any{"amount_cents": 2500, "currency": "BRL", "due_date": dueDate()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 without devedor, got %d body %s", rec.Code, rec.Body.String())
	}
}

func TestScheduledPixCreateBadDevedorTaxID(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", tenantToken,
		map[string]string{"Idempotency-Key": "k1"},
		map[string]any{
			"amount_cents": 2500, "currency": "BRL", "due_date": dueDate(),
			"devedor": map[string]any{"tax_id": "123", "name": "Maria"},
		})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad devedor taxid, got %d", rec.Code)
	}
}

func TestScheduledPixCreateMissingDueDate(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", tenantToken,
		map[string]string{"Idempotency-Key": "k1"},
		map[string]any{"amount_cents": 2500, "currency": "BRL", "devedor": devedorBody()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing due_date, got %d", rec.Code)
	}
}

func TestScheduledPixCreateMissingIdempotencyKey(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", tenantToken, nil,
		map[string]any{"amount_cents": 2500, "currency": "BRL", "due_date": dueDate(), "devedor": devedorBody()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing idempotency key, got %d", rec.Code)
	}
}

func TestScheduledPixCreateUnknownField(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", tenantToken,
		map[string]string{"Idempotency-Key": "k1"},
		map[string]any{"amount_cents": 2500, "currency": "BRL", "due_date": dueDate(), "devedor": devedorBody(), "evil": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for unknown field, got %d", rec.Code)
	}
}

func TestScheduledPixCreateRequiresAuth(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", "",
		map[string]string{"Idempotency-Key": "k1"},
		map[string]any{"amount_cents": 2500, "currency": "BRL", "due_date": dueDate(), "devedor": devedorBody()})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without auth, got %d", rec.Code)
	}
}

func TestScheduledPixGetUnknownTxid(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodGet, "/v1/pix/scheduled/missing", tenantToken, nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown txid, got %d", rec.Code)
	}
}

// roteiro 7.6: list scheduled charges by date window (200).
func TestScheduledPixListByDate(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)

	for _, k := range []string{"k1", "k2"} {
		rec := do(t, f.handler, http.MethodPost, "/v1/pix/scheduled", tenantToken,
			map[string]string{"Idempotency-Key": k},
			map[string]any{
				"amount_cents": 100, "currency": "BRL",
				"due_date": dueDate(), "validity_after_due_days": 5, "devedor": devedorBody(),
			})
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed create %s: %d body %s", k, rec.Code, rec.Body.String())
		}
	}

	start := f.now.Add(-time.Hour).UTC().Format(time.RFC3339)
	end := f.now.Add(time.Hour).UTC().Format(time.RFC3339)
	rec := do(t, f.handler, http.MethodGet, "/v1/pix/scheduled?start="+start+"&end="+end, tenantToken, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d body %s", rec.Code, rec.Body.String())
	}
	body := decodePix(t, rec)
	if body["total_items"].(float64) != 2 {
		t.Fatalf("want 2 charges, got %v", body["total_items"])
	}
	charges, ok := body["charges"].([]any)
	if !ok || len(charges) != 2 {
		t.Fatalf("charges array: %v", body["charges"])
	}
}

func TestScheduledPixListBadDates(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	cases := []string{
		"/v1/pix/scheduled", // missing both
		"/v1/pix/scheduled?start=notatime&end=2026-06-13T12:00:00Z",
		"/v1/pix/scheduled?start=2026-06-13T12:00:00Z&end=bad",
		"/v1/pix/scheduled?start=2026-06-13T12:00:00Z&end=2026-06-13T10:00:00Z",
		"/v1/pix/scheduled?start=2026-06-13T12:00:00Z&end=2026-06-13T13:00:00Z&page=-1",
	}
	for _, path := range cases {
		rec := do(t, f.handler, http.MethodGet, path, tenantToken, nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", path, rec.Code)
		}
	}
}

func TestScheduledPixListRequiresAuth(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodGet, "/v1/pix/scheduled?start=2026-06-13T11:00:00Z&end=2026-06-13T13:00:00Z", "", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without auth, got %d", rec.Code)
	}
}

// roteiro 7.8: register a PIX webhook (200) and confirm the stub recorded the URL.
func TestRegisterPixWebhook(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	const url = "https://hooks.acme.example/pix"
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/webhook", tenantToken, nil,
		map[string]any{"chave": "acme-pix-key", "webhook_url": url})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: status %d body %s", rec.Code, rec.Body.String())
	}
	body := decodePix(t, rec)
	if body["status"] != "registered" || body["chave"] != "acme-pix-key" {
		t.Fatalf("unexpected response: %v", body)
	}
	// The response must NOT echo the webhook URL (it is a capability secret).
	if _, present := body["webhook_url"]; present {
		t.Fatalf("response leaked the webhook url: %v", body)
	}
	if got, ok := f.bank.WebhookURL(f.tenantID, "acme-pix-key"); !ok || got != url {
		t.Fatalf("stub did not record the webhook url: got %q ok=%v", got, ok)
	}
}

func TestRegisterPixWebhookRejectsNonHTTPS(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/webhook", tenantToken, nil,
		map[string]any{"chave": "acme-pix-key", "webhook_url": "http://insecure.example/pix"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-https webhook, got %d", rec.Code)
	}
}

func TestRegisterPixWebhookMissingChave(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/webhook", tenantToken, nil,
		map[string]any{"webhook_url": "https://hooks.acme.example/pix"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing chave, got %d", rec.Code)
	}
}

func TestRegisterPixWebhookRequiresAuth(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	rec := do(t, f.handler, http.MethodPost, "/v1/pix/webhook", "", nil,
		map[string]any{"chave": "k", "webhook_url": "https://hooks.acme.example/pix"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without auth, got %d", rec.Code)
	}
}

// Static-vs-param routing guard: "/v1/pix/scheduled" must reach the scheduled handler,
// never the immediate "/v1/pix/{txid}" read.
func TestScheduledRouteNotShadowedByImmediateTxid(t *testing.T) {
	t.Parallel()
	f := newScheduledPixFixture(t)
	// Missing both dates => the scheduled LIST handler returns 400, not the immediate
	// GET-by-txid handler (which would treat "scheduled" as a txid and 404).
	rec := do(t, f.handler, http.MethodGet, "/v1/pix/scheduled", tenantToken, nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 from scheduled list handler, got %d", rec.Code)
	}
}
