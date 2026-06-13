package c6

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// PIX immediate-charge (cobrança imediata) support for the C6 adapter.
//
// The lifecycle is: CreateImmediateCharge PUTs an idempotent charge keyed by a
// txid and gets back the QR code (copia-e-cola + render location) and expiry;
// GetImmediateCharge reconciles the authoritative status so settlement never
// trusts a raw webhook (reconcile-before-settle, threat W3).
//
// All PIX endpoints live here so the use-cases never speak HTTP/JSON or know the
// PSP's wire shape (Hexagonal). Security posture is inherited from the foundation
// (HTTPS-only, TLS>=1.2, per-tenant OAuth2 bearer, size-capped responses, no
// secret/PII in errors).

const (
	// defaultPixExpiry is the immediate-charge QR lifetime applied when the caller
	// passes a non-positive expiresIn.
	defaultPixExpiry = time.Hour

	// pixTxIDLen is the length of the derived txid. BACEN requires a txid of 26..35
	// characters from [a-zA-Z0-9]; a 32-char hex digest sits safely inside that
	// range and uses only [0-9a-f].
	pixTxIDLen = 32
)

// compile-time assertion that Provider satisfies the PIX port.
var _ ports.PixProvider = (*Provider)(nil)

// pixCalendario is the charge schedule. Expiracao is the QR lifetime in seconds;
// Criacao is the PSP-assigned creation instant (RFC3339), present on reads.
type pixCalendario struct {
	Criacao   string `json:"criacao,omitempty"`
	Expiracao int64  `json:"expiracao"`
}

// pixValor carries the charge amount as the BACEN decimal string ("10.00").
type pixValor struct {
	Original string `json:"original"`
}

// pixLoc is the QR-code location descriptor returned by the PSP.
type pixLoc struct {
	Location string `json:"location"`
}

// pixChargeRequestBody is the JSON sent to C6 to create an immediate PIX charge.
type pixChargeRequestBody struct {
	Calendario pixCalendario `json:"calendario"`
	Valor      pixValor      `json:"valor"`
}

// pixChargeResponseBody is the subset of C6's PIX charge representation we
// consume. Human-readable / unmodelled fields are ignored on purpose.
type pixChargeResponseBody struct {
	TxID          string        `json:"txid"`
	Status        string        `json:"status"`
	Calendario    pixCalendario `json:"calendario"`
	Loc           pixLoc        `json:"loc"`
	PixCopiaECola string        `json:"pixCopiaECola"`
}

// CreateImmediateCharge creates an immediate PIX charge at C6 via an idempotent
// PUT on /v1/pix/{txid}. The txid is derived deterministically from the request's
// idempotency anchor (IdempotencyKey, falling back to PaymentID), so a re-submit
// targets the very same resource and the PSP returns the existing charge rather
// than creating a duplicate. The caller's idempotency key is additionally
// forwarded as the Idempotency-Key header (F3b defense-in-depth, SIN-64720).
func (p *Provider) CreateImmediateCharge(ctx context.Context, tenantID string, req ports.ChargeRequest, expiresIn time.Duration) (ports.PixChargeResult, error) {
	token, err := p.tokens.token(ctx, tenantID)
	if err != nil {
		return ports.PixChargeResult{}, err
	}

	// Complete mediation at the money-movement seam: refuse to derive a txid from
	// an empty idempotency anchor. idempotencyKey(req) is "" only when BOTH
	// IdempotencyKey and PaymentID are empty; deriving from "" would make every
	// such charge share the constant txid sha256("")[:32], so two distinct charges
	// would collide on the idempotent PUT and the PSP would return the first one —
	// a silent wrong-amount. Fail securely here rather than trusting upstream to
	// always supply an anchor. Likewise reject a non-positive amount at the
	// boundary: there is no domain guard upstream and a <=0 valor is never a valid
	// PIX charge (SIN-64769, SEC-1/SEC-3).
	if idempotencyKey(req) == "" {
		return ports.PixChargeResult{}, &Error{Op: "create_pix", sentinel: shared.ErrValidation}
	}
	if req.AmountCents <= 0 {
		return ports.PixChargeResult{}, &Error{Op: "create_pix", sentinel: shared.ErrValidation}
	}

	if expiresIn <= 0 {
		expiresIn = defaultPixExpiry
	}
	txid := pixTxID(req)

	payload, err := json.Marshal(pixChargeRequestBody{
		Calendario: pixCalendario{Expiracao: int64(expiresIn / time.Second)},
		Valor:      pixValor{Original: formatAmount(req.AmountCents)},
	})
	if err != nil {
		return ports.PixChargeResult{}, &Error{Op: "create_pix", sentinel: shared.ErrValidation}
	}

	endpoint := p.baseURL + "/v1/pix/" + url.PathEscape(txid)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return ports.PixChargeResult{}, transportError("create_pix")
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if idem := idempotencyKey(req); idem != "" {
		httpReq.Header.Set("Idempotency-Key", idem)
	}

	var out pixChargeResponseBody
	if err := p.do(httpReq, "create_pix", &out); err != nil {
		return ports.PixChargeResult{}, err
	}
	return p.toPixResult(out), nil
}

// GetImmediateCharge reconciles the authoritative state of a PIX charge from C6.
// This is the source of truth for settlement: a webhook may announce a payment,
// but the charge status (and whether it expired) is always read back here, never
// trusted from the raw event (reconcile-before-settle, threat W3).
func (p *Provider) GetImmediateCharge(ctx context.Context, tenantID, txID string) (ports.PixChargeResult, error) {
	token, err := p.tokens.token(ctx, tenantID)
	if err != nil {
		return ports.PixChargeResult{}, err
	}

	endpoint := p.baseURL + "/v1/pix/" + url.PathEscape(txID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ports.PixChargeResult{}, transportError("get_pix")
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/json")

	var out pixChargeResponseBody
	if err := p.do(httpReq, "get_pix", &out); err != nil {
		return ports.PixChargeResult{}, err
	}
	return p.toPixResult(out), nil
}

// toPixResult maps the PSP wire shape into the port result, computing the QR
// expiry from the charge calendar.
func (p *Provider) toPixResult(b pixChargeResponseBody) ports.PixChargeResult {
	return ports.PixChargeResult{
		TxID:           b.TxID,
		Status:         b.Status,
		QRCodePayload:  b.PixCopiaECola,
		QRCodeLocation: b.Loc.Location,
		ExpiresAt:      p.pixExpiresAt(b.Calendario),
	}
}

// pixExpiresAt derives the QR expiry instant: the PSP-assigned creation time plus
// the expiracao window. When the PSP omits the creation timestamp (e.g. on the
// create response) the adapter clock is used as the base. A non-positive window
// yields the zero time, signalling "no expiry reported".
func (p *Provider) pixExpiresAt(c pixCalendario) time.Time {
	if c.Expiracao <= 0 {
		return time.Time{}
	}
	base := p.now()
	if c.Criacao != "" {
		if t, err := time.Parse(time.RFC3339, c.Criacao); err == nil {
			base = t
		}
	}
	return base.Add(time.Duration(c.Expiracao) * time.Second)
}

// pixTxID derives a BACEN-valid (26..35 chars, [a-zA-Z0-9]) txid from the
// request's idempotency anchor. Being deterministic, it makes the create PUT
// idempotent end-to-end: the same anchor always addresses the same charge.
func pixTxID(req ports.ChargeRequest) string {
	sum := sha256.Sum256([]byte(idempotencyKey(req)))
	return hex.EncodeToString(sum[:])[:pixTxIDLen]
}

// formatAmount renders integer cents as the BACEN decimal string (e.g. 1050 ->
// "10.50"), matching the PIX valor.original contract.
func formatAmount(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}
