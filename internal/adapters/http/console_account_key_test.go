package http_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/adminweb"
	httpadapter "github.com/ia-dev-sindireceita/payment/internal/adapters/http"
	persistence "github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/inmemory"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/system"
	"github.com/ia-dev-sindireceita/payment/internal/app"
	"github.com/ia-dev-sindireceita/payment/internal/domain/account"
	"github.com/ia-dev-sindireceita/payment/internal/domain/accountkey"
	"github.com/ia-dev-sindireceita/payment/internal/ports"
)

// consoleAKFixture wires the admin console with the model (b) account-key emission
// surface: a REAL in-memory account-key store (no DB mock) backs the mint service, so
// a generation really persists a hashed key and the returned plaintext really carries
// the ak_ shape. flagOn toggles PAYMENT_ACCOUNT_KEY_SELECTOR; mintWired toggles whether
// the mint service is supplied (to exercise the fail-closed 503). A real account
// "verz-1" is seeded.
type consoleAKFixture struct {
	handler http.Handler
	keys    *persistence.AccountKeyStore
}

func newConsoleAKFixture(t *testing.T, flagOn, mintWired bool) *consoleAKFixture {
	t.Helper()
	store := persistence.NewStore()
	creds := secret.NewStore(map[string]ports.BankCredential{})
	if err := store.SaveAccount(context.Background(), account.Rehydrate("verz-1", "Verz", true, time.Unix(100, 0).UTC())); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	console := app.NewConsoleService(app.ConsoleDeps{
		Tenants: store, Accounts: store, Pricing: store, Ledger: store, Invoices: store,
		Audit: store, CredWriter: creds, CredReader: creds,
		Clock: fixedClock{}, IDs: &incIDs{},
	})
	ui, err := adminweb.New()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	auth := httpadapter.NewStaticTokenAuthWithRoles(nil, map[string]httpadapter.Role{
		adminToken:    httpadapter.RoleAdmin,
		operatorToken: httpadapter.RoleOperator,
	}, nil)
	keys := persistence.NewAccountKeyStore(system.Clock{})
	cfg := httpadapter.Config{
		Console: console, UI: ui, AdminAuth: auth, TenantAuth: auth, WebhookAuth: auth,
		AccountKeyAuth:     keys,
		AccountKeySelector: flagOn,
	}
	if mintWired {
		cfg.AccountKeyMint = app.NewAccountKeyService(keys, system.Clock{})
	}
	srv := httpadapter.NewServer(cfg)
	return &consoleAKFixture{handler: srv.Router(), keys: keys}
}

const akCardPath = "/console/accounts/verz-1/account-key"

// genAccountKeyForm builds the generate-key form body carrying the per-request
// idempotency nonce (the hidden field the card renders).
func genAccountKeyForm(nonce string) url.Values {
	return url.Values{"idempotency_key": {nonce}}
}

// TestConsoleAccountKeySectionFlagGated proves the "Chave de Acesso" section renders on
// the account detail only when PAYMENT_ACCOUNT_KEY_SELECTOR is on.
func TestConsoleAccountKeySectionFlagGated(t *testing.T) {
	t.Parallel()

	// Flag off: the section is absent.
	off := newConsoleAKFixture(t, false, true)
	rec := consoleGet(t, off.handler, "/console/accounts/verz-1", operatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail (flag off) = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Chave de Acesso") {
		t.Fatalf("flag off must hide the account-key section:\n%s", rec.Body.String())
	}

	// Flag on: the section and its generate control render.
	on := newConsoleAKFixture(t, true, true)
	rec = consoleGet(t, on.handler, "/console/accounts/verz-1", operatorToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail (flag on) = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Chave de Acesso") {
		t.Fatalf("flag on must show the account-key section:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="`+akCardPath+`"`) {
		t.Fatalf("generate control missing hx-post to %s:\n%s", akCardPath, body)
	}
	if !strings.Contains(body, "hx-confirm=") {
		t.Fatalf("generate control missing confirmation:\n%s", body)
	}
	// No secret is ever shown on a plain render.
	if strings.Contains(body, "ak_") {
		t.Fatalf("plain render must never carry a key plaintext:\n%s", body)
	}
}

// TestConsoleGenerateAccountKeyDisplayOnce proves an admin generates a key via HTMX and
// the ak_ plaintext is returned exactly once, in the swapped card.
func TestConsoleGenerateAccountKeyDisplayOnce(t *testing.T) {
	t.Parallel()
	f := newConsoleAKFixture(t, true, true)
	csrf := acctCSRF(t, f.handler, adminToken)

	// Read-only operator cannot mint (admin-only mutation) → 403.
	if rec := consolePost(t, f.handler, akCardPath, operatorToken, genAccountKeyForm("nonce-op"), csrf); rec.Code != http.StatusForbidden {
		t.Fatalf("operator generate = %d, want 403", rec.Code)
	}
	// Missing CSRF is rejected (not 200).
	if rec := consolePost(t, f.handler, akCardPath, adminToken, genAccountKeyForm("nonce-nocsrf"), nil); rec.Code == http.StatusOK {
		t.Fatalf("no-csrf generate should not be 200, got %d", rec.Code)
	}

	// Admin generates → 200, the swapped card carries the ak_ plaintext once.
	rec := consolePost(t, f.handler, akCardPath, adminToken, genAccountKeyForm("nonce-1"), csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin generate = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	secret := extractAKSecret(t, body)
	if !accountkey.HasSecretShape(secret) {
		t.Fatalf("returned value %q lacks account-key shape", secret)
	}
	// The generated key really authenticates against the store (no mock): the mint
	// actually persisted it.
	if acct, ok := f.keys.AuthenticateAccountKey(context.Background(), secret); !ok || acct != "verz-1" {
		t.Fatalf("generated key does not authenticate: ok=%v acct=%q", ok, acct)
	}
	// The card carries a fresh nonce for a subsequent regeneration and a copy-once warning.
	if !strings.Contains(body, "não será exibida novamente") {
		t.Fatalf("display-once warning missing:\n%s", body)
	}
}

// TestConsoleGenerateAccountKeyDoubleSubmitDeduped proves that re-posting the SAME
// idempotency nonce does not mint a second key (409, no swap) — the first key stays valid.
func TestConsoleGenerateAccountKeyDoubleSubmitDeduped(t *testing.T) {
	t.Parallel()
	f := newConsoleAKFixture(t, true, true)
	csrf := acctCSRF(t, f.handler, adminToken)

	first := consolePost(t, f.handler, akCardPath, adminToken, genAccountKeyForm("dup"), csrf)
	if first.Code != http.StatusOK {
		t.Fatalf("first generate = %d, want 200", first.Code)
	}
	secret := extractAKSecret(t, first.Body.String())

	// Replay of the same nonce → 409, no plaintext returned again.
	replay := consolePost(t, f.handler, akCardPath, adminToken, genAccountKeyForm("dup"), csrf)
	if replay.Code != http.StatusConflict {
		t.Fatalf("replay = %d, want 409", replay.Code)
	}
	if strings.Contains(replay.Body.String(), "ak_") {
		t.Fatalf("replay must not re-emit a key plaintext:\n%s", replay.Body.String())
	}
	// The first key still authenticates (the replay did not rotate it away).
	if _, ok := f.keys.AuthenticateAccountKey(context.Background(), secret); !ok {
		t.Fatalf("first key must remain valid after a deduped double-submit")
	}

	// A fresh nonce genuinely rotates: the old key stops authenticating, the new one works.
	rot := consolePost(t, f.handler, akCardPath, adminToken, genAccountKeyForm("fresh"), csrf)
	if rot.Code != http.StatusOK {
		t.Fatalf("rotate = %d, want 200", rot.Code)
	}
	newSecret := extractAKSecret(t, rot.Body.String())
	if _, ok := f.keys.AuthenticateAccountKey(context.Background(), secret); ok {
		t.Fatalf("old key must be invalidated after a real rotation")
	}
	if _, ok := f.keys.AuthenticateAccountKey(context.Background(), newSecret); !ok {
		t.Fatalf("new key must authenticate after rotation")
	}
}

// TestConsoleGenerateAccountKeyFlagOffAndMissingNonce covers the fail-closed paths.
func TestConsoleGenerateAccountKeyFlagOffAndMissingNonce(t *testing.T) {
	t.Parallel()

	// Flag off: the write route fails closed with the clean 404 (non-discoverable).
	off := newConsoleAKFixture(t, false, true)
	csrf := acctCSRF(t, off.handler, adminToken)
	if rec := consolePost(t, off.handler, akCardPath, adminToken, genAccountKeyForm("x"), csrf); rec.Code != http.StatusNotFound {
		t.Fatalf("flag-off generate = %d, want 404", rec.Code)
	}

	// Flag on but no mint service wired: 503.
	nomint := newConsoleAKFixture(t, true, false)
	csrf = acctCSRF(t, nomint.handler, adminToken)
	if rec := consolePost(t, nomint.handler, akCardPath, adminToken, genAccountKeyForm("x"), csrf); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-mint generate = %d, want 503", rec.Code)
	}

	// Flag on, mint wired, but the idempotency nonce is absent (hand-crafted request): 400.
	f := newConsoleAKFixture(t, true, true)
	csrf = acctCSRF(t, f.handler, adminToken)
	if rec := consolePost(t, f.handler, akCardPath, adminToken, url.Values{}, csrf); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-nonce generate = %d, want 400", rec.Code)
	}

	// Unknown account (flag on, mint wired) → clean 404.
	if rec := consolePost(t, f.handler, "/console/accounts/nope/account-key", adminToken, genAccountKeyForm("x"), csrf); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-account generate = %d, want 404", rec.Code)
	}
}

// extractAKSecret pulls the ak_ token out of the rendered card (the value inside the
// <code class="secret"> element). It fails the test if none is present.
func extractAKSecret(t *testing.T, body string) string {
	t.Helper()
	const marker = "ak_"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no ak_ secret in body:\n%s", body)
	}
	// The token runs until the next '<' (closing the <code> element) or whitespace.
	rest := body[i:]
	end := strings.IndexAny(rest, "< \n\t\r")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}
