package http

import (
	"net/http"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// --- Tenant API: account balance / saldo (C6 "Saldo & Extrato", roteiro grupo 13) ---
//
// The single handler derives the tenant from the authenticated context
// (tenantFromContext), never from the query or body: the saldo is always the
// authenticated tenant's. There is no parameter — a balance is the whole-account
// snapshot, so no window or selector is parsed (deny-by-default: the route is
// mounted inside the authenticated tenant group).

// balanceView is the JSON representation of an account-balance snapshot returned by
// GET /v1/balance. The instant is rendered RFC3339 (the saldo is a point-in-time
// value, unlike the extrato whose entries carry calendar dates).
type balanceView struct {
	AvailableCents int64  `json:"available_cents"`
	BlockedCents   int64  `json:"blocked_cents"`
	Currency       string `json:"currency"`
	AsOf           string `json:"as_of"`
}

// handleGetBalance returns the authenticated tenant's current account-balance
// snapshot (C6 "Saldo & Extrato", GET /v1/balance → 200). The tenant is derived from
// the credential; no parameter selects which tenant's saldo is read (threat H1/P1).
func (s *Server) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())

	res, err := s.balance.GetBalance(r.Context(), app.GetBalanceInput{TenantID: tenantID})
	if err != nil {
		writeDomainError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, toBalanceView(res))
}

// toBalanceView maps a port balance onto the tenant-facing view.
func toBalanceView(b ports.Balance) balanceView {
	return balanceView{
		AvailableCents: b.AvailableCents,
		BlockedCents:   b.BlockedCents,
		Currency:       b.Currency,
		AsOf:           b.AsOf.UTC().Format(time.RFC3339),
	}
}
