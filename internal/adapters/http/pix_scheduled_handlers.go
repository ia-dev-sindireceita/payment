package http

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ia-dev-sindireceita/payment/internal/app"
)

// --- Tenant API: scheduled PIX charges + webhook (cobrança com vencimento, roteiro
// 7.5–7.8) ---

// createScheduledPixRequest is the boundary body for POST /v1/pix/scheduled. A cobv
// REQUIRES a devedor (roteiro 7.5). due_date is RFC3339; validity_after_due_days is the
// validadeAposVencimento. Unknown fields are rejected by decodeJSON (anti
// mass-assignment).
type createScheduledPixRequest struct {
	AmountCents          int64          `json:"amount_cents"`
	Currency             string         `json:"currency"`
	DueDate              string         `json:"due_date"`
	ValidityAfterDueDays int            `json:"validity_after_due_days"`
	Devedor              *pixDevedorReq `json:"devedor"`
}

func (s *Server) handleCreateScheduledPix(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "missing Idempotency-Key header")
		return
	}
	var req createScheduledPixRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	due, ok := parseRFC3339(req.DueDate)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid or missing due_date (RFC3339)")
		return
	}

	in := app.CreateScheduledChargeInput{
		TenantID:             tenantID,
		AmountCents:          req.AmountCents,
		Currency:             req.Currency,
		IdempotencyKey:       idemKey,
		DueDate:              due,
		ValidityAfterDueDays: req.ValidityAfterDueDays,
	}
	if req.Devedor != nil {
		in.DebtorTaxID = req.Devedor.TaxID
		in.DebtorName = req.Devedor.Name
	}

	p, qr, err := s.pix.CreateScheduledCharge(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPixChargeView(qr, p.Amount().Cents()))
}

func (s *Server) handleGetScheduledPix(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	txID := chi.URLParam(r, "txid")
	qr, err := s.pix.GetScheduledCharge(r.Context(), tenantID, txID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPixChargeView(qr, qr.ExpectedAmountCents))
}

func (s *Server) handleListScheduledPix(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	q := r.URL.Query()

	start, ok := parseRFC3339(q.Get("start"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid or missing start (RFC3339)")
		return
	}
	end, ok := parseRFC3339(q.Get("end"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid or missing end (RFC3339)")
		return
	}
	page, ok := parseOptionalInt(q.Get("page"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	pageSize, ok := parseOptionalInt(q.Get("page_size"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid page_size")
		return
	}

	list, err := s.pix.ListScheduledCharges(r.Context(), app.ListScheduledChargesInput{
		TenantID: tenantID,
		Start:    start,
		End:      end,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}

	views := make([]pixChargeView, 0, len(list.Charges))
	for _, c := range list.Charges {
		views = append(views, toPixChargeView(c, c.ExpectedAmountCents))
	}
	writeJSON(w, http.StatusOK, pixListView{
		Charges:    views,
		Page:       list.Page,
		PageSize:   list.PageSize,
		TotalItems: list.TotalItems,
		TotalPages: list.TotalPages,
	})
}

// registerPixWebhookRequest is the boundary body for POST /v1/pix/webhook. chave is the
// tenant's PIX key; webhook_url is the notification target. Unknown fields are rejected
// by decodeJSON.
type registerPixWebhookRequest struct {
	Chave      string `json:"chave"`
	WebhookURL string `json:"webhook_url"`
}

// pixWebhookView is the response to a successful webhook registration. It deliberately
// does NOT echo the webhook URL: the URL is a capability secret kept out of responses,
// logs and errors (the caller already holds it).
type pixWebhookView struct {
	Chave  string `json:"chave"`
	Status string `json:"status"`
}

func (s *Server) handleRegisterPixWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromContext(r.Context())
	var req registerPixWebhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.pix.RegisterPixWebhook(r.Context(), tenantID, req.Chave, req.WebhookURL); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pixWebhookView{Chave: strings.TrimSpace(req.Chave), Status: "registered"})
}
