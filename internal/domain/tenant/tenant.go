// Package tenant holds the Tenant aggregate. A tenant is an isolated customer
// (B2B) whose data must never leak across the tenant boundary. Pure domain.
package tenant

import (
	"strings"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// Tenant is the aggregate root for an isolated customer account.
//
// Profile fields (displayName, cnpj, email) are collected by the admin console
// (screen B). They are optional at the aggregate level so the programmatic admin
// API can keep creating name-only tenants; the console enforces the richer set
// at its own boundary. cnpj is stored normalised (digits only) when present.
type Tenant struct {
	id          string
	name        string
	displayName string
	cnpj        string
	email       string
	active      bool
	createdAt   time.Time
}

// New constructs a name-only Tenant, enforcing its invariants. A tenant is
// created active. Used by the programmatic admin API.
func New(id, name string, now time.Time) (*Tenant, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, shared.NewValidationError("id", "tenant id is required")
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	return &Tenant{id: id, name: strings.TrimSpace(name), active: true, createdAt: now}, nil
}

// Profile carries the optional richer attributes the admin console collects when
// onboarding a tenant. Non-empty fields are validated (Postel: CNPJ accepted
// with or without mask, stored normalised).
type Profile struct {
	Name        string
	DisplayName string
	CNPJ        string
	Email       string
}

// NewWithProfile constructs a Tenant from the admin console form, validating the
// full profile. Required: name (razão social), cnpj, email. displayName is
// optional and defaults to name when empty.
func NewWithProfile(id string, p Profile, now time.Time) (*Tenant, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, shared.NewValidationError("id", "tenant id is required")
	}
	if err := validateName(p.Name); err != nil {
		return nil, err
	}
	cnpj, err := NormalizeCNPJ(p.CNPJ)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(p.Email)
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	display := strings.TrimSpace(p.DisplayName)
	if display == "" {
		display = strings.TrimSpace(p.Name)
	}
	if len(display) > 200 {
		return nil, shared.NewValidationError("display_name", "nome de exibição muito longo")
	}
	return &Tenant{
		id:          id,
		name:        strings.TrimSpace(p.Name),
		displayName: display,
		cnpj:        cnpj,
		email:       email,
		active:      true,
		createdAt:   now,
	}, nil
}

func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return shared.NewValidationError("name", "razão social é obrigatória")
	}
	if len(name) > 200 {
		return shared.NewValidationError("name", "razão social muito longa")
	}
	return nil
}

// Rehydrate rebuilds a name-only Tenant from persisted state without re-running
// creation validation (kept for compatibility with callers that do not need the
// profile fields).
func Rehydrate(id, name string, active bool, createdAt time.Time) *Tenant {
	return &Tenant{id: id, name: name, active: active, createdAt: createdAt}
}

// RehydrateProfile rebuilds a Tenant including its profile fields from persisted
// state without re-running validation (used by persistence adapters).
func RehydrateProfile(id, name, displayName, cnpj, email string, active bool, createdAt time.Time) *Tenant {
	return &Tenant{
		id:          id,
		name:        name,
		displayName: displayName,
		cnpj:        cnpj,
		email:       email,
		active:      active,
		createdAt:   createdAt,
	}
}

// ID returns the tenant identifier.
func (t *Tenant) ID() string { return t.id }

// Name returns the tenant legal name (razão social).
func (t *Tenant) Name() string { return t.name }

// DisplayName returns the display name, falling back to the legal name.
func (t *Tenant) DisplayName() string {
	if t.displayName == "" {
		return t.name
	}
	return t.displayName
}

// CNPJ returns the normalised (digits-only) CNPJ, or "" if unset.
func (t *Tenant) CNPJ() string { return t.cnpj }

// Email returns the contact e-mail, or "" if unset.
func (t *Tenant) Email() string { return t.email }

// Active reports whether the tenant may transact.
func (t *Tenant) Active() bool { return t.active }

// CreatedAt returns the creation timestamp.
func (t *Tenant) CreatedAt() time.Time { return t.createdAt }

// Deactivate suspends the tenant (no further transactions allowed).
func (t *Tenant) Deactivate() { t.active = false }

// Activate re-enables a suspended tenant (reversible, symmetric to Deactivate).
func (t *Tenant) Activate() { t.active = true }
