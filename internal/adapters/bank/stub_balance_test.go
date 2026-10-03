package bank_test

import (
	"context"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/bank"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

func newBalanceStub(t *testing.T) *bank.StubProvider {
	t.Helper()
	creds := secret.NewStore(map[string]ports.BankCredential{
		"t1": {ClientID: "c", Secret: "s"},
		"t2": {ClientID: "c2", Secret: "s2"},
	})
	return bank.NewStubProvider(creds)
}

func TestStubBalanceDefaultSnapshot(t *testing.T) {
	t.Parallel()
	p := newBalanceStub(t)
	// Pin the clock so the default snapshot's instant is deterministic.
	fixed := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	p.SetClock(func() time.Time { return fixed })

	got, err := p.GetBalance(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if got.AvailableCents != 0 || got.BlockedCents != 0 || got.Currency != "BRL" {
		t.Fatalf("default snapshot: %+v", got)
	}
	if !got.AsOf.Equal(fixed) {
		t.Fatalf("as_of not stamped by clock: %v", got.AsOf)
	}
}

func TestStubBalanceSeedAndIsolation(t *testing.T) {
	t.Parallel()
	p := newBalanceStub(t)
	ctx := context.Background()
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	p.SeedBalance("t1", ports.Balance{AvailableCents: 500000, BlockedCents: 1000, Currency: "BRL", AsOf: asOf})

	got, err := p.GetBalance(ctx, "t1")
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if got.AvailableCents != 500000 || got.BlockedCents != 1000 {
		t.Fatalf("seeded balance: %+v", got)
	}

	// Another tenant sees its own (default) snapshot, never t1's (isolation).
	other, err := p.GetBalance(ctx, "t2")
	if err != nil {
		t.Fatalf("GetBalance t2: %v", err)
	}
	if other.AvailableCents != 0 {
		t.Fatalf("tenant isolation: t2 must not see t1's balance, got %d", other.AvailableCents)
	}
}

func TestStubBalanceUnknownTenantCredential(t *testing.T) {
	t.Parallel()
	p := newBalanceStub(t)
	if _, err := p.GetBalance(context.Background(), "nope"); err == nil {
		t.Fatal("missing credential must error (isolation)")
	}
}
