package app

import (
	"context"
	"fmt"

	"github.com/ia-dev-sindireceita/payment/internal/domain/balance"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// BalanceService orchestrates the account-balance surface (saldo, C6 "Saldo &
// Extrato", roteiro grupo 13): read the current balance snapshot of the
// authenticated tenant's account. The snapshot invariants (currency present, blocked
// >= 0, snapshot instant present) live in the balance.Balance value object, not here;
// this service re-validates the PSP response through the domain (defense in depth) so
// a malformed snapshot never reaches the caller — mirroring the extrato read. The
// tenant is ALWAYS the authenticated tenant, never client input (threat H1/P1): no
// query/body parameter selects which tenant's saldo is read.
type BalanceService struct {
	tenants  ports.TenantRepository
	balances ports.BalanceProvider
}

// NewBalanceService wires a BalanceService from the provided ports.
func NewBalanceService(d Deps) *BalanceService {
	return &BalanceService{tenants: d.Tenants, balances: d.Balance}
}

// GetBalanceInput is the validated boundary input to read a saldo. TenantID is the
// authenticated tenant; there is no other parameter — the balance is always the
// authenticated tenant's whole-account snapshot.
type GetBalanceInput struct {
	TenantID string
}

// requireActiveTenant resolves the authenticated tenant and asserts it is active. It
// is the deny-by-default guard the balance read runs first.
func (s *BalanceService) requireActiveTenant(ctx context.Context, tenantID string) error {
	t, err := s.tenants.FindTenantByID(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("resolve tenant: %w", err)
	}
	if !t.Active() {
		return shared.NewValidationError("tenant", "tenant is not active")
	}
	return nil
}

// GetBalance returns the authenticated tenant's current account-balance snapshot (C6
// "Saldo & Extrato" /balance). The returned snapshot is re-validated through the
// domain so an inconsistent PSP response is rejected rather than surfaced.
func (s *BalanceService) GetBalance(ctx context.Context, in GetBalanceInput) (ports.Balance, error) {
	if err := s.requireActiveTenant(ctx, in.TenantID); err != nil {
		return ports.Balance{}, err
	}
	res, err := s.balances.GetBalance(ctx, in.TenantID)
	if err != nil {
		return ports.Balance{}, fmt.Errorf("bank get balance: %w", err)
	}
	// Re-validate the PSP response through the domain (snapshot well-formed, tenant
	// present) so a malformed balance never reaches the caller.
	if _, err := balance.New(in.TenantID, res.AvailableCents, res.BlockedCents, res.Currency, res.AsOf); err != nil {
		return ports.Balance{}, err
	}
	return res, nil
}
