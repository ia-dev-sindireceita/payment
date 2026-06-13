package http

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ia-dev-sindireceita/payment/internal/app"
)

// Server holds the application services and authenticators behind the HTTP
// driving adapter. It is constructed once at startup and is safe for concurrent
// use.
type Server struct {
	charges     *app.ChargeService
	admin       *app.AdminService
	webhooks    *app.WebhookService
	tenantAuth  TenantAuthenticator
	adminAuth   AdminAuthenticator
	webhookAuth WebhookAuthenticator
}

// Config wires a Server's dependencies.
type Config struct {
	Charges     *app.ChargeService
	Admin       *app.AdminService
	Webhooks    *app.WebhookService
	TenantAuth  TenantAuthenticator
	AdminAuth   AdminAuthenticator
	WebhookAuth WebhookAuthenticator
}

// NewServer builds a Server from its config.
func NewServer(c Config) *Server {
	return &Server{
		charges:     c.Charges,
		admin:       c.Admin,
		webhooks:    c.Webhooks,
		tenantAuth:  c.TenantAuth,
		adminAuth:   c.AdminAuth,
		webhookAuth: c.WebhookAuth,
	}
}

// Router builds the HTTP handler. All routes are authenticated (deny-by-default);
// the public health check is the only unauthenticated route.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	// Rate limiters: generous defaults; tune per deployment.
	tenantLimiter := newRateLimiter(20, 10, nil)
	webhookLimiter := newRateLimiter(50, 25, nil)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Tenant API (TB1) — authenticated, tenant-scoped, rate-limited.
	r.Route("/v1", func(r chi.Router) {
		r.Use(tenantAuthMiddleware(s.tenantAuth))
		r.Use(tenantLimiter.middleware(tenantOrIPKey))
		r.Post("/charges", s.handleCreateCharge)
		r.Get("/charges/{id}", s.handleGetCharge)
	})

	// Admin plane (TB6) — admin auth, segregated from tenant plane.
	r.Route("/admin", func(r chi.Router) {
		r.Use(adminAuthMiddleware(s.adminAuth))
		r.Post("/tenants", s.handleCreateTenant)
		r.Post("/tenants/{tenantID}/pricing", s.handleSetPrice)
	})

	// Bank webhook (TB1→TB5) — failure-closed auth in the handler, rate-limited.
	r.Group(func(r chi.Router) {
		r.Use(webhookLimiter.middleware(func(req *http.Request) string { return "ip:" + clientIP(req) }))
		r.Post("/webhooks/bank", s.handleWebhook)
	})

	return r
}
