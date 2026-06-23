package bank

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// roteiro 6.a: register a boleto with its parameters, then read it back by id; the
// registered fine/interest/discount echo for reconciliation.
func TestStubGetBoleto(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	ctx := context.Background()
	due := time.Unix(1_800_000_000, 0).UTC()

	if _, err := s.CreateBoleto(ctx, "t1", ports.BoletoRequest{
		TenantID: "t1", BoletoID: "bol_1", AmountCents: 100000, Currency: "BRL",
		DueDate: due, FineBps: 200, MonthlyInterestBps: 100,
		Discounts: []ports.BoletoDiscountTier{{DaysBeforeDue: 0, Bps: 500}},
	}); err != nil {
		t.Fatalf("CreateBoleto: %v", err)
	}

	got, err := s.GetBoleto(ctx, "t1", "bol_1")
	if err != nil {
		t.Fatalf("GetBoleto: %v", err)
	}
	if got.BoletoID != "bol_1" || got.Status != "REGISTERED" || got.AmountCents != 100000 {
		t.Fatalf("unexpected: %+v", got)
	}
	if got.FineBps != 200 || got.MonthlyInterestBps != 100 || !got.DueDate.Equal(due) {
		t.Fatalf("registered params not reconciled: %+v", got)
	}
	if len(got.Discounts) != 1 || got.Discounts[0].Bps != 500 {
		t.Fatalf("discounts not reconciled: %+v", got.Discounts)
	}
}

// An unknown id within a known tenant is not-found; a boleto registered by one
// tenant is invisible to another (tenant isolation).
func TestStubGetBoletoIsolation(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	ctx := context.Background()

	if _, err := s.CreateBoleto(ctx, "t1", ports.BoletoRequest{TenantID: "t1", BoletoID: "bol_1", AmountCents: 100, Currency: "BRL"}); err != nil {
		t.Fatalf("CreateBoleto: %v", err)
	}
	if _, err := s.GetBoleto(ctx, "t1", "nope"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("unknown id: want ErrNotFound, got %v", err)
	}
	// Another tenant cannot read t1's boleto, and a tenant without a credential is
	// rejected before any lookup.
	if _, err := s.GetBoleto(ctx, "other", "bol_1"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("cross-tenant: want ErrNotFound, got %v", err)
	}
}

// Registration is idempotent on (tenant, id): a repeat returns the same record.
func TestStubCreateBoletoIdempotent(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	ctx := context.Background()
	req := ports.BoletoRequest{TenantID: "t1", BoletoID: "bol_1", AmountCents: 100, Currency: "BRL"}

	first, err := s.CreateBoleto(ctx, "t1", req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	again, err := s.CreateBoleto(ctx, "t1", req)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if first.TxID != again.TxID {
		t.Fatalf("idempotent create returned different tx: %s vs %s", first.TxID, again.TxID)
	}
}
