package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ia-dev-sindireceita/payment/internal/domain/billing"
	"github.com/ia-dev-sindireceita/payment/internal/domain/payment"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// ChargeService creates and reads charges. It enforces idempotency (no double
// charge on retry), tenant scoping (no IDOR), and per-endpoint billing.
type ChargeService struct {
	payments ports.PaymentRepository
	tenants  ports.TenantRepository
	pricing  ports.PricingRepository
	ledger   ports.LedgerRepository
	bank     ports.BankProvider
	bus      ports.MessageBus
	clock    ports.Clock
	ids      ports.IDProvider
}

// NewChargeService wires a ChargeService from the provided ports.
func NewChargeService(d Deps) *ChargeService {
	return &ChargeService{
		payments: d.Payments,
		tenants:  d.Tenants,
		pricing:  d.Pricing,
		ledger:   d.Ledger,
		bank:     d.Bank,
		bus:      d.Bus,
		clock:    d.Clock,
		ids:      d.IDs,
	}
}

// CreateChargeInput is the validated boundary input for creating a charge. The
// TenantID is the authenticated tenant, never a client-supplied field.
type CreateChargeInput struct {
	TenantID       string
	Endpoint       string
	AmountCents    int64
	Currency       string
	IdempotencyKey string
}

// CreateCharge creates a pending charge at the bank and records the billable
// event. Retrying with the same idempotency key returns the original payment
// without charging again.
func (s *ChargeService) CreateCharge(ctx context.Context, in CreateChargeInput) (*payment.Payment, error) {
	t, err := s.tenants.FindTenantByID(ctx, in.TenantID)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant: %w", err)
	}
	if !t.Active() {
		return nil, shared.NewValidationError("tenant", "tenant is not active")
	}

	// Idempotency: a prior request with the same key returns the same result.
	if in.IdempotencyKey == "" {
		return nil, shared.NewValidationError("idempotency_key", "idempotency key is required")
	}
	existing, err := s.payments.FindPaymentByIdempotencyKey(ctx, in.TenantID, in.IdempotencyKey)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, shared.ErrNotFound) {
		return nil, fmt.Errorf("idempotency lookup: %w", err)
	}

	// Resolve per-endpoint price (config error if the endpoint isn't priced).
	price, err := s.pricing.GetEndpointPrice(ctx, in.TenantID, in.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("resolve price: %w", err)
	}

	amount, err := shared.NewMoney(in.AmountCents, in.Currency)
	if err != nil {
		return nil, err
	}

	now := s.clock.Now()
	p, err := payment.New(s.ids.NewID(), in.TenantID, in.Endpoint, in.IdempotencyKey, amount, now)
	if err != nil {
		return nil, err
	}

	res, err := s.bank.CreateCharge(ctx, in.TenantID, ports.ChargeRequest{
		TenantID:    in.TenantID,
		PaymentID:   p.ID(),
		AmountCents: amount.Cents(),
		Currency:    amount.Currency(),
	})
	if err != nil {
		return nil, fmt.Errorf("bank create charge: %w", err)
	}
	p.SetTxID(res.TxID)

	if err := s.payments.SavePayment(ctx, p); err != nil {
		return nil, fmt.Errorf("save payment: %w", err)
	}

	// Append the billable event to the authoritative ledger.
	entry, err := billing.NewLedgerEntry(s.ids.NewID(), in.TenantID, in.Endpoint, p.ID(), price.PriceCents(), now)
	if err != nil {
		return nil, err
	}
	if err := s.ledger.AppendLedgerEntry(ctx, entry); err != nil {
		return nil, fmt.Errorf("append ledger: %w", err)
	}

	s.publishPaymentEvent(ctx, TopicPaymentCreated, p)
	return p, nil
}

// GetPayment returns a payment scoped to the authenticated tenant. A payment
// owned by another tenant surfaces as not-found (no cross-tenant disclosure).
func (s *ChargeService) GetPayment(ctx context.Context, tenantID, id string) (*payment.Payment, error) {
	p, err := s.payments.FindPaymentByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if p.TenantID() != tenantID {
		// Defense in depth: the repository already scopes by tenant.
		return nil, shared.ErrNotFound
	}
	return p, nil
}

func (s *ChargeService) publishPaymentEvent(ctx context.Context, topic string, p *payment.Payment) {
	payload, err := json.Marshal(struct {
		PaymentID string `json:"payment_id"`
		TenantID  string `json:"tenant_id"`
		Status    string `json:"status"`
		TxID      string `json:"tx_id"`
	}{p.ID(), p.TenantID(), string(p.Status()), p.TxID()})
	if err != nil {
		return
	}
	// Best-effort publish; failure to publish must not fail the persisted charge.
	_ = s.bus.Publish(ctx, topic, ports.Message{
		TenantID:       p.TenantID(),
		Type:           topic,
		IdempotencyKey: p.IdempotencyKey(),
		Payload:        payload,
	})
}
