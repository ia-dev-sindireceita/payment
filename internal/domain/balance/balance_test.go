package balance_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/balance"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

func asOf() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }

// TestNewValid covers the legal snapshots: a plain positive balance, a zero balance
// and an overdrawn (negative available) balance — the last proves the domain does
// NOT force the available amount positive.
func TestNewValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		available, blocked   int64
		currency             string
		wantAvail, wantBlock int64
		wantCurrency         string
	}{
		{"positive", 150000, 2500, "BRL", 150000, 2500, "BRL"},
		{"zero", 0, 0, "brl", 0, 0, "BRL"},
		{"overdraft negative available", -5000, 0, "BRL", -5000, 0, "BRL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := balance.New("t1", tc.available, tc.blocked, tc.currency, asOf())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if b.TenantID() != "t1" {
				t.Fatalf("tenant: %q", b.TenantID())
			}
			if b.AvailableCents() != tc.wantAvail || b.BlockedCents() != tc.wantBlock {
				t.Fatalf("amounts: avail=%d blocked=%d", b.AvailableCents(), b.BlockedCents())
			}
			if b.Currency() != tc.wantCurrency {
				t.Fatalf("currency normalise: %q", b.Currency())
			}
			if !b.AsOf().Equal(asOf()) {
				t.Fatalf("as_of: %v", b.AsOf())
			}
		})
	}
}

// TestNewInvalid covers every rejected invariant; each must be a shared.ErrValidation.
func TestNewInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		tenantID           string
		available, blocked int64
		currency           string
		asOf               time.Time
	}{
		{"missing tenant", "  ", 100, 0, "BRL", asOf()},
		{"missing currency", "t1", 100, 0, "  ", asOf()},
		{"negative blocked", "t1", 100, -1, "BRL", asOf()},
		{"missing as_of", "t1", 100, 0, "BRL", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := balance.New(tc.tenantID, tc.available, tc.blocked, tc.currency, tc.asOf); !errors.Is(err, shared.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}
