// Package balance holds the account-balance (saldo) domain (C6 "Saldo & Extrato",
// roteiro grupo 13). A Balance is a point-in-time snapshot of a tenant's account:
// the amount available to spend, the amount currently blocked/reserved, the
// currency and the instant the bank computed it. The aggregate OWNS its invariants
// as PURE domain — it never touches the network or the PSP. The adapter only
// transports the snapshot; deciding whether a transported snapshot is well-formed is
// the domain's responsibility (Hexagonal), so a malformed PSP response is rejected at
// the trust boundary rather than surfaced to the caller (defense in depth, mirrors
// the extrato re-validation).
package balance

import (
	"strings"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// Balance is a tenant's account-balance snapshot. It is a value object: the only way
// to build one is New, which enforces the invariants, so a Balance that exists is
// always well-formed.
//
// AvailableCents MAY be negative: an account with an overdraft line (cheque especial)
// can legitimately report a negative available balance, so the domain does not force
// it positive. BlockedCents is the reserved/held amount and can never be negative.
// Currency is the ISO-4217 code (e.g. BRL) and AsOf is the instant the bank computed
// the snapshot — both are required so a snapshot is never surfaced without the context
// needed to interpret it.
type Balance struct {
	tenantID       string
	availableCents int64
	blockedCents   int64
	currency       string
	asOf           time.Time
}

// New builds a Balance for tenantID, enforcing the invariants: the tenant id is
// required (a saldo is always the authenticated tenant's, never client input — threat
// H1/P1), the currency is required, the blocked amount cannot be negative and the
// snapshot instant is required. The available amount is unconstrained (an overdraft
// may make it negative). Each violation is a distinct shared.ErrValidation so the
// boundary can surface a precise 400/422.
func New(tenantID string, availableCents, blockedCents int64, currency string, asOf time.Time) (Balance, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Balance{}, shared.NewValidationError("tenant_id", "tenant id is required")
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return Balance{}, shared.NewValidationError("currency", "currency is required")
	}
	if blockedCents < 0 {
		return Balance{}, shared.NewValidationError("blocked_cents", "blocked amount must not be negative")
	}
	if asOf.IsZero() {
		return Balance{}, shared.NewValidationError("as_of", "snapshot instant is required")
	}
	return Balance{
		tenantID:       tenantID,
		availableCents: availableCents,
		blockedCents:   blockedCents,
		currency:       currency,
		asOf:           asOf,
	}, nil
}

// TenantID returns the owning tenant.
func (b Balance) TenantID() string { return b.tenantID }

// AvailableCents returns the amount available to spend, in cents. It may be negative
// when the account carries an overdraft line.
func (b Balance) AvailableCents() int64 { return b.availableCents }

// BlockedCents returns the (non-negative) blocked/reserved amount, in cents.
func (b Balance) BlockedCents() int64 { return b.blockedCents }

// Currency returns the ISO-4217 currency code (upper-cased).
func (b Balance) Currency() string { return b.currency }

// AsOf returns the instant the bank computed the snapshot.
func (b Balance) AsOf() time.Time { return b.asOf }
