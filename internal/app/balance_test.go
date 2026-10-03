package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// fakeBalanceProvider is a controllable BalanceProvider for the use-case tests: it
// returns a fixed balance and/or a fixed error so the service's domain re-validation
// and error-wrapping paths can be exercised without the stub.
type fakeBalanceProvider struct {
	res ports.Balance
	err error
}

func (f *fakeBalanceProvider) GetBalance(_ context.Context, _ string) (ports.Balance, error) {
	if f.err != nil {
		return ports.Balance{}, f.err
	}
	return f.res, nil
}

func balanceAsOf() time.Time { return time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC) }

// newBalanceHarness wires a BalanceService over the given provider plus a seeded,
// credentialed tenant. The saldo is not a billable surface, so no pricing is needed.
func newBalanceHarness(t *testing.T, prov ports.BalanceProvider) (*app.BalanceService, string) {
	t.Helper()
	h := newHarness(t)
	h.deps.Balance = prov
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	return app.NewBalanceService(h.deps), tn.ID()
}

// TestBalanceGet covers the happy path through the stub and the unknown-tenant guard.
func TestBalanceGet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Balance = h.bank
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	h.bank.SeedBalance(tn.ID(), ports.Balance{AvailableCents: 123456, BlockedCents: 789, Currency: "BRL", AsOf: balanceAsOf()})
	svc := app.NewBalanceService(h.deps)

	got, err := svc.GetBalance(context.Background(), app.GetBalanceInput{TenantID: tn.ID()})
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if got.AvailableCents != 123456 || got.BlockedCents != 789 || got.Currency != "BRL" {
		t.Fatalf("balance: %+v", got)
	}

	// Unknown tenant → error (resolve tenant fails).
	if _, err := svc.GetBalance(context.Background(), app.GetBalanceInput{TenantID: "missing"}); err == nil {
		t.Fatal("unknown tenant must error")
	}
}

// TestBalanceDefaultSnapshot proves an un-seeded tenant reads the stub's deterministic
// zero snapshot (valid BRL + instant) rather than an error.
func TestBalanceDefaultSnapshot(t *testing.T) {
	t.Parallel()
	svc, tenantID := newBalanceHarnessStub(t)
	got, err := svc.GetBalance(context.Background(), app.GetBalanceInput{TenantID: tenantID})
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if got.AvailableCents != 0 || got.BlockedCents != 0 || got.Currency != "BRL" {
		t.Fatalf("default snapshot: %+v", got)
	}
}

// newBalanceHarnessStub wires a BalanceService over the in-memory stub (not the fake)
// plus a credentialed tenant, leaving the balance un-seeded.
func newBalanceHarnessStub(t *testing.T) (*app.BalanceService, string) {
	t.Helper()
	h := newHarness(t)
	h.deps.Balance = h.bank
	admin := app.NewAdminService(h.deps)
	tn, err := admin.CreateTenant(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	h.deps.Credentials.(*secret.Store).Set(tn.ID(), ports.BankCredential{ClientID: "cid", Secret: "shh"})
	return app.NewBalanceService(h.deps), tn.ID()
}

func TestBalanceProviderError(t *testing.T) {
	t.Parallel()
	svc, tenantID := newBalanceHarness(t, &fakeBalanceProvider{err: shared.ErrUnavailable})
	if _, err := svc.GetBalance(context.Background(), app.GetBalanceInput{TenantID: tenantID}); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable wrapped, got %v", err)
	}
}

// TestBalanceMalformedResponseRejected proves the use-case re-validates the PSP
// response through the domain: a malformed snapshot (here a negative blocked amount)
// is rejected rather than surfaced (defense in depth).
func TestBalanceMalformedResponseRejected(t *testing.T) {
	t.Parallel()
	prov := &fakeBalanceProvider{res: ports.Balance{AvailableCents: 100, BlockedCents: -1, Currency: "BRL", AsOf: balanceAsOf()}}
	svc, tenantID := newBalanceHarness(t, prov)
	if _, err := svc.GetBalance(context.Background(), app.GetBalanceInput{TenantID: tenantID}); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("want ErrValidation for malformed snapshot, got %v", err)
	}
}
