package c6

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// PIX webhook registration (BACEN PUT /v2/webhook/{chave}) for the C6 adapter
// (homologação roteiro 7.8). It configures, per PIX key (chave), the URL the PSP
// notifies when a PIX is received. The operation is idempotent (the chave keys the
// configuration) so a retried registration overwrites without a duplicate effect.
//
// Security: the callback URL is a capability secret. It travels ONLY in the request
// body — never in the request URL (the path carries the chave, not the URL) and never
// in an error (the adapter's Error type carries no URL/body by construction). The op
// label is "register_pix_webhook"; nothing here logs the webhook URL.

// pixWebhookRequestBody is the JSON sent to C6 to register the notification URL for a
// PIX key. BACEN names the field webhookUrl.
type pixWebhookRequestBody struct {
	WebhookURL string `json:"webhookUrl"`
}

// RegisterPixWebhook registers webhookURL as the notification target for the tenant's
// PIX key (chave) via an idempotent PUT on /v2/webhook/{chave}. A 404 surfaces as
// shared.ErrNotFound (unknown key); the operation is tenant-scoped through the
// per-tenant OAuth2 bearer token. The URL is sent in the body and never logged.
func (p *Provider) RegisterPixWebhook(ctx context.Context, tenantID, chave, webhookURL string) error {
	payload, err := json.Marshal(pixWebhookRequestBody{WebhookURL: webhookURL})
	if err != nil {
		return &Error{Op: "register_pix_webhook", sentinel: shared.ErrValidation}
	}

	// The chave keys the (idempotent) configuration, so it doubles as the idempotency
	// anchor forwarded to the PSP.
	endpoint := p.baseURL + "/v2/webhook/" + url.PathEscape(chave)
	httpReq, err := p.authedJSONRequest(ctx, tenantID, "register_pix_webhook", http.MethodPut, endpoint, payload, chave)
	if err != nil {
		return err
	}
	return p.doNoContent(httpReq, "register_pix_webhook")
}

// doNoContent executes an authenticated request and maps a non-2xx into a domain
// error, but unlike do it does not require (or decode) a response body — the PSP's
// webhook-registration PUT answers with an empty 200/201/204. The body is still
// drained under the size cap so the connection can be reused, and raw body bytes
// never escape into an error.
func (p *Provider) doNoContent(req *http.Request, op string) error {
	resp, err := p.httpc.Do(req)
	if err != nil {
		return transportError(op)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode/100 != 2 {
		return mapError(op, resp.StatusCode, body)
	}
	return nil
}

// compile-time assertions that Provider satisfies the scheduled-charge and webhook
// ports (in addition to the immediate ports.PixProvider asserted in pix.go).
var (
	_ ports.PixScheduledProvider = (*Provider)(nil)
	_ ports.PixWebhookRegistrar  = (*Provider)(nil)
)
