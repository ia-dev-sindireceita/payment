package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/account"
	"github.com/ia-dev-sindireceita/payment/internal/domain/accountkey"
	"github.com/ia-dev-sindireceita/payment/internal/domain/audit"
	"github.com/ia-dev-sindireceita/payment/internal/domain/shared"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// newAccountConsoleWithKeys builds a ConsoleService wired with the account plane AND
// the account-key emission use-case over REAL in-memory stores (no DB mock), so
// GenerateAccountKey exercises the same create==rotate + display-once path as the
// JSON routes. It returns the account-key store too so tests can authenticate the
// minted secret and prove rotation invalidated the previous key.
func newAccountConsoleWithKeys(clk ports.Clock) (*app.ConsoleService, *persistence.Store, *persistence.AccountKeyStore) {
	store := persistence.NewStore()
	creds := secret.NewStore(map[string]ports.BankCredential{})
	keys := persistence.NewAccountKeyStore(clk)
	svc := app.NewConsoleService(app.ConsoleDeps{
		Tenants:     store,
		Accounts:    store,
		Pricing:     store,
		Ledger:      store,
		Invoices:    store,
		Audit:       store,
		CredWriter:  creds,
		CredReader:  creds,
		Clock:       clk,
		IDs:         &seqIDs{},
		AccountKeys: app.NewAccountKeyService(keys, clk),
	})
	return svc, store, keys
}

func countMintActions(entries []audit.Entry) int {
	n := 0
	for _, e := range entries {
		if e.Action() == audit.ActionMintAccountKey {
			n++
		}
	}
	return n
}

func TestConsoleGenerateAccountKey_MintsAuditsAuthenticates(t *testing.T) {
	t.Parallel()
	clk := fixedClock{t: time.Unix(9000, 0).UTC()}
	svc, store, keys := newAccountConsoleWithKeys(clk)
	ctx := app.WithOperatorID(context.Background(), "op-1")

	a, err := svc.CreateAccount(ctx, "Verz")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	secret, err := svc.GenerateAccountKey(ctx, a.ID(), "idem-1")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !accountkey.HasSecretShape(secret) {
		t.Fatalf("minted secret %q lacks the account-key shape", secret)
	}
	// The minted plaintext authenticates back to exactly this account.
	gotID, ok := keys.AuthenticateAccountKey(ctx, secret)
	if !ok || gotID != a.ID() {
		t.Fatalf("authenticate = %q, %v; want %q", gotID, ok, a.ID())
	}

	// Exactly one account-scoped mint is audited: who / which-account / when — and
	// NO tenant id (it is an account-scoped action, not a tenant one).
	entries := store.AuditEntries()
	if got := countMintActions(entries); got != 1 {
		t.Fatalf("mint audit count = %d, want 1", got)
	}
	for _, e := range entries {
		if e.Action() != audit.ActionMintAccountKey {
			continue
		}
		if e.AccountID() != a.ID() {
			t.Fatalf("audit account id = %q, want %q", e.AccountID(), a.ID())
		}
		if e.OperatorID() != "op-1" {
			t.Fatalf("audit operator = %q, want op-1", e.OperatorID())
		}
		if e.TenantID() != "" {
			t.Fatalf("account-scoped mint must not carry a tenant id, got %q", e.TenantID())
		}
	}
}

func TestConsoleGenerateAccountKey_RotateInvalidatesPrevious(t *testing.T) {
	t.Parallel()
	clk := fixedClock{t: time.Unix(9000, 0).UTC()}
	svc, _, keys := newAccountConsoleWithKeys(clk)
	ctx := context.Background()

	a, err := svc.CreateAccount(ctx, "Verz")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	first, err := svc.GenerateAccountKey(ctx, a.ID(), "idem-1")
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	// A deliberate regeneration carries a FRESH nonce and mints a new key.
	second, err := svc.GenerateAccountKey(ctx, a.ID(), "idem-2")
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if first == second {
		t.Fatalf("rotation returned the same secret twice")
	}
	// The previous key stops authenticating immediately (create==rotate).
	if _, ok := keys.AuthenticateAccountKey(ctx, first); ok {
		t.Fatalf("rotated-out key still authenticates")
	}
	if id, ok := keys.AuthenticateAccountKey(ctx, second); !ok || id != a.ID() {
		t.Fatalf("new key authenticate = %q, %v; want %q", id, ok, a.ID())
	}
}

func TestConsoleGenerateAccountKey_ReplayNoSecondSecret(t *testing.T) {
	t.Parallel()
	clk := fixedClock{t: time.Unix(9000, 0).UTC()}
	svc, store, keys := newAccountConsoleWithKeys(clk)
	ctx := context.Background()

	a, err := svc.CreateAccount(ctx, "Verz")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	first, err := svc.GenerateAccountKey(ctx, a.ID(), "idem-dup")
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	// A double-submit replays the SAME nonce: no second mint, and the secret is
	// NEVER returned twice (display-once).
	replay, err := svc.GenerateAccountKey(ctx, a.ID(), "idem-dup")
	if !errors.Is(err, app.ErrAccountKeyAlreadyRotated) {
		t.Fatalf("replay err = %v, want ErrAccountKeyAlreadyRotated", err)
	}
	if replay != "" {
		t.Fatalf("replay leaked a secret: %q", replay)
	}
	// A replay mints nothing, so it audits nothing: only the original mint is recorded.
	if got := countMintActions(store.AuditEntries()); got != 1 {
		t.Fatalf("mint audit count after replay = %d, want 1", got)
	}
	// The original key is untouched (a replay must not invalidate it).
	if id, ok := keys.AuthenticateAccountKey(ctx, first); !ok || id != a.ID() {
		t.Fatalf("original key invalidated by replay: %q, %v", id, ok)
	}
}

func TestConsoleGenerateAccountKey_RefusesSelfAccount(t *testing.T) {
	t.Parallel()
	clk := fixedClock{t: time.Unix(9000, 0).UTC()}
	svc, store, _ := newAccountConsoleWithKeys(clk)
	ctx := context.Background()

	// A derived self-account (the legacy 1:1 "acct-<tenantID>" backfill) has no
	// chave-de-Conta: minting one is a validation error, and nothing is audited.
	selfID := account.SelfAccountID("tnt-legacy")
	seedAccount(t, store, selfID, "Conta própria (legado)", true, 8000)

	if _, err := svc.GenerateAccountKey(ctx, selfID, "idem-x"); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("self-account mint err = %v, want validation", err)
	}
	if got := countMintActions(store.AuditEntries()); got != 0 {
		t.Fatalf("refused mint still audited %d times", got)
	}
}

func TestConsoleGenerateAccountKey_NotFound(t *testing.T) {
	t.Parallel()
	clk := fixedClock{t: time.Unix(9000, 0).UTC()}
	svc, _, _ := newAccountConsoleWithKeys(clk)

	if _, err := svc.GenerateAccountKey(context.Background(), "no-such-account", "idem"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("missing account err = %v, want not found", err)
	}
}

func TestConsoleGenerateAccountKey_Unavailable(t *testing.T) {
	t.Parallel()
	// A console wired WITHOUT an account-key minter fails closed with the 503
	// sentinel rather than panicking on a nil dependency.
	svc, _ := newAccountConsole()
	if _, err := svc.GenerateAccountKey(context.Background(), "acct-anything", "idem"); !errors.Is(err, app.ErrAccountKeysUnavailable) {
		t.Fatalf("nil minter err = %v, want ErrAccountKeysUnavailable", err)
	}
}
