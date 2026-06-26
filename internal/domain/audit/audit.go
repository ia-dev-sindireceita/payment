// Package audit holds the immutable, append-only audit trail of privileged
// admin-plane actions. An Entry captures who (the server-derived operator id),
// what (the action), the target tenant and when — it is the forensic/compliance
// record for cross-tenant privileged operations (credential writes, tenant
// provisioning, pricing changes, and future activation/suspension).
//
// An Entry NEVER carries a secret value or any credential material: it records
// only the fact that an action occurred and by whom (threat C1/C4). Pure domain:
// this package MUST NOT import database/sql, net/http or vendor SDKs.
package audit

import (
	"strings"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
)

// Action is the privileged operation an audit Entry records. Actions are a
// closed vocabulary so the trail stays queryable and tamper-evident.
type Action string

const (
	// ActionCreateTenant records provisioning of a new tenant.
	ActionCreateTenant Action = "tenant.create"
	// ActionSetEndpointPrice records an upsert of a tenant's endpoint pricing.
	ActionSetEndpointPrice Action = "pricing.set"
	// ActionSetBankCredential records a write of a tenant's bank (PSP) credential.
	// The audit entry names the tenant and operator only — never the secret.
	ActionSetBankCredential Action = "credential.set"
	// ActionSuspendTenant records suspension of a tenant (reserved for when the
	// lifecycle operation exists).
	ActionSuspendTenant Action = "tenant.suspend"
	// ActionActivateTenant records (re)activation of a tenant (reserved).
	ActionActivateTenant Action = "tenant.activate"
	// ActionSettlementAmountMismatch records a money-movement divergence: a charge
	// the PSP marked paid whose received amount did not match the expected
	// (original) amount, so settlement was refused (reconcile-before-settle, threat
	// W3). Unlike the other actions this is a system-actor event (no human
	// operator) and carries the expected/received cents and the txid.
	ActionSettlementAmountMismatch Action = "settlement.amount_mismatch"

	// ActionRecCreated..ActionRecCancelled record the lifecycle transitions of a PIX
	// Automático recurring mandate (Rec, SIN-66037). They reuse the durable
	// audit_log mechanism (SIN-66016/66025) rather than a parallel trail: the action
	// names WHICH transition occurred and the entry's TxID() carries the subject
	// idRec (see NewRecurrenceTransitionEntry). Append-only ordering over a tenant's
	// entries reconstructs the full mandate history. No secret is involved — a Rec
	// transition records only who/what/which-mandate/tenant/when.
	ActionRecCreated   Action = "recurrence.created"
	ActionRecApproved  Action = "recurrence.approved"
	ActionRecRejected  Action = "recurrence.rejected"
	ActionRecExpired   Action = "recurrence.expired"
	ActionRecCancelled Action = "recurrence.cancelled"
)

// recurrenceActionByStatus maps a recurrence.RecStatus string to the audit Action
// that records reaching it. The keys are the (closed) status vocabulary the
// recurrence domain transitions through; they are kept as plain strings so the
// audit package stays decoupled from the recurrence domain (no import cycle, and
// audit owns its own action vocabulary). A status with no recordable transition
// (none today) is reported as not-ok.
var recurrenceActionByStatus = map[string]Action{
	"CRIADA":    ActionRecCreated,
	"APROVADA":  ActionRecApproved,
	"REJEITADA": ActionRecRejected,
	"EXPIRADA":  ActionRecExpired,
	"CANCELADA": ActionRecCancelled,
}

// valid reports whether a is a known action (deny-by-default: unknown actions
// are rejected so a typo can never produce an unclassifiable audit record).
func (a Action) valid() bool {
	switch a {
	case ActionCreateTenant, ActionSetEndpointPrice, ActionSetBankCredential,
		ActionSuspendTenant, ActionActivateTenant, ActionSettlementAmountMismatch,
		ActionRecCreated, ActionRecApproved, ActionRecRejected, ActionRecExpired,
		ActionRecCancelled:
		return true
	default:
		return false
	}
}

// Entry is an immutable record of one privileged admin-plane or system action.
// Fields are unexported and exposed via accessors so an entry cannot be mutated
// after construction (append-only at the type level, mirroring
// billing.LedgerEntry).
//
// txID is the subject resource id: the charge txid for a money-movement event
// (ActionSettlementAmountMismatch) or the mandate idRec for a recurrence
// transition (ActionRec*). expectedCents/receivedCents are populated only for a
// settlement mismatch. bankID is populated only for a credential write
// (ActionSetBankCredential). They are zero-valued for the other actions. No field
// ever carries a secret — bankID is a non-secret routing slug (ADR-0007).
type Entry struct {
	id            string
	operatorID    string
	action        Action
	tenantID      string
	at            time.Time
	txID          string
	expectedCents int64
	receivedCents int64
	bankID        string
}

// NewEntry builds an audit entry, enforcing invariants: a non-empty id, a known
// action and a target tenant. The operator id is intentionally allowed to be
// empty: it denotes a non-attributed internal caller (the HTTP admin middleware
// always populates it server-side for real requests). NewEntry rejects any
// attempt to smuggle a secret by construction — it has no secret parameter.
func NewEntry(id, operatorID string, action Action, tenantID string, at time.Time) (Entry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Entry{}, shared.NewValidationError("id", "audit entry id is required")
	}
	if !action.valid() {
		return Entry{}, shared.NewValidationError("action", "unknown audit action")
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Entry{}, shared.NewValidationError("tenant_id", "tenant id is required")
	}
	return Entry{
		id:         id,
		operatorID: strings.TrimSpace(operatorID),
		action:     action,
		tenantID:   tenantID,
		at:         at,
	}, nil
}

// NewCredentialSetEntry builds the audit record for a bank credential write
// (ActionSetBankCredential). Beyond who/what/tenant/when it carries the non-secret
// bankID, so the trail records WHICH bank's credential was (re)written for the
// tenant. It NEVER records the secret or the client id by construction — it has
// no such parameter (threat C1/C4). Invariants: a non-empty id, tenant and bankID;
// the admin service normalizes an empty selector to the default bank before
// calling, so a blank bankID here is a programming error.
func NewCredentialSetEntry(id, operatorID, tenantID, bankID string, at time.Time) (Entry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Entry{}, shared.NewValidationError("id", "audit entry id is required")
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Entry{}, shared.NewValidationError("tenant_id", "tenant id is required")
	}
	bankID = strings.TrimSpace(bankID)
	if bankID == "" {
		return Entry{}, shared.NewValidationError("bank_id", "bank id is required")
	}
	return Entry{
		id:         id,
		operatorID: strings.TrimSpace(operatorID),
		action:     ActionSetBankCredential,
		tenantID:   tenantID,
		at:         at,
		bankID:     bankID,
	}, nil
}

// NewSettlementMismatchEntry builds the audit record for a refused settlement: a
// charge the PSP marked paid whose received amount did not match the expected
// amount (reconcile-before-settle, threat W3). It is a system-actor event — the
// operatorID is a reserved synthetic id (e.g. "system:c6-webhook"), since a PSP
// webhook has no human operator. It records who/what/tenant/when plus the txid
// and the expected/received cents so the divergence is durably queryable; it
// carries no secret by construction (it has no secret parameter).
//
// Invariants: a non-empty id, tenant and txid. The amounts are recorded verbatim
// (including a zero received) — the whole point is to capture what diverged.
func NewSettlementMismatchEntry(id, operatorID, tenantID, txID string, expectedCents, receivedCents int64, at time.Time) (Entry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Entry{}, shared.NewValidationError("id", "audit entry id is required")
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Entry{}, shared.NewValidationError("tenant_id", "tenant id is required")
	}
	txID = strings.TrimSpace(txID)
	if txID == "" {
		return Entry{}, shared.NewValidationError("tx_id", "tx id is required")
	}
	return Entry{
		id:            id,
		operatorID:    strings.TrimSpace(operatorID),
		action:        ActionSettlementAmountMismatch,
		tenantID:      tenantID,
		at:            at,
		txID:          txID,
		expectedCents: expectedCents,
		receivedCents: receivedCents,
	}, nil
}

// NewRecurrenceTransitionEntry builds the audit record for a PIX Automático
// mandate (Rec) status transition (SIN-66037). toStatus is the recurrence status
// the mandate moved to (the closed CRIADA/APROVADA/REJEITADA/EXPIRADA/CANCELADA
// vocabulary, passed as a plain string so audit stays decoupled from the
// recurrence domain); it maps to the matching recurrence.* Action. The subject
// idRec is carried in the existing tx_id column — the audit_log row's "subject
// resource id" — so no schema change is needed (it reuses the durable mechanism
// from SIN-66016/66025 rather than introducing a parallel trail). It records only
// who/what/which-mandate/tenant/when and carries no secret by construction.
//
// Invariants: a non-empty id, tenant and idRec, plus a recognised toStatus. When
// emitted inside the unit of work that performed the transition, the audit append
// and the Rec save commit atomically (the bundled ports.Repository), closing the
// forensic-gap window.
func NewRecurrenceTransitionEntry(id, operatorID, tenantID, idRec, toStatus string, at time.Time) (Entry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Entry{}, shared.NewValidationError("id", "audit entry id is required")
	}
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return Entry{}, shared.NewValidationError("tenant_id", "tenant id is required")
	}
	idRec = strings.TrimSpace(idRec)
	if idRec == "" {
		return Entry{}, shared.NewValidationError("id_rec", "id rec is required")
	}
	action, ok := recurrenceActionByStatus[strings.TrimSpace(toStatus)]
	if !ok {
		return Entry{}, shared.NewValidationError("status", "unknown recurrence transition status")
	}
	return Entry{
		id:         id,
		operatorID: strings.TrimSpace(operatorID),
		action:     action,
		tenantID:   tenantID,
		at:         at,
		txID:       idRec,
	}, nil
}

// ID returns the audit entry identifier.
func (e Entry) ID() string { return e.id }

// OperatorID returns the server-derived id of the operator who performed the
// action, or "" for a non-attributed internal caller.
func (e Entry) OperatorID() string { return e.operatorID }

// Action returns the recorded privileged action.
func (e Entry) Action() Action { return e.action }

// TenantID returns the tenant the action targeted.
func (e Entry) TenantID() string { return e.tenantID }

// At returns the time the action occurred.
func (e Entry) At() time.Time { return e.at }

// TxID returns the subject resource id: the charge txid for a money-movement
// event, the mandate idRec for a recurrence transition, or "" for an admin-plane
// action.
func (e Entry) TxID() string { return e.txID }

// ExpectedCents returns the expected (original) charge amount in cents for a
// money-movement event, or 0 for an admin-plane action.
func (e Entry) ExpectedCents() int64 { return e.expectedCents }

// ReceivedCents returns the received amount in cents for a money-movement event,
// or 0 for an admin-plane action.
func (e Entry) ReceivedCents() int64 { return e.receivedCents }

// BankID returns the non-secret bank slug for a credential.set event, or "" for
// any other action.
func (e Entry) BankID() string { return e.bankID }
