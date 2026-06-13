package http

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRF protection for browser-served, cookie-authenticated admin pages (the
// HTMX admin UI in the child issue). The JSON tenant/admin API authenticates
// with a Bearer token (not an ambient cookie) and is therefore not CSRF-exposed;
// this middleware is for the HTML plane only.
//
// Strategy: double-submit token. On a safe request the middleware mints a random
// token, sets it in a cookie and exposes the same value via CSRFToken(ctx) so a
// template can echo it into the page. On a mutating request (POST/PUT/PATCH/
// DELETE) the submitted token (header or form field) must equal the cookie,
// compared in constant time. The cookie is HttpOnly, so the only way to learn
// the token is to render it server-side into a same-origin page — a cross-origin
// attacker can do neither.
const (
	// csrfCookieName is the cookie carrying the per-session CSRF token.
	csrfCookieName = "csrf_token"
	// CSRFHeaderName is the request header HTMX should send the token in (wire it
	// once with hx-headers='{"X-CSRF-Token": "<token>"}' on the admin <body>).
	CSRFHeaderName = "X-CSRF-Token"
	// CSRFFieldName is the hidden form-field name for non-HTMX/plain-form fallback.
	CSRFFieldName = "csrf_token"
	// csrfTokenBytes is the entropy of a freshly minted token (256 bits).
	csrfTokenBytes = 32
)

// newCSRFToken returns a URL-safe random token.
func newCSRFToken() (string, error) {
	b := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// csrfMiddleware enforces double-submit CSRF protection on mutating requests and
// ensures a token cookie exists on safe ones. It is exported for the admin-UI
// child via CSRFProtect.
func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(csrfCookieName)
		token := ""
		if err == nil {
			token = cookie.Value
		}

		if isSafeMethod(r.Method) {
			// Ensure a token exists so the rendered page can carry it.
			if token == "" {
				minted, mErr := newCSRFToken()
				if mErr != nil {
					writeError(w, http.StatusInternalServerError, "internal error")
					return
				}
				token = minted
				setCSRFCookie(w, r, token)
			}
			ctx := context.WithValue(r.Context(), ctxCSRFToken, token)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Mutating method: the cookie token must exist and match the submitted one.
		submitted := r.Header.Get(CSRFHeaderName)
		if submitted == "" {
			submitted = r.PostFormValue(CSRFFieldName)
		}
		if token == "" || submitted == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(submitted)) != 1 {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		ctx := context.WithValue(r.Context(), ctxCSRFToken, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// CSRFProtect is the reusable middleware the admin-UI child wraps its
// browser/HTML routes with. Compose it after the auth middleware so only
// authenticated sessions mint tokens.
func CSRFProtect(next http.Handler) http.Handler { return csrfMiddleware(next) }

// CSRFToken returns the CSRF token for the current request so a template can
// render it into the form / hx-headers. Empty if the request did not pass
// through csrfMiddleware.
func CSRFToken(ctx context.Context) string {
	if v, ok := ctx.Value(ctxCSRFToken).(string); ok {
		return v
	}
	return ""
}

// setCSRFCookie writes the token cookie. HttpOnly so JS cannot read it; SameSite
// Lax and Secure as defense-in-depth alongside the double-submit check. Secure is
// dropped only for plaintext loopback so local/dev over http still works.
func setCSRFCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// isSafeMethod reports whether the method is read-only per RFC 7231 (no CSRF
// token required).
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}
