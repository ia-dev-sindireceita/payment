package c6

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// PIX scheduled-charge (cobrança com vencimento, BACEN /cobv) support for the C6
// adapter (homologação roteiro 7.5–7.7). It is the dated-charge sibling of the
// immediate /pix (/cob) surface: CreateScheduledCharge PUTs an idempotent charge
// keyed by a deterministic txid carrying a dataDeVencimento + validadeAposVencimento
// calendar; Get/List reconcile authoritative state so settlement never trusts a raw
// webhook (reconcile-before-settle, threat W3). The cobv charge reuses the single-
// charge wire shape (pixChargeResponseBody) so toPixResult maps it identically; only
// the request calendar differs.

// cobvDateLayout is the BACEN dataDeVencimento format ("YYYY-MM-DD"): a cobv due date
// is a calendar date, not an instant.
const cobvDateLayout = "2006-01-02"

// pixCobvCalendario is the cobv schedule sent to the PSP: the due date and the number
// of days after it the charge may still be paid. It is distinct from pixCalendario
// (the immediate-charge Expiracao window).
type pixCobvCalendario struct {
	DataDeVencimento       string `json:"dataDeVencimento"`
	ValidadeAposVencimento int    `json:"validadeAposVencimento"`
}

// pixCobvRequestBody is the JSON sent to C6 to create a scheduled PIX charge. A cobv
// always carries a devedor (BACEN requires a named debtor on a dated charge); the
// use-case guarantees it is present before the request reaches here.
type pixCobvRequestBody struct {
	Calendario pixCobvCalendario `json:"calendario"`
	Devedor    *pixDevedor       `json:"devedor"`
	Valor      pixValor          `json:"valor"`
}

// CreateScheduledCharge creates a scheduled PIX charge at C6 via an idempotent PUT on
// /v2/cobv/{txid}. The txid is derived deterministically from the request's
// idempotency anchor (IdempotencyKey, falling back to PaymentID), so a re-submit
// targets the very same resource and the PSP returns the existing charge rather than
// creating a duplicate. The caller's idempotency key is additionally forwarded as the
// Idempotency-Key header (defense-in-depth).
func (p *Provider) CreateScheduledCharge(ctx context.Context, tenantID string, req ports.ScheduledChargeRequest) (ports.PixChargeResult, error) {
	// Complete mediation at the money-movement seam, mirroring the immediate path:
	// refuse an empty idempotency anchor (would collide every such charge on the
	// constant txid sha256("")[:32]), a non-positive amount, and a missing due date.
	anchor := scheduledAnchor(req)
	if anchor == "" {
		return ports.PixChargeResult{}, &Error{Op: "create_cobv", sentinel: shared.ErrValidation}
	}
	if req.AmountCents <= 0 {
		return ports.PixChargeResult{}, &Error{Op: "create_cobv", sentinel: shared.ErrValidation}
	}
	if req.DueDate.IsZero() || req.ValidityAfterDueDays < 0 {
		return ports.PixChargeResult{}, &Error{Op: "create_cobv", sentinel: shared.ErrValidation}
	}

	payload, err := json.Marshal(pixCobvRequestBody{
		Calendario: pixCobvCalendario{
			DataDeVencimento:       req.DueDate.UTC().Format(cobvDateLayout),
			ValidadeAposVencimento: req.ValidityAfterDueDays,
		},
		Devedor: buildDevedorFields(req.DebtorTaxID, req.DebtorName),
		Valor:   pixValor{Original: formatAmount(req.AmountCents)},
	})
	if err != nil {
		return ports.PixChargeResult{}, &Error{Op: "create_cobv", sentinel: shared.ErrValidation}
	}

	txid := scheduledTxID(anchor)
	endpoint := p.baseURL + "/v2/cobv/" + url.PathEscape(txid)
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "create_cobv", http.MethodPut, endpoint, payload, anchor)
	if err != nil {
		return ports.PixChargeResult{}, err
	}

	var out pixChargeResponseBody
	if err := p.do(httpReq, "create_cobv", &out); err != nil {
		return ports.PixChargeResult{}, err
	}
	return p.toPixResult(out, "create_cobv")
}

// GetScheduledCharge reconciles the authoritative state of a scheduled PIX charge
// from C6 via GET /v2/cobv/{txid}. A 404 surfaces as shared.ErrNotFound; the read is
// tenant-scoped through the per-tenant OAuth2 bearer token.
func (p *Provider) GetScheduledCharge(ctx context.Context, tenantID, txID string) (ports.PixChargeResult, error) {
	endpoint := p.baseURL + "/v2/cobv/" + url.PathEscape(txID)
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "get_cobv", http.MethodGet, endpoint, nil, "")
	if err != nil {
		return ports.PixChargeResult{}, err
	}

	var out pixChargeResponseBody
	if err := p.do(httpReq, "get_cobv", &out); err != nil {
		return ports.PixChargeResult{}, err
	}
	return p.toPixResult(out, "get_cobv")
}

// ListScheduledCharges lists the scheduled PIX charges created within [Start,End] via
// GET /v2/cobv?inicio=…&fim=… (BACEN cobv list, roteiro 7.6). The window bounds are
// mandatory and rendered as RFC3339 UTC instants; pagination is forwarded only when
// supplied. Like the single-charge reads it is fail-secure on the money: a malformed
// amount in any cob maps to ErrUnavailable rather than reconciling to zero.
func (p *Provider) ListScheduledCharges(ctx context.Context, tenantID string, filter ports.PixListFilter) (ports.PixChargeList, error) {
	if filter.Start.IsZero() || filter.End.IsZero() {
		return ports.PixChargeList{}, &Error{Op: "list_cobv", sentinel: shared.ErrValidation}
	}

	q := url.Values{}
	q.Set("inicio", filter.Start.UTC().Format(time.RFC3339))
	q.Set("fim", filter.End.UTC().Format(time.RFC3339))
	if filter.Page > 0 {
		q.Set("paginacao.paginaAtual", strconv.Itoa(filter.Page))
	}
	if filter.PageSize > 0 {
		q.Set("paginacao.itensPorPagina", strconv.Itoa(filter.PageSize))
	}

	endpoint := p.baseURL + "/v2/cobv?" + q.Encode()
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "list_cobv", http.MethodGet, endpoint, nil, "")
	if err != nil {
		return ports.PixChargeList{}, err
	}

	var out pixListResponseBody
	if err := p.do(httpReq, "list_cobv", &out); err != nil {
		return ports.PixChargeList{}, err
	}

	charges := make([]ports.PixChargeResult, 0, len(out.Cobs))
	for _, cob := range out.Cobs {
		r, err := p.toPixResult(cob, "list_cobv")
		if err != nil {
			return ports.PixChargeList{}, err
		}
		charges = append(charges, r)
	}
	return ports.PixChargeList{
		Charges:    charges,
		Page:       out.Parametros.Paginacao.PaginaAtual,
		PageSize:   out.Parametros.Paginacao.ItensPorPagina,
		TotalItems: out.Parametros.Paginacao.QuantidadeTotalDeItens,
		TotalPages: out.Parametros.Paginacao.QuantidadeDePaginas,
	}, nil
}

// scheduledAnchor returns the request's idempotency anchor: the IdempotencyKey when
// present, else the PaymentID.
func scheduledAnchor(req ports.ScheduledChargeRequest) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return req.PaymentID
}

// scheduledTxID derives a BACEN-valid (26..35 chars, [a-zA-Z0-9]) txid from the
// idempotency anchor. Being deterministic, it makes the create PUT idempotent
// end-to-end: the same anchor always addresses the same charge.
func scheduledTxID(anchor string) string {
	sum := sha256.Sum256([]byte(anchor))
	return hex.EncodeToString(sum[:])[:pixTxIDLen]
}
