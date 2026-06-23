package c6

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// compile-time assertion that Provider satisfies the boleto port.
var _ ports.BoletoProvider = (*Provider)(nil)

// boletoDiscountBody is the transport JSON for one early-payment discount tier
// (roteiro grupo 3). Exactly one of Bps/FixedCents is non-zero.
type boletoDiscountBody struct {
	DaysBeforeDue int   `json:"days_before_due"`
	Bps           int64 `json:"bps,omitempty"`
	FixedCents    int64 `json:"fixed_cents,omitempty"`
}

// boletoRequestBody is the JSON sent to C6 to register a BolePix boleto. The fine,
// interest and discount RATES are transmitted so C6 registers them; the amount owed
// at any instant is computed by the boleto domain, never here (Hexagonal).
type boletoRequestBody struct {
	BoletoID           string               `json:"boleto_id"`
	AmountCents        int64                `json:"amount_cents"`
	Currency           string               `json:"currency"`
	DueDate            time.Time            `json:"due_date"`
	FineBps            int64                `json:"fine_bps"`
	FineFixedCents     int64                `json:"fine_fixed_cents,omitempty"`
	MonthlyInterestBps int64                `json:"monthly_interest_bps"`
	Discounts          []boletoDiscountBody `json:"discounts,omitempty"`
	PayerTaxID         string               `json:"payer_tax_id"`
}

// boletoResponseBody is the subset of C6's boleto representation we consume: the
// status plus the scannable artifacts (PIX EMV payload and boleto barcode) and the
// registered parameters echoed back for reconciliation (roteiro 6.a).
type boletoResponseBody struct {
	BoletoID           string               `json:"boleto_id"`
	TxID               string               `json:"txid"`
	Status             string               `json:"status"`
	QRCode             string               `json:"qr_code"`
	Barcode            string               `json:"barcode"`
	AmountCents        int64                `json:"amount_cents"`
	DueDate            time.Time            `json:"due_date"`
	FineBps            int64                `json:"fine_bps"`
	FineFixedCents     int64                `json:"fine_fixed_cents"`
	MonthlyInterestBps int64                `json:"monthly_interest_bps"`
	Discounts          []boletoDiscountBody `json:"discounts"`
}

// toDiscountBodies maps the port discount tiers to their transport JSON.
func toDiscountBodies(in []ports.BoletoDiscountTier) []boletoDiscountBody {
	if len(in) == 0 {
		return nil
	}
	out := make([]boletoDiscountBody, len(in))
	for i, d := range in {
		out[i] = boletoDiscountBody{DaysBeforeDue: d.DaysBeforeDue, Bps: d.Bps, FixedCents: d.FixedCents}
	}
	return out
}

// fromDiscountBodies maps the transport discount JSON back to the port tiers.
func fromDiscountBodies(in []boletoDiscountBody) []ports.BoletoDiscountTier {
	if len(in) == 0 {
		return nil
	}
	out := make([]ports.BoletoDiscountTier, len(in))
	for i, d := range in {
		out[i] = ports.BoletoDiscountTier{DaysBeforeDue: d.DaysBeforeDue, Bps: d.Bps, FixedCents: d.FixedCents}
	}
	return out
}

// toBoletoResult maps a parsed C6 boleto representation to the port result.
func toBoletoResult(out boletoResponseBody) ports.BoletoResult {
	return ports.BoletoResult{
		BoletoID:           out.BoletoID,
		TxID:               out.TxID,
		Status:             out.Status,
		QRCode:             out.QRCode,
		Barcode:            out.Barcode,
		AmountCents:        out.AmountCents,
		DueDate:            out.DueDate,
		FineBps:            out.FineBps,
		FineFixedCents:     out.FineFixedCents,
		MonthlyInterestBps: out.MonthlyInterestBps,
		Discounts:          fromDiscountBodies(out.Discounts),
	}
}

// CreateBoleto registers a BolePix boleto at C6 and returns the scannable
// artifacts (PIX copia-e-cola payload and barcode). The caller's IdempotencyKey
// (falling back to the BoletoID) is forwarded so the PSP collapses retried
// registrations into one boleto. The OAuth2 bearer token is attached per tenant.
func (p *Provider) CreateBoleto(ctx context.Context, tenantID string, req ports.BoletoRequest) (ports.BoletoResult, error) {
	payload, err := json.Marshal(boletoRequestBody{
		BoletoID:           req.BoletoID,
		AmountCents:        req.AmountCents,
		Currency:           req.Currency,
		DueDate:            req.DueDate,
		FineBps:            req.FineBps,
		FineFixedCents:     req.FineFixedCents,
		MonthlyInterestBps: req.MonthlyInterestBps,
		Discounts:          toDiscountBodies(req.Discounts),
		PayerTaxID:         req.PayerTaxID,
	})
	if err != nil {
		return ports.BoletoResult{}, &Error{Op: "create_boleto", sentinel: shared.ErrValidation}
	}

	idem := req.IdempotencyKey
	if idem == "" {
		idem = req.BoletoID
	}
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "create_boleto", http.MethodPost, p.baseURL+"/boletos", payload, idem)
	if err != nil {
		return ports.BoletoResult{}, err
	}

	var out boletoResponseBody
	if err := p.do(httpReq, "create_boleto", &out); err != nil {
		return ports.BoletoResult{}, err
	}
	return toBoletoResult(out), nil
}

// GetBoleto reconciles the authoritative state of a registered boleto from C6
// (roteiro 6.a). A 404 surfaces as shared.ErrNotFound via the adapter's error
// mapping; the read is tenant-scoped through the per-tenant OAuth2 bearer token, so
// one tenant can never read another's boleto.
func (p *Provider) GetBoleto(ctx context.Context, tenantID, boletoID string) (ports.BoletoResult, error) {
	endpoint := p.baseURL + "/boletos/" + url.PathEscape(boletoID)
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "get_boleto", http.MethodGet, endpoint, nil, "")
	if err != nil {
		return ports.BoletoResult{}, err
	}

	var out boletoResponseBody
	if err := p.do(httpReq, "get_boleto", &out); err != nil {
		return ports.BoletoResult{}, err
	}
	return toBoletoResult(out), nil
}
