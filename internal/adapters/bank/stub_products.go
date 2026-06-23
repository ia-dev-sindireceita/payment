package bank

import (
	"context"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// This file extends StubProvider to back the C6-C product ports (PIX Automático
// consent, BolePix boleto, unified checkout) in-memory, so wiring and use-cases
// run end-to-end without an external dependency. Like CreateCharge, every method
// resolves the tenant's credential first to demonstrate per-tenant isolation; the
// secret is never logged.

// compile-time assertions that StubProvider satisfies the C6-C product ports.
var (
	_ ports.ConsentProvider  = (*StubProvider)(nil)
	_ ports.BoletoProvider   = (*StubProvider)(nil)
	_ ports.CheckoutProvider = (*StubProvider)(nil)
)

// CreateConsent registers a recurring-debit consent deterministically. A repeat
// call for the same (tenant, consent id) returns the existing consent rather than
// creating a new one, modelling PSP-side idempotency.
func (s *StubProvider) CreateConsent(ctx context.Context, tenantID string, req ports.ConsentRequest) (ports.ConsentResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.ConsentResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(tenantID, req.ConsentID)
	if prev, ok := s.consents[k]; ok {
		return prev, nil
	}
	res := ports.ConsentResult{ConsentID: req.ConsentID, Status: "PENDING"}
	s.consents[k] = res
	return res, nil
}

// GetConsent returns the authoritative state of a consent for reconciliation.
func (s *StubProvider) GetConsent(ctx context.Context, tenantID, consentID string) (ports.ConsentResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.ConsentResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.consents[key(tenantID, consentID)]
	if !ok {
		return ports.ConsentResult{}, shared.ErrNotFound
	}
	return res, nil
}

// CancelConsent revokes a consent. Cancelling is idempotent: a second cancel of an
// already-cancelled consent succeeds and returns the cancelled state.
func (s *StubProvider) CancelConsent(ctx context.Context, tenantID, consentID string) (ports.ConsentResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.ConsentResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(tenantID, consentID)
	res, ok := s.consents[k]
	if !ok {
		return ports.ConsentResult{}, shared.ErrNotFound
	}
	res.Status = "CANCELLED"
	s.consents[k] = res
	return res, nil
}

// CreateBoleto registers a boleto deterministically, deriving a txid from the
// boleto id and returning placeholder scannable artifacts. The registered state
// (including the fine/interest/discount parameters) is retained so GetBoleto can
// reconcile it (roteiro 6.a). Registration is idempotent on (tenant, boleto id): a
// repeat call returns the existing record rather than registering a new boleto.
func (s *StubProvider) CreateBoleto(ctx context.Context, tenantID string, req ports.BoletoRequest) (ports.BoletoResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.BoletoResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(tenantID, req.BoletoID)
	if prev, ok := s.boletos[k]; ok {
		return prev, nil // PSP dedupe: same (tenant, id) => same boleto, no new effect
	}
	res := ports.BoletoResult{
		BoletoID:           req.BoletoID,
		TxID:               "tx_" + req.BoletoID,
		Status:             "REGISTERED",
		QRCode:             "pix-emv-" + req.BoletoID,
		Barcode:            "barcode-" + req.BoletoID,
		AmountCents:        req.AmountCents,
		DueDate:            req.DueDate,
		FineBps:            req.FineBps,
		FineFixedCents:     req.FineFixedCents,
		MonthlyInterestBps: req.MonthlyInterestBps,
		Discounts:          req.Discounts,
	}
	s.boletos[k] = res
	return res, nil
}

// GetBoleto returns the authoritative state of a registered boleto for the tenant
// (roteiro 6.a). An unknown id within the tenant is shared.ErrNotFound; the read is
// keyed by (tenant, id) so one tenant can never observe another's boleto.
func (s *StubProvider) GetBoleto(ctx context.Context, tenantID, boletoID string) (ports.BoletoResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.BoletoResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.boletos[key(tenantID, boletoID)]
	if !ok {
		return ports.BoletoResult{}, shared.ErrNotFound
	}
	return res, nil
}

// CreateCheckoutSession opens a checkout session deterministically, summing the
// item amounts and returning a placeholder hosted redirect URL.
func (s *StubProvider) CreateCheckoutSession(ctx context.Context, tenantID string, req ports.CheckoutRequest) (ports.CheckoutResult, error) {
	if _, err := s.creds.GetBankCredential(ctx, tenantID); err != nil {
		return ports.CheckoutResult{}, err
	}
	var sum int64
	for _, it := range req.Items {
		sum += it.AmountCents
	}
	return ports.CheckoutResult{
		SessionID:             req.SessionID,
		Status:                "OPEN",
		RedirectURL:           "https://checkout.c6.example/" + req.SessionID,
		AmountCents:           sum,
		CardType:              req.CardType,
		RequireAuthentication: req.RequireAuthentication,
	}, nil
}
