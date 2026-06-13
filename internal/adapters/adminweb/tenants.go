package adminweb

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

// listTenants renders screen A (full page or #main fragment).
func (h *Handler) listTenants(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	status := app.ParseStatusFilter(r.URL.Query().Get("status"))
	tenants, err := h.console.ListTenants(r.Context(), app.ListTenantsQuery{Search: search, Status: status})
	if err != nil {
		h.renderError(w, r)
		return
	}
	h.rd.page(w, r, "tenants_list", http.StatusOK, listView{
		base:    newBase("Tenants", "tenants", csrfToken(r)),
		Tenants: toTenantViews(tenants),
		Search:  search,
		Status:  string(status),
	})
}

// tenantRows renders only the #tenant-rows fragment for search/filter swaps.
func (h *Handler) tenantRows(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	status := app.ParseStatusFilter(r.URL.Query().Get("status"))
	tenants, err := h.console.ListTenants(r.Context(), app.ListTenantsQuery{Search: search, Status: status})
	if err != nil {
		h.renderError(w, r)
		return
	}
	h.rd.partial(w, http.StatusOK, "tenant_rows", rowsView{
		Tenants: toTenantViews(tenants),
		Search:  search,
	})
}

// newTenantForm renders screen B (empty create form).
func (h *Handler) newTenantForm(w http.ResponseWriter, r *http.Request) {
	h.rd.page(w, r, "tenant_new", http.StatusOK, newView{
		base:   newBase("Novo tenant", "tenants", csrfToken(r)),
		Form:   map[string]string{},
		Errors: map[string]string{},
	})
}

// createTenant handles screen B submission. CSRF is enforced by middleware.
func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{
		"name":         strings.TrimSpace(r.PostFormValue("name")),
		"cnpj":         strings.TrimSpace(r.PostFormValue("cnpj")),
		"display_name": strings.TrimSpace(r.PostFormValue("display_name")),
		"email":        strings.TrimSpace(r.PostFormValue("email")),
	}

	t, err := h.console.CreateTenant(r.Context(), tenant.Profile{
		Name:        form["name"],
		DisplayName: form["display_name"],
		CNPJ:        form["cnpj"],
		Email:       form["email"],
	})
	if err != nil {
		var ve *shared.ValidationError
		if errors.As(err, &ve) {
			field := mapValidationField(ve.Field)
			h.rd.page(w, r, "tenant_new", http.StatusUnprocessableEntity, newView{
				base:       newBase("Novo tenant", "tenants", csrfToken(r)),
				Form:       form,
				Errors:     map[string]string{field: ve.Msg},
				FirstError: field,
			})
			return
		}
		h.renderError(w, r)
		return
	}

	dest := "/console/tenants/" + t.ID()
	if !isHX(r) {
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	view := toTenantView(t)
	w.Header().Set("HX-Push-Url", dest)
	h.rd.bodyWithOOB(w, http.StatusOK, "tenant_detail",
		detailView{base: newBase(view.DisplayName, "tenants", csrfToken(r)), Tenant: view},
		oobPart{name: "toast_oob", data: toastData{Kind: "success", Message: "Tenant criado — configure credenciais e tarifação."}},
	)
}

// tenantDetail renders screen C.
func (h *Handler) tenantDetail(w http.ResponseWriter, r *http.Request) {
	t, err := h.console.GetTenant(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.renderNotFound(w, r)
		return
	}
	view := toTenantView(t)
	h.rd.page(w, r, "tenant_detail", http.StatusOK, detailView{
		base:   newBase(view.DisplayName, "tenants", csrfToken(r)),
		Tenant: view,
	})
}

func (h *Handler) suspendTenant(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.console.SuspendTenant, "Tenant suspenso. Novas chamadas retornam 403.")
}

func (h *Handler) activateTenant(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.console.ActivateTenant, "Tenant reativado. Chamadas voltam a ser aceitas.")
}

// transition runs a suspend/activate operation and renders the result: OOB
// status header + toast for htmx, or a redirect back to the detail page for the
// no-JS fallback.
func (h *Handler) transition(w http.ResponseWriter, r *http.Request, op func(context.Context, string) (*tenant.Tenant, error), msg string) {
	id := chi.URLParam(r, "id")
	t, err := op(r.Context(), id)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			h.renderNotFound(w, r)
			return
		}
		h.renderError(w, r)
		return
	}
	view := toTenantView(t)
	if !isHX(r) {
		http.Redirect(w, r, "/console/tenants/"+view.ID, http.StatusSeeOther)
		return
	}
	h.rd.oob(w, http.StatusOK,
		oobPart{name: "status_header_oob", data: view},
		oobPart{name: "toast_oob", data: toastData{Kind: "success", Message: msg}},
	)
}

// tenantStub renders an "em breve" placeholder for a deferred tenant tab,
// preserving the back-link to the tenant detail.
func (h *Handler) tenantStub(heading, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := trimID(chi.URLParam(r, "id"))
		// Confirm the tenant exists so the back-link is valid and we don't render
		// a section for a non-existent tenant.
		if _, err := h.console.GetTenant(r.Context(), id); err != nil {
			h.renderNotFound(w, r)
			return
		}
		h.rd.page(w, r, "stub", http.StatusOK, stubView{
			base:     newBase(heading, "tenants", csrfToken(r)),
			Heading:  heading,
			Message:  message,
			BackHref: "/console/tenants/" + id,
		})
	}
}

// globalStub renders an "em breve" placeholder for a deferred top-level section.
func (h *Handler) globalStub(heading, nav, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.rd.page(w, r, "stub", http.StatusOK, stubView{
			base:    newBase(heading, nav, csrfToken(r)),
			Heading: heading,
			Message: message,
		})
	}
}

func (h *Handler) renderError(w http.ResponseWriter, r *http.Request) {
	h.rd.page(w, r, "stub", http.StatusInternalServerError, stubView{
		base:    newBase("Erro", "tenants", csrfToken(r)),
		Heading: "Algo deu errado",
		Message: "Não foi possível concluir a operação. Tente novamente.",
	})
}

func (h *Handler) renderNotFound(w http.ResponseWriter, r *http.Request) {
	h.rd.page(w, r, "stub", http.StatusNotFound, stubView{
		base:     newBase("Não encontrado", "tenants", csrfToken(r)),
		Heading:  "Tenant não encontrado",
		Message:  "O tenant solicitado não existe ou foi removido.",
		BackHref: "/console/tenants",
	})
}

// mapValidationField maps a domain validation field name to the form field id so
// the boundary can highlight and focus the right input.
func mapValidationField(domainField string) string {
	switch domainField {
	case "name":
		return "name"
	case "cnpj":
		return "cnpj"
	case "email":
		return "email"
	case "display_name":
		return "display_name"
	default:
		return "form"
	}
}
