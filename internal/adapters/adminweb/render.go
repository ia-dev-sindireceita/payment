package adminweb

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// screenFiles are the per-screen template files. Each defines a "body" template
// and is cloned onto the shared base (layout + partials), so it can render both
// the full page ("layout") and the fragment ("body").
var screenFiles = map[string]string{
	"tenants_list":  "templates/tenants_list.html",
	"tenant_new":    "templates/tenant_new.html",
	"tenant_detail": "templates/tenant_detail.html",
	"stub":          "templates/stub.html",
}

// renderer holds the parsed template sets. base carries the layout and all
// shared partials; pages maps a screen name to its cloned, body-bearing set.
type renderer struct {
	base  *template.Template
	pages map[string]*template.Template
}

// newRenderer parses the embedded templates. It returns an error if any template
// fails to parse so wiring fails fast at startup.
func newRenderer() (*renderer, error) {
	base, err := template.New("base").ParseFS(templateFS, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return nil, fmt.Errorf("parse base templates: %w", err)
	}
	pages := make(map[string]*template.Template, len(screenFiles))
	for name, file := range screenFiles {
		clone, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone base for %s: %w", name, err)
		}
		if _, err := clone.ParseFS(templateFS, file); err != nil {
			return nil, fmt.Errorf("parse screen %s: %w", name, err)
		}
		pages[name] = clone
	}
	return &renderer{base: base, pages: pages}, nil
}

// isHX reports whether the request came from htmx (partial swap expected).
func isHX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// page renders a screen: the full layout for normal navigations, or just the
// "body" fragment for htmx swaps. The output is buffered so a template error
// yields a clean 500 instead of a half-written response.
func (rd *renderer) page(w http.ResponseWriter, r *http.Request, screen string, status int, data any) {
	tmpl, ok := rd.pages[screen]
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	def := "layout"
	if isHX(r) {
		def = "body"
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, def, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHTML(w, status, buf.Bytes())
}

// partial renders a named shared partial (e.g. "tenant_rows") to the response.
func (rd *renderer) partial(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := rd.base.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeHTML(w, status, buf.Bytes())
}

// oob renders one or more shared partials concatenated into a single response —
// used for out-of-band swaps (e.g. status header + toast in one reply).
func (rd *renderer) oob(w http.ResponseWriter, status int, parts ...oobPart) {
	var buf bytes.Buffer
	for _, p := range parts {
		if err := rd.base.ExecuteTemplate(&buf, p.name, p.data); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	writeHTML(w, status, buf.Bytes())
}

type oobPart struct {
	name string
	data any
}

// bodyWithOOB renders a screen's "body" fragment followed by one or more
// out-of-band partials in a single htmx response (e.g. swap #main to the new
// detail view and inject a toast). Output is buffered for clean error handling.
func (rd *renderer) bodyWithOOB(w http.ResponseWriter, status int, screen string, data any, parts ...oobPart) {
	tmpl, ok := rd.pages[screen]
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "body", data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, p := range parts {
		if err := tmpl.ExecuteTemplate(&buf, p.name, p.data); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	writeHTML(w, status, buf.Bytes())
}

func writeHTML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
