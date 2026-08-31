package postgres_test

import (
	"context"
	"sort"
	"testing"
)

// TestFindTenantsByCreditorKey exercises the CreditorKeySharingLookup read side that
// backs the tenant-isolation write guard (assertCreditorKeyFree, SIN-69368/A01): it
// finds every tenant of a bank that holds a given PIX creditor key. The key is sealed
// at rest, so the lookup opens each row's ciphertext with its row-bound AAD and
// compares in memory — a row sealed under a rotated KEK is skipped, never fatal.
func TestFindTenantsByCreditorKey(t *testing.T) {
	_, db := openVaultDB(t)
	ctx := context.Background()
	v := newCredentialVault(t, db)

	const key = "12345678000199" // a valid PIX creditor key (CNPJ), shared by two tenants
	for _, tnt := range []string{"tnt-a", "tnt-b"} {
		if err := v.SetBankCredential(ctx, tnt, "c6", "cid-"+tnt, "sec"); err != nil {
			t.Fatalf("seed credential for %s: %v", tnt, err)
		}
		if err := v.SetCreditorKey(ctx, tnt, key); err != nil {
			t.Fatalf("seed creditor key for %s: %v", tnt, err)
		}
	}
	// A third tenant on the same bank holds a DIFFERENT key and must not match.
	if err := v.SetBankCredential(ctx, "tnt-c", "c6", "cid-c", "sec"); err != nil {
		t.Fatalf("seed tnt-c: %v", err)
	}
	if err := v.SetCreditorKey(ctx, "tnt-c", "98765432000188"); err != nil {
		t.Fatalf("seed tnt-c key: %v", err)
	}

	got, err := v.FindTenantsByCreditorKey(ctx, "c6", key)
	if err != nil {
		t.Fatalf("find by creditor key: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "tnt-a" || got[1] != "tnt-b" {
		t.Fatalf("want [tnt-a tnt-b], got %v", got)
	}

	// Empty key short-circuits to no holders (the guard treats "no key" as "nothing to
	// collide with"), and an unheld key returns none.
	if out, err := v.FindTenantsByCreditorKey(ctx, "c6", ""); err != nil || out != nil {
		t.Fatalf("empty key: want (nil,nil), got (%v,%v)", out, err)
	}
	if out, err := v.FindTenantsByCreditorKey(ctx, "c6", "00000000000000"); err != nil || len(out) != 0 {
		t.Fatalf("unheld key: want empty, got (%v,%v)", out, err)
	}
	// An empty bankID defaults to c6; a different bank isolates the lookup.
	if out, err := v.FindTenantsByCreditorKey(ctx, "sicoob", key); err != nil || len(out) != 0 {
		t.Fatalf("other bank must not match c6 holders: got (%v,%v)", out, err)
	}
}

// TestFindTenantsByClientID exercises the sibling lookup over the plaintext client_id
// (an identity, not a secret): every tenant of a bank sharing a client_id, isolated by
// bank. Backs the same A01 write guard.
func TestFindTenantsByClientID(t *testing.T) {
	_, db := openVaultDB(t)
	ctx := context.Background()
	v := newCredentialVault(t, db)

	const shared = "shared-client-id"
	if err := v.SetBankCredential(ctx, "tnt-a", "c6", shared, "sec"); err != nil {
		t.Fatalf("seed tnt-a: %v", err)
	}
	if err := v.SetBankCredential(ctx, "tnt-b", "c6", shared, "sec"); err != nil {
		t.Fatalf("seed tnt-b: %v", err)
	}
	if err := v.SetBankCredential(ctx, "tnt-c", "c6", "other-cid", "sec"); err != nil {
		t.Fatalf("seed tnt-c: %v", err)
	}

	got, err := v.FindTenantsByClientID(ctx, "", shared) // empty bank defaults to c6
	if err != nil {
		t.Fatalf("find by client id: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "tnt-a" || got[1] != "tnt-b" {
		t.Fatalf("want [tnt-a tnt-b], got %v", got)
	}

	if out, err := v.FindTenantsByClientID(ctx, "c6", ""); err != nil || out != nil {
		t.Fatalf("empty client id: want (nil,nil), got (%v,%v)", out, err)
	}
	if out, err := v.FindTenantsByClientID(ctx, "c6", "nobody"); err != nil || len(out) != 0 {
		t.Fatalf("unheld client id: want empty, got (%v,%v)", out, err)
	}
}
