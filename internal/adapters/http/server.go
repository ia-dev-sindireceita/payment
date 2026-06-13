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
	csrf        CSRFGuard
}

// Config wires a Server's dependencies.
type Config struct {
	Charges     *app.ChargeService
	Admin       *app.AdminService
	Webhooks    *app.WebhookService
	TenantAuth  TenantAuthenticator
	AdminAuth   AdminAuthenticator
	WebhookAuth WebhookAuthenticator
	// SecureCookies sets the Secure attribute on cookies this adapter issues
	// (CSRF token; the admin-UI session cookie via Server.CSRF). Driven by config
	// because TLS is terminated at a proxy — see config.Config.SecureCookies.
	SecureCookies bool
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
		csrf:        NewCSRFGuard(c.SecureCookies),
	}
}

// CSRF returns the server's CSRF guard so the admin-UI child can wrap its live
// HTML routes with Protect under the configured Secure-cookie policy.
func (s *Server) CSRF() CSRFGuard { return s.csrf }

// Router builds the HTTP handler. All routes are authenticated (deny-by-default);
// the public health check is the only unauthenticated route.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	// Rate limiters: generous defaults; tune per deployment. The admin plane is
	// the most privileged surface (tenant creation, per-tenant bank-credential
	// writes), so it is limited at least as strictly as the tenant plane.
	tenantLimiter := newRateLimiter(20, 10, nil)
	adminLimiter := newRateLimiter(20, 10, nil)
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

	// Admin plane (TB6) — admin auth, segregated from tenant plane. Every route
	// is behind adminAuthMiddleware (deny-by-default; a tenant token never
	// resolves to a role and is rejected). Mutations additionally require the full
	// RoleAdmin; RoleOperator is read-only (least privilege). Read routes that
	// admit operators are added by the admin-UI child guarded by
	// requireRole(RoleAdmin, RoleOperator).
	r.Route("/admin", func(r chi.Router) {
		r.Use(adminAuthMiddleware(s.adminAuth))
		// Defense-in-depth: throttle per authenticated admin identity (falling back
		// to client IP). Sits after auth so invalid tokens are rejected cheaply and
		// each admin identity gets its own bucket, mirroring the tenant plane.
		r.Use(adminLimiter.middleware(adminTokenKey))
		r.Group(func(r chi.Router) {
			r.Use(requireRole(RoleAdmin))
			r.Post("/tenants", s.handleCreateTenant)
			r.Post("/tenants/{tenantID}/pricing", s.handleSetPrice)
			r.Put("/tenants/{tenantID}/bank-credential", s.handleSetBankCredential)
		})
	})

	// Bank webhook (TB1→TB5) — failure-closed auth in the handler, rate-limited.
	r.Group(func(r chi.Router) {
		r.Use(webhookLimiter.middleware(func(req *http.Request) string { return "ip:" + clientIP(req) }))
		r.Post("/webhooks/bank", s.handleWebhook)
	})

	return r
}
