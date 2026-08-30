package sqlite_test

import (
	"bytes"
	"context"
	"sort"
	"testing"
	"time"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/persistence/sqlite"
	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
)

// The two lookups below back the SIN-69368 invariant "a PIX creditor key / PSP account
// belongs to ONE active empresa at a time" (ports.CreditorKeySharingLookup). The console
// exercises them through fakes; these tests pin the REAL sqlite adapter behaviour —
// per-row AEAD open-and-compare for the key, plaintext SQL match for the client_id — on a
// migrated database file, no mocks.

// keyA / keyB are two valid PIX creditor keys (CNPJ form) accepted by SetCreditorKey.
const (
	keyA = "12345678000199"
	keyB = "98765432000155"
)

// setKey writes a C6 credential for tenantID and registers creditorKey on it. SetCreditorKey
// requires an existing credential, so the two steps always travel together here.
func setKey(t *testing.T, v *sqlite.CredentialVault, tenantID, clientID, creditorKey string) {
	t.Helper()
	ctx := context.Background()
	if err := v.SetBankCredential(ctx, tenantID, "c6", clientID, "sec-"+tenantID); err != nil {
		t.Fatalf("set credential %s: %v", tenantID, err)
	}
	if creditorKey != "" {
		if err := v.SetCreditorKey(ctx, tenantID, creditorKey); err != nil {
			t.Fatalf("set creditor key %s: %v", tenantID, err)
		}
	}
}

func sortedEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestFindTenantsByCreditorKeyReturnsEveryActiveHolder: two empresas that registered the
// SAME PIX key both surface — that shared state is exactly the collision the caller must
// see. A third empresa on a different key must NOT appear.
func TestFindTenantsByCreditorKeyReturnsEveryActiveHolder(t *testing.T) {
	t.Parallel()
	_, db := openVaultDB(t)
	ctx := context.Background()
	v := newCredentialVault(t, db)

	setKey(t, v, "tnt-a", "cid-a", keyA)
	setKey(t, v, "tnt-b", "cid-b", keyA) // same key as tnt-a
	setKey(t, v, "tnt-c", "cid-c", keyB) // different key

	holders, err := v.FindTenantsByCreditorKey(ctx, "c6", keyA)
	if err != nil {
		t.Fatalf("find by creditor key: %v", err)
	}
	if !sortedEqual(holders, []string{"tnt-a", "tnt-b"}) {
		t.Fatalf("holders of keyA = %v, want [tnt-a tnt-b]", holders)
	}

	// The other key resolves to only its single holder.
	if holders, err := v.FindTenantsByCreditorKey(ctx, "c6", keyB); err != nil {
		t.Fatalf("find keyB: %v", err)
	} else if !sortedEqual(holders, []string{"tnt-c"}) {
		t.Fatalf("holders of keyB = %v, want [tnt-c]", holders)
	}
}

// TestFindTenantsByCreditorKeyDefaultsBankAndUnknownKey: an empty bankID resolves to C6
// (retro-compat), and a key nobody holds returns an empty slice, never an error.
func TestFindTenantsByCreditorKeyDefaultsBankAndUnknownKey(t *testing.T) {
	t.Parallel()
	_, db := openVaultDB(t)
	ctx := context.Background()
	v := newCredentialVault(t, db)
	setKey(t, v, "tnt-a", "cid-a", keyA)

	// Empty bankID must behave like "c6".
	if holders, err := v.FindTenantsByCreditorKey(ctx, "", keyA); err != nil {
		t.Fatalf("find with default bank: %v", err)
	} else if !sortedEqual(holders, []string{"tnt-a"}) {
		t.Fatalf("default-bank holders = %v, want [tnt-a]", holders)
	}

	// A key registered by nobody: empty, no error.
	if holders, err := v.FindTenantsByCreditorKey(ctx, "c6", "00000000000000"); err != nil {
		t.Fatalf("find unknown key: %v", err)
	} else if len(holders) != 0 {
		t.Fatalf("unknown key holders = %v, want none", holders)
	}
}

// TestFindTenantsByCreditorKeyEmptyKeyIsNoop: an empty key is not a query — it returns
// nil without touching the database, so a blank input can never match every row.
func TestFindTenantsByCreditorKeyEmptyKeyIsNoop(t *testing.T) {
	t.Parallel()
	_, db := openVaultDB(t)
	ctx := context.Background()
	v := newCredentialVault(t, db)
	setKey(t, v, "tnt-a", "cid-a", keyA)

	holders, err := v.FindTenantsByCreditorKey(ctx, "c6", "")
	if err != nil {
		t.Fatalf("empty key: %v", err)
	}
	if len(holders) != 0 {
		t.Fatalf("empty key matched %v, want none", holders)
	}
}

// TestFindTenantsByCreditorKeySkipsUnopenableRow: a row sealed under a rotated KEK cannot
// be opened, and MUST be skipped rather than aborting the whole scan — one un-openable
// row must not block every future key write. A vault with the wrong cipher therefore sees
// no holders, not an error.
func TestFindTenantsByCreditorKeySkipsUnopenableRow(t *testing.T) {
	t.Parallel()
	_, db := openVaultDB(t)
	ctx := context.Background()
	setKey(t, newCredentialVault(t, db), "tnt-a", "cid-a", keyA)

	wrongKey := bytes.Repeat([]byte{0xB6}, 32)
	wrongCipher, err := secret.NewCipher(wrongKey)
	if err != nil {
		t.Fatalf("wrong cipher: %v", err)
	}
	other := sqlite.NewCredentialVault(db, wrongCipher, fixedClock{t: time.Unix(1700000000, 0).UTC()})

	holders, err := other.FindTenantsByCreditorKey(ctx, "c6", keyA)
	if err != nil {
		t.Fatalf("scan with wrong KEK must skip rows, not error: %v", err)
	}
	if len(holders) != 0 {
		t.Fatalf("un-openable row leaked into holders: %v", holders)
	}
}

// TestFindTenantsByClientIDReturnsEveryHolder: client_id is the PSP ACCOUNT the
// account-level webhook channels are keyed by; two empresas sharing it both surface.
func TestFindTenantsByClientIDReturnsEveryHolder(t *testing.T) {
	t.Parallel()
	_, db := openVaultDB(t)
	ctx := context.Background()
	v := newCredentialVault(t, db)

	setKey(t, v, "tnt-a", "shared-cid", "")
	setKey(t, v, "tnt-b", "shared-cid", "") // same account
	setKey(t, v, "tnt-c", "own-cid", "")    // different account

	holders, err := v.FindTenantsByClientID(ctx, "c6", "shared-cid")
	if err != nil {
		t.Fatalf("find by client id: %v", err)
	}
	if !sortedEqual(holders, []string{"tnt-a", "tnt-b"}) {
		t.Fatalf("holders of shared-cid = %v, want [tnt-a tnt-b]", holders)
	}
}

// TestFindTenantsByClientIDDefaultsBankEmptyAndUnknown: empty bankID resolves to C6, an
// empty client_id is a no-op, and an unheld client_id returns an empty slice.
func TestFindTenantsByClientIDDefaultsBankEmptyAndUnknown(t *testing.T) {
	t.Parallel()
	_, db := openVaultDB(t)
	ctx := context.Background()
	v := newCredentialVault(t, db)
	setKey(t, v, "tnt-a", "cid-a", "")

	if holders, err := v.FindTenantsByClientID(ctx, "", "cid-a"); err != nil {
		t.Fatalf("default bank: %v", err)
	} else if !sortedEqual(holders, []string{"tnt-a"}) {
		t.Fatalf("default-bank holders = %v, want [tnt-a]", holders)
	}

	if holders, err := v.FindTenantsByClientID(ctx, "c6", ""); err != nil {
		t.Fatalf("empty client id: %v", err)
	} else if len(holders) != 0 {
		t.Fatalf("empty client id matched %v, want none", holders)
	}

	if holders, err := v.FindTenantsByClientID(ctx, "c6", "nobody"); err != nil {
		t.Fatalf("unknown client id: %v", err)
	} else if len(holders) != 0 {
		t.Fatalf("unknown client id holders = %v, want none", holders)
	}
}
