package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/billing"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/domain/tenant"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// ConsoleService implements the server-rendered admin console use-cases: tenant
// lifecycle + listing, per-endpoint pricing CRUD, per-tenant bank-credential
// writes, and the read-only consumption audit. It is separate from AdminService
// (the programmatic JSON admin API) so the human console and the machine API
// evolve independently; both are privileged and RBAC-gated at the HTTP boundary.
//
// The service depends only on narrow, segregated input ports (accept-narrow):
// the concrete sqlite/inmemory stores satisfy them, but the console declares
// exactly the capabilities it uses and nothing more.
type ConsoleService struct {
	tenants     TenantStore
	pricing     PricingStore
	ledger      LedgerReader
	credWriter  ports.CredentialWriter
	credReader  ports.CredentialStore
	credEvictor ports.CredentialInvalidator
	clock       ports.Clock
	ids         ports.IDProvider
}

// TenantStore is the tenant capability the console needs: the foundation's
// persistence plus cross-tenant listing for the admin plane.
type TenantStore interface {
	SaveTenant(ctx context.Context, t *tenant.Tenant) error
	FindTenantByID(ctx context.Context, id string) (*tenant.Tenant, error)
	ListTenants(ctx context.Context) ([]*tenant.Tenant, error)
}

// PricingStore is the pricing capability the console needs.
type PricingStore interface {
	UpsertEndpointPrice(ctx context.Context, p billing.EndpointPricing) error
	ListEndpointPrices(ctx context.Context, tenantID string) ([]billing.EndpointPricing, error)
}

// LedgerReader is the read side of the billing ledger powering the consumption
// audit. Writes stay on the append-only path used by the charge/settlement flow.
type LedgerReader interface {
	ListLedgerEntries(ctx context.Context, tenantID string) ([]billing.LedgerEntry, error)
}

// ConsoleDeps bundles the console's dependencies. The fields reuse the same
// concrete adapters wired in Deps, narrowed to the console's ports.
type ConsoleDeps struct {
	Tenants    TenantStore
	Pricing    PricingStore
	Ledger     LedgerReader
	CredWriter ports.CredentialWriter
	// CredReader is the read side of the credential store. The console uses it ONLY
	// to answer "does this (tenant, bank) have a credential configured?" and to echo
	// back the non-secret ClientID / CreditorKey on the bank screens — the secret is
	// never projected into a view (threat C1/C4). Optional: nil degrades to "no bank
	// reports a credential" (the screens still render, every bank shows — pendente).
	CredReader ports.CredentialStore
	// CredInvalidator evicts cached state keyed on a tenant's credential (the C6
	// OAuth2 token cache) right after a credential write, closing the
	// token-revocation lag (ADR-0003). Optional: nil degrades to a no-op.
	CredInvalidator ports.CredentialInvalidator
	Clock           ports.Clock
	IDs             ports.IDProvider
}

// NewConsoleService wires a ConsoleService from its dependencies. A nil
// CredInvalidator degrades to a no-op (the credential write still succeeds; only
// the cache-eviction step is skipped).
func NewConsoleService(d ConsoleDeps) *ConsoleService {
	ci := d.CredInvalidator
	if ci == nil {
		ci = noopCredInvalidator{}
	}
	return &ConsoleService{
		tenants:     d.Tenants,
		pricing:     d.Pricing,
		ledger:      d.Ledger,
		credWriter:  d.CredWriter,
		credReader:  d.CredReader,
		credEvictor: ci,
		clock:       d.Clock,
		ids:         d.IDs,
	}
}

// Now returns the service's notion of the current time from the injected clock.
// The console's read side uses it to default the consumption date window, so the
// default tracks the same clock the rest of the use-cases stamp events with
// (deterministic under a fixed test clock).
func (s *ConsoleService) Now() time.Time { return s.clock.Now() }

// --- Tenants ---

// StatusFilter narrows a tenant listing by lifecycle status.
type StatusFilter string

const (
	// StatusAny matches active and suspended tenants.
	StatusAny StatusFilter = ""
	// StatusActive matches only active tenants.
	StatusActive StatusFilter = "active"
	// StatusSuspended matches only suspended tenants.
	StatusSuspended StatusFilter = "suspended"
)

// ParseStatusFilter maps a raw query value to a StatusFilter, defaulting to
// StatusAny for unknown/empty input (forgiving input handling at the boundary).
func ParseStatusFilter(raw string) StatusFilter {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "active":
		return StatusActive
	case "suspended":
		return StatusSuspended
	default:
		return StatusAny
	}
}

// ListTenantsQuery describes a filtered tenant listing.
type ListTenantsQuery struct {
	Search string       // case-insensitive substring over the tenant name
	Status StatusFilter // lifecycle filter
}

// ListTenants returns tenants matching the query, newest-first.
func (s *ConsoleService) ListTenants(ctx context.Context, q ListTenantsQuery) ([]*tenant.Tenant, error) {
	all, err := s.tenants.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	needle := strings.ToLower(strings.TrimSpace(q.Search))
	out := make([]*tenant.Tenant, 0, len(all))
	for _, t := range all {
		if !matchesStatus(t, q.Status) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(t.Name()), needle) {
			continue
		}
		out = append(out, t)
	}
	// The store already returns newest-first; enforce determinism here too in case
	// an adapter returns an unordered slice.
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := out[i].CreatedAt(), out[j].CreatedAt()
		if ci.Equal(cj) {
			return out[i].ID() > out[j].ID()
		}
		return ci.After(cj)
	})
	return out, nil
}

func matchesStatus(t *tenant.Tenant, f StatusFilter) bool {
	switch f {
	case StatusActive:
		return t.Active()
	case StatusSuspended:
		return !t.Active()
	default:
		return true
	}
}

// GetTenant returns a tenant by id, or shared.ErrNotFound.
func (s *ConsoleService) GetTenant(ctx context.Context, id string) (*tenant.Tenant, error) {
	t, err := s.tenants.FindTenantByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("find tenant: %w", err)
	}
	return t, nil
}

// CreateTenant provisions a new tenant from the console form (name only). The
// domain enforces the name invariants; a validation error is surfaced inline by
// the boundary.
func (s *ConsoleService) CreateTenant(ctx context.Context, name string) (*tenant.Tenant, error) {
	t, err := tenant.New(s.ids.NewID(), name, s.clock.Now())
	if err != nil {
		return nil, err
	}
	if err := s.tenants.SaveTenant(ctx, t); err != nil {
		return nil, fmt.Errorf("save tenant: %w", err)
	}
	return t, nil
}

// SuspendTenant deactivates a tenant (reversible). Returns the updated tenant.
func (s *ConsoleService) SuspendTenant(ctx context.Context, id string) (*tenant.Tenant, error) {
	return s.transition(ctx, id, (*tenant.Tenant).Deactivate)
}

// ActivateTenant re-enables a suspended tenant. Returns the updated tenant.
func (s *ConsoleService) ActivateTenant(ctx context.Context, id string) (*tenant.Tenant, error) {
	return s.transition(ctx, id, (*tenant.Tenant).Activate)
}

func (s *ConsoleService) transition(ctx context.Context, id string, apply func(*tenant.Tenant)) (*tenant.Tenant, error) {
	t, err := s.tenants.FindTenantByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("find tenant: %w", err)
	}
	apply(t)
	if err := s.tenants.SaveTenant(ctx, t); err != nil {
		return nil, fmt.Errorf("save tenant: %w", err)
	}
	return t, nil
}

// --- Bank credentials (write-only) ---

// SetBankCredential stores a tenant's bank (PSP) credential via the secret-store
// write port. The target tenant must exist. The secret transits straight to the
// writer: it never enters domain state, logs, errors or any rendered response
// (threat C1/C4). The console never reads a credential back.
func (s *ConsoleService) SetBankCredential(ctx context.Context, tenantID, clientID, secret string) error {
	if _, err := s.tenants.FindTenantByID(ctx, strings.TrimSpace(tenantID)); err != nil {
		return fmt.Errorf("resolve tenant: %w", err)
	}
	// Single-bank write path: persists under the default bank (BankIDC6),
	// preserving current behaviour. A per-bank selector in the console is the
	// routing/UX workstream (SIN-66022 / SIN-66017), not this schema change.
	if err := s.credWriter.SetBankCredential(ctx, tenantID, ports.BankIDC6, clientID, secret); err != nil {
		// Wrap with non-sensitive context only; never include the secret.
		return fmt.Errorf("set bank credential: %w", err)
	}
	// Evict any cached OAuth2 token minted under the prior credential so the
	// rotation/revocation takes effect immediately instead of after the cached
	// bearer expires (token-revocation lag, ADR-0003). Best-effort and local.
	s.credEvictor.InvalidateToken(strings.TrimSpace(tenantID))
	return nil
}

// --- Banks (multi-bank console, SIN-66017 / SIN-66086) ---

// BankInfo is the console projection of one bank within a tenant. It never
// carries the secret: CredentialSet answers "is a credential configured?" and
// ClientID / CreditorKey are non-secret identity fields echoed back for operator
// recognition (the secret is fetched at use time and never rendered, threat C1).
type BankInfo struct {
	Slug          string
	CredentialSet bool
	ClientID      string
	CreditorKey   string
}

// lookupBank reads the (tenantID, bankID) credential and reports whether one is
// configured. A missing credential (ErrNotFound) is not an error — it is the
// "pendente" state. A nil reader (optional dependency) degrades to "not
// configured". Any other store error propagates so the screen 500s honestly.
func (s *ConsoleService) lookupBank(ctx context.Context, tenantID, bankID string) (BankInfo, error) {
	info := BankInfo{Slug: bankID}
	if s.credReader == nil {
		return info, nil
	}
	c, err := s.credReader.GetBankCredential(ctx, tenantID, bankID)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return info, nil
		}
		return BankInfo{}, fmt.Errorf("read bank credential: %w", err)
	}
	// Project only non-secret identity fields. The secret stays in the store.
	info.CredentialSet = true
	info.ClientID = c.ClientID
	info.CreditorKey = c.CreditorKey
	return info, nil
}

// ListBanks returns the tenant's configured banks (those with a credential),
// ordered deterministically by slug. The set of candidate banks is the platform's
// closed allow-list (ports.KnownBankIDs) — there is no per-tenant bank registry,
// so "the banks this tenant uses" is "the supported banks this tenant has a
// credential for". A brand-new tenant returns an empty slice (the empty state).
// The tenant must exist so the screen 404s cleanly.
func (s *ConsoleService) ListBanks(ctx context.Context, tenantID string) ([]BankInfo, error) {
	tenantID = strings.TrimSpace(tenantID)
	if _, err := s.tenants.FindTenantByID(ctx, tenantID); err != nil {
		return nil, fmt.Errorf("resolve tenant: %w", err)
	}
	out := make([]BankInfo, 0, len(ports.KnownBankIDs()))
	for _, slug := range ports.KnownBankIDs() {
		info, err := s.lookupBank(ctx, tenantID, slug)
		if err != nil {
			return nil, err
		}
		if info.CredentialSet {
			out = append(out, info)
		}
	}
	return out, nil
}

// AddableBankSlugs returns the supported bank slugs the tenant has NOT configured
// yet, ordered by slug — the closed allow-list for the add-bank selector. When it
// is empty every supported bank is already configured (the selector is disabled).
func (s *ConsoleService) AddableBankSlugs(ctx context.Context, tenantID string) ([]string, error) {
	tenantID = strings.TrimSpace(tenantID)
	if _, err := s.tenants.FindTenantByID(ctx, tenantID); err != nil {
		return nil, fmt.Errorf("resolve tenant: %w", err)
	}
	out := make([]string, 0, len(ports.KnownBankIDs()))
	for _, slug := range ports.KnownBankIDs() {
		info, err := s.lookupBank(ctx, tenantID, slug)
		if err != nil {
			return nil, err
		}
		if !info.CredentialSet {
			out = append(out, slug)
		}
	}
	return out, nil
}

// GetBank returns one bank's console projection for the detail screen. The bank
// slug must be a supported bank (deny-by-default allow-list) — an unknown slug is
// shared.ErrNotFound so the screen 404s rather than rendering a bank that can
// never route a charge. The tenant must exist.
func (s *ConsoleService) GetBank(ctx context.Context, tenantID, bankID string) (BankInfo, error) {
	tenantID = strings.TrimSpace(tenantID)
	if _, err := s.tenants.FindTenantByID(ctx, tenantID); err != nil {
		return BankInfo{}, fmt.Errorf("resolve tenant: %w", err)
	}
	slug := ports.NormalizeBankID(bankID)
	if !ports.IsKnownBankID(slug) {
		return BankInfo{}, fmt.Errorf("unknown bank %q: %w", bankID, shared.ErrNotFound)
	}
	return s.lookupBank(ctx, tenantID, slug)
}

// SetBankCredentialFor stores a tenant's credential for an explicit bank (the
// per-bank console write path, mirroring the admin PUT bank-credential contract
// of SIN-66023). The bank slug is validated against the closed allow-list
// (deny-by-default): an unknown slug is a validation error and the input is never
// echoed (threat C1/C4). The secret transits straight to the writer — it never
// enters domain state, logs, errors or any rendered response. The cached OAuth2
// token minted under the prior credential is evicted immediately (ADR-0003).
func (s *ConsoleService) SetBankCredentialFor(ctx context.Context, tenantID, bankID, clientID, secret string) error {
	tenantID = strings.TrimSpace(tenantID)
	if _, err := s.tenants.FindTenantByID(ctx, tenantID); err != nil {
		return fmt.Errorf("resolve tenant: %w", err)
	}
	slug := ports.NormalizeBankID(bankID)
	if !ports.IsKnownBankID(slug) {
		return shared.NewValidationError("bank", "banco não suportado")
	}
	if err := s.credWriter.SetBankCredential(ctx, tenantID, slug, clientID, secret); err != nil {
		return fmt.Errorf("set bank credential: %w", err)
	}
	s.credEvictor.InvalidateToken(tenantID)
	return nil
}

// --- Pricing ---

// ListPricing returns a tenant's per-endpoint pricing rules (ordered by
// endpoint). The tenant must exist so the screen 404s cleanly for a bad id.
func (s *ConsoleService) ListPricing(ctx context.Context, tenantID string) ([]billing.EndpointPricing, error) {
	if _, err := s.tenants.FindTenantByID(ctx, strings.TrimSpace(tenantID)); err != nil {
		return nil, fmt.Errorf("resolve tenant: %w", err)
	}
	prices, err := s.pricing.ListEndpointPrices(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list prices: %w", err)
	}
	return prices, nil
}

// SetPrice upserts the price (cents) a tenant pays for an endpoint call. The
// tenant must exist; the operation is idempotent (a repeated upsert with the
// same value is a no-op), so the inline editor is safe to retry.
func (s *ConsoleService) SetPrice(ctx context.Context, tenantID, endpoint string, priceCents int64) (billing.EndpointPricing, error) {
	if _, err := s.tenants.FindTenantByID(ctx, strings.TrimSpace(tenantID)); err != nil {
		return billing.EndpointPricing{}, fmt.Errorf("resolve tenant: %w", err)
	}
	p, err := billing.NewEndpointPricing(tenantID, endpoint, priceCents)
	if err != nil {
		return billing.EndpointPricing{}, err
	}
	if err := s.pricing.UpsertEndpointPrice(ctx, p); err != nil {
		return billing.EndpointPricing{}, fmt.Errorf("upsert price: %w", err)
	}
	return p, nil
}

// --- Consumption audit ---

// ConsumptionLine is one endpoint's aggregated usage in a consumption report.
type ConsumptionLine struct {
	Endpoint   string
	Calls      int
	TotalCents int64
}

// ConsumptionReport is the read-only consumption audit for one tenant: the
// per-endpoint aggregation of the append-only ledger plus the grand totals.
type ConsumptionReport struct {
	TenantID   string
	Lines      []ConsumptionLine
	TotalCalls int
	TotalCents int64
}

// ConsumptionRange bounds a consumption report by ledger-entry time. The window
// is half-open [Start, End): an entry is counted when its time is not before
// Start and strictly before End. A zero value on either side is unbounded on
// that side, so the zero ConsumptionRange selects every entry.
type ConsumptionRange struct {
	Start time.Time
	End   time.Time
}

// includes reports whether t falls inside the (possibly unbounded) window.
func (rng ConsumptionRange) includes(t time.Time) bool {
	if !rng.Start.IsZero() && t.Before(rng.Start) {
		return false
	}
	if !rng.End.IsZero() && !t.Before(rng.End) {
		return false
	}
	return true
}

// Consumption builds the per-endpoint consumption report for a tenant over all
// recorded ledger entries. See ConsumptionInRange for the time-bounded variant.
func (s *ConsoleService) Consumption(ctx context.Context, tenantID string) (ConsumptionReport, error) {
	return s.ConsumptionInRange(ctx, tenantID, ConsumptionRange{})
}

// ConsumptionInRange builds the per-endpoint consumption report for a tenant from
// its ledger, counting only entries whose time falls inside rng. The ledger is
// authoritative for billing, so the audit is a pure aggregation of recorded
// events — never a value derived from mutable state. The tenant must exist so
// the screen 404s cleanly.
func (s *ConsoleService) ConsumptionInRange(ctx context.Context, tenantID string, rng ConsumptionRange) (ConsumptionReport, error) {
	tenantID = strings.TrimSpace(tenantID)
	if _, err := s.tenants.FindTenantByID(ctx, tenantID); err != nil {
		return ConsumptionReport{}, fmt.Errorf("resolve tenant: %w", err)
	}
	entries, err := s.ledger.ListLedgerEntries(ctx, tenantID)
	if err != nil {
		return ConsumptionReport{}, fmt.Errorf("list ledger: %w", err)
	}
	byEndpoint := make(map[string]*ConsumptionLine)
	rep := ConsumptionReport{TenantID: tenantID}
	for _, e := range entries {
		if !rng.includes(e.At()) {
			continue
		}
		line, ok := byEndpoint[e.Endpoint()]
		if !ok {
			line = &ConsumptionLine{Endpoint: e.Endpoint()}
			byEndpoint[e.Endpoint()] = line
		}
		line.Calls++
		line.TotalCents += e.PriceCents()
		rep.TotalCalls++
		rep.TotalCents += e.PriceCents()
	}
	rep.Lines = make([]ConsumptionLine, 0, len(byEndpoint))
	for _, line := range byEndpoint {
		rep.Lines = append(rep.Lines, *line)
	}
	sort.SliceStable(rep.Lines, func(i, j int) bool { return rep.Lines[i].Endpoint < rep.Lines[j].Endpoint })
	return rep, nil
}
