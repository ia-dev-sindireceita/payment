// Package account holds the Account aggregate — the API user / reseller that
// sits ABOVE the tenant in the two-level tenancy model (ADR-0009, SIN-69119 §3.1).
//
// An Account owns N Tenants (empresas-clientes) by reference (id), not by
// composition — the two aggregates stay separate (DDD-lite; the self-join on
// tenants was rejected, see ADR-0009 §4). An Account is an attribution-only
// concept: it identifies who a token belongs to and is the rollup target for
// billing, but it NEVER holds a bank credential and NEVER touches money (model
// (a) PSP-Indireto pass-through). It mirrors the Tenant aggregate deliberately so
// the persistence keying and the admin plane can reuse the tenant schema shape.
//
// Pure domain: no database/sql, no HTTP, no vendor SDK.
package account

import (
	"strings"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// Account is the aggregate root for an API user (reseller) that owns tenants.
type Account struct {
	id        string
	name      string
	active    bool
	createdAt time.Time
}

// New constructs an Account, enforcing its invariants. An account is created
// active. Invariants mirror Tenant.New so the admin plane treats the two the
// same way.
func New(id, name string, now time.Time) (*Account, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, shared.NewValidationError("id", "account id is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, shared.NewValidationError("name", "account name is required")
	}
	if len(name) > 200 {
		return nil, shared.NewValidationError("name", "account name too long")
	}
	return &Account{id: id, name: name, active: true, createdAt: now}, nil
}

// Rehydrate rebuilds an Account from persisted state without re-running creation
// validation (used by persistence adapters).
func Rehydrate(id, name string, active bool, createdAt time.Time) *Account {
	return &Account{id: id, name: name, active: active, createdAt: createdAt}
}

// ID returns the account identifier.
func (a *Account) ID() string { return a.id }

// Name returns the account display name.
func (a *Account) Name() string { return a.name }

// Active reports whether the account is enabled.
func (a *Account) Active() bool { return a.active }

// CreatedAt returns the creation timestamp.
func (a *Account) CreatedAt() time.Time { return a.createdAt }

// Deactivate suspends the account.
func (a *Account) Deactivate() { a.active = false }

// Activate re-enables a suspended account. Idempotent, mirroring Tenant.Activate.
func (a *Account) Activate() { a.active = true }
