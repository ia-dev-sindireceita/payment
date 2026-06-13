package adminweb

import (
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
)

// roleLabel is the operator identity shown in the sidebar. RBAC enforcement is a
// Security/CTO decision (‹Q1›); this is display-only for now.
const roleLabel = "operador@sindireceita · platform_admin"

// base carries the layout-level fields every full page needs.
type base struct {
	Title string
	Nav   string
	CSRF  string
	Role  string
}

func newBase(title, nav, csrf string) base {
	return base{Title: title, Nav: nav, CSRF: csrf, Role: roleLabel}
}

// tenantView is the template-facing projection of a tenant. It exposes only what
// the UI renders and never carries secrets.
type tenantView struct {
	ID          string
	Name        string
	DisplayName string
	Email       string
	CNPJ        string // normalised digits, or "" when unset
	Active      bool
	CreatedAt   time.Time
}

func toTenantView(t *tenant.Tenant) tenantView {
	return tenantView{
		ID:          t.ID(),
		Name:        t.Name(),
		DisplayName: t.DisplayName(),
		Email:       t.Email(),
		CNPJ:        t.CNPJ(),
		Active:      t.Active(),
		CreatedAt:   t.CreatedAt(),
	}
}

func toTenantViews(ts []*tenant.Tenant) []tenantView {
	out := make([]tenantView, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTenantView(t))
	}
	return out
}

// CNPJMasked renders the stored digits as a masked CNPJ (00.000.000/0000-00).
// Unset CNPJ renders as an em dash.
func (v tenantView) CNPJMasked() string { return maskCNPJ(v.CNPJ) }

// CreatedAtBR renders the creation date in Brazilian format.
func (v tenantView) CreatedAtBR() string { return v.CreatedAt.Format("02/01/2006") }

// maskCNPJ formats 14 digits as 00.000.000/0000-00. Non-14-length input is
// returned as-is (defensive: never panic on unexpected data); "" -> "—".
func maskCNPJ(digits string) string {
	if digits == "" {
		return "—"
	}
	if len(digits) != 14 {
		return digits
	}
	return digits[0:2] + "." + digits[2:5] + "." + digits[5:8] + "/" + digits[8:12] + "-" + digits[12:14]
}

// listView backs screen A (tenant list).
type listView struct {
	base
	Tenants []tenantView
	Search  string
	Status  string
}

// rowsView backs the #tenant-rows fragment (search/filter swaps).
type rowsView struct {
	Tenants []tenantView
	Search  string
}

// newView backs screen B (create tenant form), including echoed values and
// per-field validation errors.
type newView struct {
	base
	Form       map[string]string
	Errors     map[string]string
	FirstError string
}

// detailView backs screen C (tenant detail).
type detailView struct {
	base
	Tenant tenantView
}

// stubView backs the "em breve" placeholder for deferred tabs/sections.
type stubView struct {
	base
	Heading  string
	Message  string
	BackHref string
}

// toastData backs an out-of-band toast.
type toastData struct {
	Kind    string // "success" | "danger" | "info"
	Message string
}
