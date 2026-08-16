package http_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/adminweb"
	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/account"
	"github.com/ia-dev-sindireceita/payment/internal/domain/accountkey"
	"github.com/ia-dev-sindireceita/payment/internal/domain/audit"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// accountKeyFixture wires the console with the account plane AND the account-key
// emission use-case over real in-memory stores, seeding a real account "verz-1" and
// a legacy self-account. keys is exposed so a test can authenticate a minted secret.
type accountKeyFixture struct {
	handler http.Handler
	store   *persistence.Store
	keys    *persistence.AccountKeyStore
}

func newAccountKeyFixture(t *testing.T, wireKeys bool) *accountKeyFixture {
	t.Helper()
	store := persistence.NewStore()
	creds := secret.NewStore(map[string]ports.BankCredential{})
	keys := persistence.NewAccountKeyStore(fixedClock{})
	if err := store.SaveAccount(context.Background(), account.Rehydrate("verz-1", "Verz", true, time.Unix(100, 0).UTC())); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// A legacy self-account (the 1:1 "acct-<tenantID>" backfill): it has no
	// chave-de-Conta, so the card must be hidden and a crafted POST refused.
	selfID := account.SelfAccountID("tnt-legacy")
	if err := store.SaveAccount(context.Background(), account.Rehydrate(selfID, "Conta própria", true, time.Unix(50, 0).UTC())); err != nil {
		t.Fatalf("seed self-account: %v", err)
	}
	deps := app.ConsoleDeps{
		Tenants: store, Accounts: store, Pricing: store, Ledger: store, Invoices: store,
		Audit: store, CredWriter: creds, CredReader: creds,
		Clock: fixedClock{}, IDs: &incIDs{},
	}
	if wireKeys {
		deps.AccountKeys = app.NewAccountKeyService(keys, fixedClock{})
	}
	console := app.NewConsoleService(deps)
	ui, err := adminweb.New()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	auth := httpadapter.NewStaticTokenAuthWithRoles(nil, map[string]httpadapter.Role{
		adminToken:    httpadapter.RoleAdmin,
		operatorToken: httpadapter.RoleOperator,
	}, nil)
	srv := httpadapter.NewServer(httpadapter.Config{
		Console: console, UI: ui, AdminAuth: auth, TenantAuth: auth, WebhookAuth: auth,
	})
	return &accountKeyFixture{handler: srv.Router(), store: store, keys: keys}
}

var accountKeySecretRe = regexp.MustCompile(`<code id="account-key-secret">([^<]+)</code>`)

func mintCountHTTP(entries []audit.Entry) int {
	n := 0
	for _, e := range entries {
		if e.Action() == audit.ActionMintAccountKey {
			n++
		}
	}
	return n
}

func TestConsoleAccountDetail_KeyCardOnlyForRealAccount(t *testing.T) {
	t.Parallel()
	f := newAccountKeyFixture(t, true)

	real := consoleGet(t, f.handler, "/console/accounts/verz-1", operatorToken)
	if real.Code != http.StatusOK || !strings.Contains(real.Body.String(), "Chave-de-Conta") {
		t.Fatalf("real account detail missing key card: %d %s", real.Code, real.Body.String())
	}
	// A safe GET must never render a secret.
	if strings.Contains(real.Body.String(), `id="account-key-secret"`) {
		t.Fatalf("detail render leaked a secret box")
	}

	selfID := account.SelfAccountID("tnt-legacy")
	self := consoleGet(t, f.handler, "/console/accounts/"+selfID, operatorToken)
	if self.Code != http.StatusOK {
		t.Fatalf("self-account detail = %d", self.Code)
	}
	if strings.Contains(self.Body.String(), "Chave-de-Conta") {
		t.Fatalf("self-account must not offer a chave-de-Conta: %s", self.Body.String())
	}
}

func TestConsoleGenerateAccountKey_HappyPath(t *testing.T) {
	t.Parallel()
	f := newAccountKeyFixture(t, true)
	csrf := acctCSRF(t, f.handler, adminToken)

	// Operator (read-only) cannot mint — 403.
	if rec := consolePost(t, f.handler, "/console/accounts/verz-1/account-key", operatorToken, url.Values{"idempotency_key": {"n1"}}, csrf); rec.Code != http.StatusForbidden {
		t.Fatalf("operator mint = %d, want 403", rec.Code)
	}
	// Missing CSRF is rejected.
	if rec := consolePost(t, f.handler, "/console/accounts/verz-1/account-key", adminToken, url.Values{"idempotency_key": {"n1"}}, nil); rec.Code == http.StatusOK {
		t.Fatalf("no-csrf mint should not be 200")
	}

	// Admin mints → 200 with the secret shown once.
	rec := consolePost(t, f.handler, "/console/accounts/verz-1/account-key", adminToken, url.Values{"idempotency_key": {"n1"}}, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Chave gerada") {
		t.Fatalf("mint response missing display-once banner: %s", body)
	}
	m := accountKeySecretRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("mint response missing secret box: %s", body)
	}
	secret := m[1]
	if !accountkey.HasSecretShape(secret) {
		t.Fatalf("rendered secret %q lacks account-key shape", secret)
	}
	// The rendered secret really authenticates back to the account.
	if id, ok := f.keys.AuthenticateAccountKey(context.Background(), secret); !ok || id != "verz-1" {
		t.Fatalf("rendered secret authenticate = %q, %v; want verz-1", id, ok)
	}
	// Exactly one mint is audited.
	if got := mintCountHTTP(f.store.AuditEntries()); got != 1 {
		t.Fatalf("mint audit count = %d, want 1", got)
	}
}

func TestConsoleGenerateAccountKey_ReplayShowsNoticeNoSecret(t *testing.T) {
	t.Parallel()
	f := newAccountKeyFixture(t, true)
	csrf := acctCSRF(t, f.handler, adminToken)

	first := consolePost(t, f.handler, "/console/accounts/verz-1/account-key", adminToken, url.Values{"idempotency_key": {"dup"}}, csrf)
	if first.Code != http.StatusOK || accountKeySecretRe.FindStringSubmatch(first.Body.String()) == nil {
		t.Fatalf("first mint missing secret: %d %s", first.Code, first.Body.String())
	}
	// Replay the SAME nonce (double-submit): the notice renders, NO second secret.
	replay := consolePost(t, f.handler, "/console/accounts/verz-1/account-key", adminToken, url.Values{"idempotency_key": {"dup"}}, csrf)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d", replay.Code)
	}
	rb := replay.Body.String()
	if accountKeySecretRe.FindStringSubmatch(rb) != nil {
		t.Fatalf("replay re-showed a secret: %s", rb)
	}
	if !strings.Contains(rb, "exibida uma única vez") {
		t.Fatalf("replay missing display-once notice: %s", rb)
	}
	// Only the first submit minted, so only one audit entry exists.
	if got := mintCountHTTP(f.store.AuditEntries()); got != 1 {
		t.Fatalf("mint audit count after replay = %d, want 1", got)
	}
}

func TestConsoleGenerateAccountKey_SelfAccountRefused(t *testing.T) {
	t.Parallel()
	f := newAccountKeyFixture(t, true)
	csrf := acctCSRF(t, f.handler, adminToken)
	selfID := account.SelfAccountID("tnt-legacy")

	rec := consolePost(t, f.handler, "/console/accounts/"+selfID+"/account-key", adminToken, url.Values{"idempotency_key": {"n"}}, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-account mint = %d, want 400", rec.Code)
	}
	if got := mintCountHTTP(f.store.AuditEntries()); got != 0 {
		t.Fatalf("refused self-account mint audited %d times", got)
	}
}

func TestConsoleGenerateAccountKey_Unavailable503(t *testing.T) {
	t.Parallel()
	f := newAccountKeyFixture(t, false) // no account-key minter wired
	csrf := acctCSRF(t, f.handler, adminToken)

	rec := consolePost(t, f.handler, "/console/accounts/verz-1/account-key", adminToken, url.Values{"idempotency_key": {"n"}}, csrf)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("mint without minter = %d, want 503", rec.Code)
	}
}
