package bank

import (
	"context"

	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// This file extends StubProvider to back ports.BalanceProvider (account balance /
// saldo, C6 "Saldo & Extrato", roteiro grupo 13) in-memory, so the use-case and HTTP
// route run end-to-end in stub mode (PAYMENT_C6_BASE_URL unset) without C6. The
// behaviour mirrors the real C6 adapter's observable contract: per-tenant credential
// isolation resolved on every call (the secret is never logged) and a tenant-scoped
// read where a tenant only ever observes its own balance (no cross-tenant leak).

// compile-time assertion that StubProvider satisfies the balance port.
var _ ports.BalanceProvider = (*StubProvider)(nil)

// SeedBalance sets the account-balance snapshot (saldo) a tenant reads, for tests and
// local dev. It overwrites any previously seeded balance for the tenant.
func (s *StubProvider) SeedBalance(tenantID string, bal ports.Balance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[tenantID] = bal
}

// GetBalance returns the tenant's current account-balance snapshot (C6 "Saldo &
// Extrato" /balance). It resolves the tenant credential first (isolation), then
// returns the seeded snapshot. A tenant with no seeded balance reads a deterministic
// zero snapshot (BRL, stamped with the stub clock) so the route works out of the box
// in stub mode and still satisfies the domain's snapshot invariants (currency +
// instant present).
func (s *StubProvider) GetBalance(ctx context.Context, tenantID string) (ports.Balance, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID, s.bankID); err != nil {
		return ports.Balance{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bal, ok := s.balances[tenantID]; ok {
		return bal, nil
	}
	return ports.Balance{Currency: "BRL", AsOf: s.now()}, nil
}
