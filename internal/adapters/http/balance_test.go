package http_test

import (
	"context"
	"encoding/json"
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

// balanceFixture wires a Server with the BalanceService backed by the in-memory stub,
// plus two seeded/credentialed tenants (A with a seeded balance, B with none) so
// cross-tenant isolation can be exercised.
type balanceFixture struct {
	handler  http.Handler
	tenantID string
	bank     *bank.StubProvider
}

func newBalanceFixture(t *testing.T) *balanceFixture {
	t.Helper()
	ctx := context.Background()
	store := persistence.NewStore()
	creds := secret.NewStore(nil)
	stub := bank.NewStubProvider(creds)
	bus := inmemory.NewBus()
	deps := app.Deps{
		Payments:    store,
		Tenants:     store,
		Pricing:     store,
		Ledger:      store,
		Processed:   store,
		Bus:         bus,
		Bank:        stub,
		Balance:     stub,
		Credentials: creds,
		Clock:       system.Clock{},
		IDs:         system.IDProvider{},
	}
	admin := app.NewAdminService(deps)
	tnA, err := admin.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatalf("seed tenant A: %v", err)
	}
	tnB, err := admin.CreateTenant(ctx, "Beta")
	if err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	creds.Set(tnA.ID(), ports.BankCredential{ClientID: tenantClientID, Secret: "s"})
	creds.Set(tnB.ID(), ports.BankCredential{ClientID: "c6-beta", Secret: "s"})
	// Seed tenant A's balance. Tenant B reads the stub's default zero snapshot.
	stub.SeedBalance(tnA.ID(), ports.Balance{
		AvailableCents: 123456,
		BlockedCents:   789,
		Currency:       "BRL",
		AsOf:           time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
	})
	auth := httpadapter.NewStaticTokenAuth(
		map[string]string{tenantToken: tnA.ID(), tenantTokenB: tnB.ID()},
		[]string{adminToken}, nil)
	srv := httpadapter.NewServer(httpadapter.Config{
		Balance:    app.NewBalanceService(deps),
		Admin:      admin,
		TenantAuth: auth,
		AdminAuth:  auth,
	})
	return &balanceFixture{handler: srv.Router(), tenantID: tnA.ID(), bank: stub}
}

// C6 "Saldo & Extrato": GET /v1/balance → 200 with the authenticated tenant's snapshot.
func TestBalanceGetSuccess(t *testing.T) {
	t.Parallel()
	f := newBalanceFixture(t)
	rec := do(t, f.handler, http.MethodGet, "/v1/balance", tenantToken, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var v struct {
		AvailableCents int64  `json:"available_cents"`
		BlockedCents   int64  `json:"blocked_cents"`
		Currency       string `json:"currency"`
		AsOf           string `json:"as_of"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.AvailableCents != 123456 || v.BlockedCents != 789 || v.Currency != "BRL" {
		t.Fatalf("view mapping: %+v", v)
	}
	if v.AsOf != "2026-10-02T12:00:00Z" {
		t.Fatalf("as_of mapping: %q", v.AsOf)
	}

	// Deny-by-default: no token → 401.
	if rec := do(t, f.handler, http.MethodGet, "/v1/balance", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without auth, got %d", rec.Code)
	}
}

// Cross-tenant isolation: tenant B (no seeded balance) reads its own default zero
// snapshot, never tenant A's — the saldo is the authenticated tenant's, never
// selectable.
func TestBalanceTenantIsolation(t *testing.T) {
	t.Parallel()
	f := newBalanceFixture(t)
	rec := do(t, f.handler, http.MethodGet, "/v1/balance", tenantTokenB, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var v struct {
		AvailableCents int64  `json:"available_cents"`
		Currency       string `json:"currency"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.AvailableCents != 0 {
		t.Fatalf("tenant B must not see tenant A's balance, got %d", v.AvailableCents)
	}
	if v.Currency != "BRL" {
		t.Fatalf("default snapshot currency: %q", v.Currency)
	}
}
