package sqlite

import (
	"context"
	"fmt"

	"github.com/ia-dev-sindireceita/payment/internal/adapters/secret"
)

// openForReseal decrypts a blob written by a previous cipher, transparently
// upgrading legacy blobs. It first tries the AAD-bound Open (the row is bound to
// its (tenant, bank), the posture since SIN-69369); if that fails it retries with
// no AAD, which recovers blobs written before row-binding existed (pre-SIN-69369,
// sealed with a nil AAD). Both failing means the blob cannot be recovered with
// oldCipher — a wrong PAYMENT_BANK_VAULT_KEY_PREVIOUS or a corrupted row — and the
// error aborts the whole re-seal so nothing is half-rotated. This fallback lives
// ONLY in the offline re-seal path, never on the hot read path, so a genuine
// authentication failure at runtime is still fatal.
func openForReseal(oldCipher *secret.Cipher, sealed, aad []byte) ([]byte, error) {
	pt, err := oldCipher.OpenWithAAD(sealed, aad)
	if err == nil {
		return pt, nil
	}
	legacy, legacyErr := oldCipher.OpenWithAAD(sealed, nil)
	if legacyErr == nil {
		return legacy, nil
	}
	// Report the AAD-bound failure (the expected path); the legacy attempt was a
	// best-effort upgrade. Never include the blob or key material.
	return nil, fmt.Errorf("reseal: cannot open blob with previous key: %w", err)
}

// Reseal re-encrypts every bank_credentials row from oldCipher to this vault's own
// cipher, preserving the plaintext. It is the KEK-rotation and AAD-migration path
// (SIN-69369): each sealed column is opened with oldCipher (via openForReseal, so
// pre-row-binding blobs are upgraded) and re-sealed with the current cipher, bound
// to the row's RowAAD(tenant, bank). The rewrite runs in ONE transaction: on any
// error nothing is committed, so a failed rotation leaves the vault fully readable
// with the OLD key (fail-closed, reversible). Returns the number of rows rewritten.
//
// Operationally: set PAYMENT_BANK_VAULT_KEY to the NEW key and
// PAYMENT_BANK_VAULT_KEY_PREVIOUS to the CURRENT key, then run the re-seal command
// (see docs/ops/bank-vault-kek-rotation-runbook.md). Running it twice with the same
// pair fails loudly (the second pass cannot open new-key blobs with the old key)
// rather than corrupting data.
func (v *CredentialVault) Reseal(ctx context.Context, oldCipher *secret.Cipher) (int, error) {
	if oldCipher == nil {
		return 0, fmt.Errorf("reseal: previous cipher is required")
	}
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reseal credentials: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT tenant_id, bank_id, secret_sealed, creditor_key_sealed FROM bank_credentials`)
	if err != nil {
		return 0, fmt.Errorf("reseal credentials: scan: %w", err)
	}
	type row struct {
		tenantID, bankID    string
		secSealed, ckSealed []byte
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tenantID, &r.bankID, &r.secSealed, &r.ckSealed); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("reseal credentials: read row: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("reseal credentials: rows: %w", err)
	}
	_ = rows.Close()

	n := 0
	for _, r := range batch {
		aad := secret.RowAAD(r.tenantID, r.bankID)
		sec, err := openForReseal(oldCipher, r.secSealed, aad)
		if err != nil {
			return 0, err
		}
		newSec, err := v.cipher.SealWithAAD(sec, aad)
		if err != nil {
			return 0, fmt.Errorf("reseal credentials: seal secret: %w", err)
		}
		var newCK []byte
		if len(r.ckSealed) > 0 {
			ck, err := openForReseal(oldCipher, r.ckSealed, aad)
			if err != nil {
				return 0, err
			}
			if newCK, err = v.cipher.SealWithAAD(ck, aad); err != nil {
				return 0, fmt.Errorf("reseal credentials: seal creditor key: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bank_credentials SET secret_sealed = ?, creditor_key_sealed = ? WHERE tenant_id = ? AND bank_id = ?`,
			newSec, newCK, r.tenantID, r.bankID); err != nil {
			return 0, fmt.Errorf("reseal credentials: write row: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reseal credentials: commit: %w", err)
	}
	return n, nil
}

// Reseal re-encrypts every bank_certificates row's sealed private key from
// oldCipher to this vault's own cipher (see CredentialVault.Reseal for the full
// contract). cert_pem is public and left untouched. Returns the rows rewritten.
func (v *CertificateVault) Reseal(ctx context.Context, oldCipher *secret.Cipher) (int, error) {
	if oldCipher == nil {
		return 0, fmt.Errorf("reseal: previous cipher is required")
	}
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reseal certificates: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT tenant_id, bank_id, key_pem_sealed FROM bank_certificates`)
	if err != nil {
		return 0, fmt.Errorf("reseal certificates: scan: %w", err)
	}
	type row struct {
		tenantID, bankID string
		keySealed        []byte
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tenantID, &r.bankID, &r.keySealed); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("reseal certificates: read row: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("reseal certificates: rows: %w", err)
	}
	_ = rows.Close()

	n := 0
	for _, r := range batch {
		aad := secret.RowAAD(r.tenantID, r.bankID)
		key, err := openForReseal(oldCipher, r.keySealed, aad)
		if err != nil {
			return 0, err
		}
		newKey, err := v.cipher.SealWithAAD(key, aad)
		if err != nil {
			return 0, fmt.Errorf("reseal certificates: seal key: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bank_certificates SET key_pem_sealed = ? WHERE tenant_id = ? AND bank_id = ?`,
			newKey, r.tenantID, r.bankID); err != nil {
			return 0, fmt.Errorf("reseal certificates: write row: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reseal certificates: commit: %w", err)
	}
	return n, nil
}
