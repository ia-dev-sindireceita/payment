package c6

import (
	"context"
	"net/http"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// Account-balance (saldo) support for the C6 adapter (C6 "Saldo & Extrato", roteiro
// grupo 13).
//
// GetBalance reads the tenant's current account balance. It lives here so the
// use-case never speaks HTTP/JSON or knows the PSP wire shape (Hexagonal), mirroring
// statement.go.
//
// The JSON shape below is the adapter's clean internal contract (snake_case, explicit
// cents, RFC3339 instant): it round-trips exactly so Camada A (stub mode) is
// deterministic. Camada B maps it to the real C6 /balance wire against the
// homologação endpoint; that translation does not change this port's surface.
//
// NOTE (SIN-72423): the exact /balance path and response shape must be confirmed
// against the live C6 portal/OAS + homologação (SIN-65856) before this is treated as
// the final contract — the field names here are the adapter's provisional internal
// contract, not a copy of a verified C6 payload.

// compile-time assertion that Provider satisfies the balance port.
var _ ports.BalanceProvider = (*Provider)(nil)

// balanceResponse is the adapter's internal wire shape for a balance snapshot.
type balanceResponse struct {
	AvailableCents int64     `json:"available_cents"`
	BlockedCents   int64     `json:"blocked_cents"`
	Currency       string    `json:"currency"`
	AsOf           time.Time `json:"as_of"`
}

// toBalance maps the wire balance to the port type.
func toBalance(in balanceResponse) ports.Balance {
	return ports.Balance{
		AvailableCents: in.AvailableCents,
		BlockedCents:   in.BlockedCents,
		Currency:       in.Currency,
		AsOf:           in.AsOf,
	}
}

// GetBalance reads the tenant's current account balance (C6 "Saldo & Extrato"
// /balance). The bearer token is attached per tenant; the read is tenant-scoped
// through it — no parameter selects which tenant's saldo is read (threat H1/P1).
func (p *Provider) GetBalance(ctx context.Context, tenantID string) (ports.Balance, error) {
	endpoint := p.baseURL + "/v1/balance"
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "get_balance", http.MethodGet, endpoint, nil, "")
	if err != nil {
		return ports.Balance{}, err
	}
	var out balanceResponse
	if err := p.do(httpReq, "get_balance", &out); err != nil {
		return ports.Balance{}, err
	}
	return toBalance(out), nil
}
