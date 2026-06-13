// Package http is the driving adapter exposing the application over HTTP: the
// tenant API, the admin plane and the bank webhook. Security posture is
// deny-by-default: every route is behind authentication; the tenant is derived
// from the credential server-side and never from client input (threat H1).
package http

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

type ctxKey int

const (
	ctxTenantID ctxKey = iota
)

// TenantAuthenticator resolves an opaque API token to a tenant id. A failed
// lookup denies the request (threat H2).
type TenantAuthenticator interface {
	AuthenticateTenant(token string) (tenantID string, ok bool)
}

// AdminAuthenticator authenticates an admin operator token (admin plane, TB6).
type AdminAuthenticator interface {
	AuthenticateAdmin(token string) bool
}

// WebhookAuthenticator authenticates an inbound bank webhook. In production this
// is mTLS client-cert validation; the scaffold verifies a shared secret in
// constant time. Either way the posture is failure-closed (threat W1).
type WebhookAuthenticator interface {
	AuthenticateWebhook(secret string) bool
}

// StaticTokenAuth is a config-driven authenticator: opaque tokens map to tenant
// ids; an admin-token set; a webhook secret. Tokens/secrets come from config, not
// code. Suitable for the foundation; replace with a real IdP/mTLS adapter later.
type StaticTokenAuth struct {
	tenantTokens  map[string]string // token -> tenantID
	adminTokens   map[string]struct{}
	webhookSecret string
}

// NewStaticTokenAuth builds a StaticTokenAuth from config values.
func NewStaticTokenAuth(tenantTokens map[string]string, adminTokens []string, webhookSecret string) *StaticTokenAuth {
	tt := make(map[string]string, len(tenantTokens))
	for k, v := range tenantTokens {
		tt[k] = v
	}
	at := make(map[string]struct{}, len(adminTokens))
	for _, t := range adminTokens {
		at[t] = struct{}{}
	}
	return &StaticTokenAuth{tenantTokens: tt, adminTokens: at, webhookSecret: webhookSecret}
}

// AuthenticateTenant resolves a token to a tenant id.
func (a *StaticTokenAuth) AuthenticateTenant(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	tenantID, ok := a.tenantTokens[token]
	return tenantID, ok
}

// AuthenticateAdmin reports whether the token is a valid admin token.
func (a *StaticTokenAuth) AuthenticateAdmin(token string) bool {
	if token == "" {
		return false
	}
	_, ok := a.adminTokens[token]
	return ok
}

// AuthenticateWebhook compares the provided secret in constant time.
func (a *StaticTokenAuth) AuthenticateWebhook(secret string) bool {
	if a.webhookSecret == "" || secret == "" {
		return false // failure-closed: no secret configured => deny
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(a.webhookSecret)) == 1
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// tenantAuthMiddleware enforces tenant authentication and injects the tenant id.
func tenantAuthMiddleware(auth TenantAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID, ok := auth.AuthenticateTenant(bearerToken(r))
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			ctx := context.WithValue(r.Context(), ctxTenantID, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// adminAuthMiddleware enforces admin authentication.
func adminAuthMiddleware(auth AdminAuthenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auth.AuthenticateAdmin(bearerToken(r)) {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tenantFromContext returns the authenticated tenant id, or "" if absent.
func tenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxTenantID).(string); ok {
		return v
	}
	return ""
}
