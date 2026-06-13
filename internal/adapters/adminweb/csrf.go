package adminweb

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRF protection for the admin console.
//
// Pattern: synchronizer-token / double-submit hybrid. A random token is stored
// in an HttpOnly cookie and ALSO embedded by the server into every form (hidden
// field) and into the htmx X-CSRF-Token header (rendered into the layout). On an
// unsafe request the submitted token is compared, in constant time, to the
// cookie value. Because the server injects the token into the HTML (it is not
// read from the cookie by client JS), the cookie can stay HttpOnly.
//
// This is defence-in-depth on top of SameSite=Lax. Storage/rotation policy is a
// Security/CTO concern (loop SecurityEngineer, SIN-64706).
const (
	csrfCookieName = "pay_csrf"
	csrfFieldName  = "csrf_token"
	csrfHeaderName = "X-CSRF-Token"
)

type csrfCtxKey struct{}

// csrfConfig configures the middleware. tokenFunc is injectable for
// deterministic tests; secure toggles the cookie Secure attribute (off for
// plain-HTTP tests/dev, on behind TLS in production).
type csrfConfig struct {
	tokenFunc func() string
	secure    bool
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is catastrophic; fail closed with an unusable token
		// so every comparison rejects rather than accepting a predictable value.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// csrfMiddleware ensures a token cookie exists, exposes the active token via the
// request context, and rejects unsafe requests whose submitted token does not
// match the cookie.
func csrfMiddleware(cfg csrfConfig) func(http.Handler) http.Handler {
	gen := cfg.tokenFunc
	if gen == nil {
		gen = newToken
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := cookieToken(r)
			if token == "" {
				token = gen()
				http.SetCookie(w, &http.Cookie{
					Name:     csrfCookieName,
					Value:    token,
					Path:     "/console",
					HttpOnly: true,
					Secure:   cfg.secure,
					SameSite: http.SameSiteLaxMode,
				})
			}

			if !isSafeMethod(r.Method) {
				if !validCSRF(r, token) {
					http.Error(w, "CSRF token inválido", http.StatusForbidden)
					return
				}
			}

			ctx := context.WithValue(r.Context(), csrfCtxKey{}, token)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func cookieToken(r *http.Request) string {
	c, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// validCSRF compares the submitted token (header preferred, then form field) to
// the cookie token in constant time.
func validCSRF(r *http.Request, cookie string) bool {
	if cookie == "" {
		return false // fail closed: no cookie token to compare against
	}
	submitted := r.Header.Get(csrfHeaderName)
	if submitted == "" {
		submitted = r.PostFormValue(csrfFieldName)
	}
	if submitted == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(submitted), []byte(cookie)) == 1
}

// csrfToken returns the active token for the request, or "" if the middleware
// did not run.
func csrfToken(r *http.Request) string {
	if v, ok := r.Context().Value(csrfCtxKey{}).(string); ok {
		return v
	}
	return ""
}
