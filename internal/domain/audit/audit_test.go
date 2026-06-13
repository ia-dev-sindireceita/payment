package audit_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/audit"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

func TestNewEntryValid(t *testing.T) {
	t.Parallel()
	at := time.Unix(1700000000, 0).UTC()
	e, err := audit.NewEntry("  id-1  ", "  op-7  ", audit.ActionSetBankCredential, "  ten-9  ", at)
	if err != nil {
		t.Fatalf("NewEntry: %v", err)
	}
	if e.ID() != "id-1" {
		t.Errorf("id not trimmed: %q", e.ID())
	}
	if e.OperatorID() != "op-7" {
		t.Errorf("operator not trimmed: %q", e.OperatorID())
	}
	if e.Action() != audit.ActionSetBankCredential {
		t.Errorf("action: %q", e.Action())
	}
	if e.TenantID() != "ten-9" {
		t.Errorf("tenant not trimmed: %q", e.TenantID())
	}
	if !e.At().Equal(at) {
		t.Errorf("at: %v", e.At())
	}
}

func TestNewEntryAllowsEmptyOperator(t *testing.T) {
	t.Parallel()
	// An internal (non-attributed) caller is valid: operator id may be empty.
	if _, err := audit.NewEntry("id-1", "", audit.ActionCreateTenant, "ten-1", time.Now()); err != nil {
		t.Fatalf("empty operator should be allowed: %v", err)
	}
}

func TestNewEntryValidation(t *testing.T) {
	t.Parallel()
	at := time.Unix(1, 0)
	cases := []struct {
		name       string
		id, tenant string
		action     audit.Action
	}{
		{"missing id", "", "ten-1", audit.ActionCreateTenant},
		{"missing tenant", "id-1", "", audit.ActionCreateTenant},
		{"unknown action", "id-1", "ten-1", audit.Action("hack")},
		{"empty action", "id-1", "ten-1", audit.Action("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := audit.NewEntry(tc.id, "op-1", tc.action, tc.tenant, at)
			if !errors.Is(err, shared.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}

func TestKnownActionsValid(t *testing.T) {
	t.Parallel()
	for _, a := range []audit.Action{
		audit.ActionCreateTenant, audit.ActionSetEndpointPrice, audit.ActionSetBankCredential,
		audit.ActionSuspendTenant, audit.ActionActivateTenant,
	} {
		if _, err := audit.NewEntry("id", "op", a, "ten", time.Now()); err != nil {
			t.Errorf("action %q should be valid: %v", a, err)
		}
	}
}
