// Package adminweb is the server-rendered HTMX driving adapter for the platform
// admin console (SIN-64710, wireframes SIN-64707). It renders HTML over the
// app.ConsoleService use-cases. Security posture: every mutation is CSRF-checked
// and has a no-JS <form method=post> fallback; HTMX is progressive enhancement.
// Output is auto-escaped by html/template (XSS-safe); no secret is ever rendered.
package adminweb

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

// Console is the input port the adapter depends on. *app.ConsoleService satisfies
// it (accept-narrow: the adapter declares only what it uses).
type Console interface {
	ListTenants(ctx context.Context, q app.ListTenantsQuery) ([]*tenant.Tenant, error)
	GetTenant(ctx context.Context, id string) (*tenant.Tenant, error)
	CreateTenant(ctx context.Context, p tenant.Profile) (*tenant.Tenant, error)
	SuspendTenant(ctx context.Context, id string) (*tenant.Tenant, error)
	ActivateTenant(ctx context.Context, id string) (*tenant.Tenant, error)
}

// Options configures the adapter.
type Options struct {
	// Secure sets the CSRF cookie Secure attribute (enable behind TLS).
	Secure bool
	// CSRFToken overrides token generation (tests only; nil = crypto/rand).
	CSRFToken func() string
	// Auth, when set, wraps every console route (except static assets) to
	// authenticate the staff operator and enforce RBAC (‹Q1›: platform_admin /
	// platform_finance). It is intentionally injected by the deployment because
	// the session/RBAC mechanism is a Security/CTO decision (SIN-64706). When nil
	// the console is UNAUTHENTICATED — acceptable for local dev/tests only; a real
	// gate MUST be wired before the console is exposed.
	Auth func(http.Handler) http.Handler
}

// Handler is the admin console HTTP adapter.
type Handler struct {
	console Console
	rd      *renderer
	csrf    func(http.Handler) http.Handler
	auth    func(http.Handler) http.Handler
}

// New builds a Handler, parsing templates up-front (fails fast on a bad template).
func New(console Console, opts Options) (*Handler, error) {
	rd, err := newRenderer()
	if err != nil {
		return nil, err
	}
	return &Handler{
		console: console,
		rd:      rd,
		csrf:    csrfMiddleware(csrfConfig{tokenFunc: opts.CSRFToken, secure: opts.Secure}),
		auth:    opts.Auth,
	}, nil
}

// Routes returns the console router intended to be mounted at /console.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/static/*", h.serveStatic)
	r.Group(func(r chi.Router) {
		r.Use(securityHeaders)
		if h.auth != nil {
			r.Use(h.auth)
		}
		r.Use(h.csrf)
		r.Get("/", h.redirectToTenants)
		r.Get("/tenants", h.listTenants)
		r.Get("/tenants/rows", h.tenantRows)
		r.Get("/tenants/new", h.newTenantForm)
		r.Post("/tenants", h.createTenant)
		r.Get("/tenants/{id}", h.tenantDetail)
		r.Post("/tenants/{id}/suspend", h.suspendTenant)
		r.Post("/tenants/{id}/activate", h.activateTenant)

		// Deferred sections (child issues): render an accessible "em breve"
		// placeholder so navigation stays coherent and never 404s.
		r.Get("/tenants/{id}/credentials", h.tenantStub("Credenciais · C6 Bank",
			"Configuração de credenciais bancárias write-only — em revisão de segurança."))
		r.Get("/tenants/{id}/pricing", h.tenantStub("Tarifação do tenant",
			"Matriz de preços por endpoint editável inline."))
		r.Get("/tenants/{id}/consumption", h.tenantStub("Consumo do tenant",
			"Consumo por endpoint e billing do período."))
		r.Get("/pricing", h.globalStub("Tarifação", "pricing",
			"Preço por tenant × endpoint."))
		r.Get("/billing", h.globalStub("Consumo & Billing", "billing",
			"Uso e faturamento por tenant e período."))
	})
	return r
}

func (h *Handler) redirectToTenants(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/console/tenants", http.StatusSeeOther)
}

// allowedStatic restricts the embedded asset names the console will serve.
var allowedStatic = map[string]string{
	"tokens.css":  "text/css; charset=utf-8",
	"app.css":     "text/css; charset=utf-8",
	"htmx.min.js": "application/javascript; charset=utf-8",
}

func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := path.Base(path.Clean("/" + chi.URLParam(r, "*")))
	ctype, ok := allowedStatic[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// securityHeaders applies a strict, self-only baseline. CSP forbids inline
// script/style (htmx is self-hosted; indicator styles are in app.css), blocks
// framing and constrains form submission to same-origin. Coordinate hardening
// with SecurityEngineer (SIN-64706).
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
		"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func trimID(id string) string { return strings.TrimSpace(id) }
