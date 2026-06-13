package http_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
)

// echoCSRFHandler renders the CSRF token from the context so tests can read what
// a template would embed into the page.
var echoCSRFHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(httpadapter.CSRFToken(r.Context())))
})

func csrfCookieValue(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "csrf_token" {
			return c.Value
		}
	}
	return ""
}

func TestCSRFSafeMethodMintsToken(t *testing.T) {
	t.Parallel()
	h := httpadapter.CSRFProtect(echoCSRFHandler)

	req := httptest.NewRequest(http.MethodGet, "/admin/form", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	cookie := csrfCookieValue(rec)
	if cookie == "" {
		t.Fatal("expected a csrf_token cookie to be set")
	}
	if body := rec.Body.String(); body == "" || body != cookie {
		t.Fatalf("rendered token %q must equal cookie %q", body, cookie)
	}
}

func TestCSRFRejectsMutationWithoutToken(t *testing.T) {
	t.Parallel()
	h := httpadapter.CSRFProtect(echoCSRFHandler)

	req := httptest.NewRequest(http.MethodPost, "/admin/save", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for POST without token, got %d", rec.Code)
	}
}

func TestCSRFAcceptsMatchingHeader(t *testing.T) {
	t.Parallel()
	h := httpadapter.CSRFProtect(echoCSRFHandler)

	// Mint a token via a safe request.
	get := httptest.NewRequest(http.MethodGet, "/admin/form", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, get)
	token := csrfCookieValue(getRec)
	if token == "" {
		t.Fatal("no token minted")
	}

	// Replay it as header + cookie on a mutation.
	post := httptest.NewRequest(http.MethodPost, "/admin/save", nil)
	post.AddCookie(&http.Cookie{Name: "csrf_token", Value: token})
	post.Header.Set(httpadapter.CSRFHeaderName, token)
	postRec := httptest.NewRecorder()
	h.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusOK {
		t.Fatalf("want 200 with matching token, got %d", postRec.Code)
	}
}

func TestCSRFRejectsMismatchedHeader(t *testing.T) {
	t.Parallel()
	h := httpadapter.CSRFProtect(echoCSRFHandler)

	post := httptest.NewRequest(http.MethodPost, "/admin/save", nil)
	post.AddCookie(&http.Cookie{Name: "csrf_token", Value: "the-real-token"})
	post.Header.Set(httpadapter.CSRFHeaderName, "an-attacker-guess")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, post)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for mismatched token, got %d", rec.Code)
	}
}

func TestCSRFAcceptsMatchingFormField(t *testing.T) {
	t.Parallel()
	h := httpadapter.CSRFProtect(echoCSRFHandler)

	form := url.Values{httpadapter.CSRFFieldName: {"form-token"}}
	post := httptest.NewRequest(http.MethodPost, "/admin/save", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(&http.Cookie{Name: "csrf_token", Value: "form-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, post)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 with matching form field, got %d", rec.Code)
	}
}

func TestCSRFReusesExistingCookie(t *testing.T) {
	t.Parallel()
	h := httpadapter.CSRFProtect(echoCSRFHandler)

	req := httptest.NewRequest(http.MethodGet, "/admin/form", nil)
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "preexisting"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if body := rec.Body.String(); body != "preexisting" {
		t.Fatalf("want existing token reused, got %q", body)
	}
}
