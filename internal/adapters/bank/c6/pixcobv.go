package c6

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// PIX cobrança-com-vencimento (cobv) support for the C6 adapter (roteiro 7.5–7.7).
//
// The lifecycle mirrors the immediate charge: CreateDueCharge PUTs an idempotent
// charge keyed by a txid derived from the idempotency anchor and gets back the QR
// material; GetDueCharge reconciles the authoritative status so settlement never
// trusts a raw webhook (reconcile-before-settle, threat W3); UpdateDueCharge amends
// the registered parameters. All cobv endpoints live here so the use-cases never
// speak HTTP/JSON or know the PSP wire shape (Hexagonal).
//
// The JSON shape below is the adapter's clean internal contract (snake_case,
// explicit bps/cents/days), mirroring the boleto adapter: it round-trips exactly so
// Camada A (stub mode) is deterministic. Camada B maps it to the real BACEN cobv
// wire (calendario.dataDeVencimento, valor.multa/juros/desconto, chave) against the
// homologação endpoint; that translation does not change this port's surface.

// compile-time assertion that Provider satisfies the cobv port.
var _ ports.PixDueChargeProvider = (*Provider)(nil)

// cobvRequestBody is the JSON sent to C6 to register or amend a cobv charge.
type cobvRequestBody struct {
	TxID               string    `json:"txid"`
	AmountCents        int64     `json:"amount_cents"`
	Currency           string    `json:"currency"`
	DueDate            time.Time `json:"due_date"`
	ValidityDays       int       `json:"validity_days"`
	FineBps            int64     `json:"fine_bps"`
	MonthlyInterestBps int64     `json:"monthly_interest_bps"`
	DiscountBps        int64     `json:"discount_bps,omitempty"`
	DiscountFixedCents int64     `json:"discount_fixed_cents,omitempty"`
	DebtorTaxID        string    `json:"debtor_tax_id"`
	DebtorName         string    `json:"debtor_name"`
	CreditorKey        string    `json:"creditor_key"`
}

// cobvResponseBody is the subset of C6's cobv representation we consume: the status
// plus the scannable QR artifacts, the registered parameters echoed back for
// reconciliation (roteiro 7.6) and the money needed to reconcile a settlement
// (amount_cents = expected, received_amount_cents = what was paid).
type cobvResponseBody struct {
	TxID                string    `json:"txid"`
	Status              string    `json:"status"`
	QRCode              string    `json:"qr_code"`
	QRCodeLocation      string    `json:"qr_code_location"`
	DueDate             time.Time `json:"due_date"`
	ValidityDays        int       `json:"validity_days"`
	FineBps             int64     `json:"fine_bps"`
	MonthlyInterestBps  int64     `json:"monthly_interest_bps"`
	DiscountBps         int64     `json:"discount_bps"`
	DiscountFixedCents  int64     `json:"discount_fixed_cents"`
	AmountCents         int64     `json:"amount_cents"`
	ReceivedAmountCents int64     `json:"received_amount_cents"`
}

// cobvAnchor returns the request's idempotency anchor: the IdempotencyKey when
// present, else the TxID. Empty only when both are empty.
func cobvAnchor(req ports.PixDueChargeRequest) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return req.TxID
}

// cobvTxID derives a BACEN-valid (32 hex chars, inside the 26..35 [a-zA-Z0-9]
// range) txid from the request's idempotency anchor. Being deterministic, it makes
// the create PUT idempotent end-to-end: the same anchor always addresses the same
// charge.
func cobvTxID(anchor string) string {
	sum := sha256.Sum256([]byte(anchor))
	return hex.EncodeToString(sum[:])[:pixTxIDLen]
}

// toCobvRequestBody maps the port request to the adapter transport JSON (shared by
// register and amend). The txid is supplied explicitly so amend addresses the known
// resource.
func toCobvRequestBody(txID string, req ports.PixDueChargeRequest) cobvRequestBody {
	return cobvRequestBody{
		TxID:               txID,
		AmountCents:        req.AmountCents,
		Currency:           req.Currency,
		DueDate:            req.DueDate,
		ValidityDays:       req.ValidityDays,
		FineBps:            req.FineBps,
		MonthlyInterestBps: req.MonthlyInterestBps,
		DiscountBps:        req.DiscountBps,
		DiscountFixedCents: req.DiscountFixedCents,
		DebtorTaxID:        strings.TrimSpace(req.DebtorTaxID),
		DebtorName:         strings.TrimSpace(req.DebtorName),
		CreditorKey:        strings.TrimSpace(req.CreditorKey),
	}
}

// toCobvResult maps a parsed C6 cobv representation to the port result.
func toCobvResult(out cobvResponseBody) ports.PixDueChargeResult {
	return ports.PixDueChargeResult{
		TxID:                out.TxID,
		Status:              out.Status,
		QRCodePayload:       out.QRCode,
		QRCodeLocation:      out.QRCodeLocation,
		DueDate:             out.DueDate,
		ValidityDays:        out.ValidityDays,
		FineBps:             out.FineBps,
		MonthlyInterestBps:  out.MonthlyInterestBps,
		DiscountBps:         out.DiscountBps,
		DiscountFixedCents:  out.DiscountFixedCents,
		ExpectedAmountCents: out.AmountCents,
		ReceivedAmountCents: out.ReceivedAmountCents,
	}
}

// CreateDueCharge registers a cobv charge at C6 via an idempotent PUT on
// /v1/pix/cobv/{txid}. The txid is derived deterministically from the idempotency
// anchor so a re-submit targets the same resource and the PSP returns the existing
// charge rather than creating a duplicate. The anchor is additionally forwarded as
// the Idempotency-Key header (defense-in-depth). The bearer token is attached per
// tenant. Complete mediation at the money seam: an empty anchor or a non-positive
// amount is refused at the boundary (no domain guard upstream guarantees it).
func (p *Provider) CreateDueCharge(ctx context.Context, tenantID string, req ports.PixDueChargeRequest) (ports.PixDueChargeResult, error) {
	anchor := cobvAnchor(req)
	if anchor == "" || req.AmountCents <= 0 {
		return ports.PixDueChargeResult{}, &Error{Op: "create_cobv", sentinel: shared.ErrValidation}
	}
	txid := cobvTxID(anchor)

	payload, err := json.Marshal(toCobvRequestBody(txid, req))
	if err != nil {
		return ports.PixDueChargeResult{}, &Error{Op: "create_cobv", sentinel: shared.ErrValidation}
	}

	endpoint := p.baseURL + "/v1/pix/cobv/" + url.PathEscape(txid)
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "create_cobv", http.MethodPut, endpoint, payload, anchor)
	if err != nil {
		return ports.PixDueChargeResult{}, err
	}

	var out cobvResponseBody
	if err := p.do(httpReq, "create_cobv", &out); err != nil {
		return ports.PixDueChargeResult{}, err
	}
	return toCobvResult(out), nil
}

// GetDueCharge reconciles the authoritative state of a cobv charge from C6. This is
// the source of truth for settlement: a webhook may announce a payment, but the
// charge status is always read back here, never trusted from the raw event
// (reconcile-before-settle, threat W3). A 404 surfaces as shared.ErrNotFound; the
// read is tenant-scoped through the per-tenant OAuth2 bearer token.
func (p *Provider) GetDueCharge(ctx context.Context, tenantID, txID string) (ports.PixDueChargeResult, error) {
	endpoint := p.baseURL + "/v1/pix/cobv/" + url.PathEscape(txID)
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "get_cobv", http.MethodGet, endpoint, nil, "")
	if err != nil {
		return ports.PixDueChargeResult{}, err
	}

	var out cobvResponseBody
	if err := p.do(httpReq, "get_cobv", &out); err != nil {
		return ports.PixDueChargeResult{}, err
	}
	return toCobvResult(out), nil
}

// UpdateDueCharge amends a registered cobv's parameters at C6 (roteiro 7.7) via PUT
// on /v1/pix/cobv/{txid}. The caller's IdempotencyKey (falling back to the txid) is
// forwarded so a retried amendment is collapsed. A 404 surfaces as
// shared.ErrNotFound; the operation is tenant-scoped through the per-tenant bearer.
func (p *Provider) UpdateDueCharge(ctx context.Context, tenantID, txID string, req ports.PixDueChargeRequest) (ports.PixDueChargeResult, error) {
	if req.AmountCents <= 0 {
		return ports.PixDueChargeResult{}, &Error{Op: "update_cobv", sentinel: shared.ErrValidation}
	}
	payload, err := json.Marshal(toCobvRequestBody(txID, req))
	if err != nil {
		return ports.PixDueChargeResult{}, &Error{Op: "update_cobv", sentinel: shared.ErrValidation}
	}

	idem := req.IdempotencyKey
	if idem == "" {
		idem = txID
	}
	endpoint := p.baseURL + "/v1/pix/cobv/" + url.PathEscape(txID)
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "update_cobv", http.MethodPut, endpoint, payload, idem)
	if err != nil {
		return ports.PixDueChargeResult{}, err
	}

	var out cobvResponseBody
	if err := p.do(httpReq, "update_cobv", &out); err != nil {
		return ports.PixDueChargeResult{}, err
	}
	return toCobvResult(out), nil
}
