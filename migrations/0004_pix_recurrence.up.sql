-- 0004_pix_recurrence.up.sql — durable PIX Automático (recorrência) records
-- (SIN-66037, F3 of the plan SIN-66030).
--
-- Persists the recurring-debit mandate (pix_rec) and each scheduled charge
-- instance of its cycle (pix_cobr) so the authorization and the cobrança cycle
-- survive a process restart. Status TRANSITIONS are NOT stored here as mutable
-- history — the durable audit_log (SIN-66016) records each transition as an
-- append-only event (recurrence.* actions), so this schema keeps only the CURRENT
-- aggregate state. The two together are the trail: pix_rec.status is "where the
-- mandate is now", audit_log is "how it got there".
--
-- Multi-tenancy: every row carries tenant_id and is queried scoped (threat P1).
-- Multi-bank: pix_rec carries bank_id, the non-secret bank slug (ADR-0007), so a
-- mandate is attributable to which bank originated it. No secret/credential/PII is
-- stored.
--
-- Portability (same conventions as 0001_init): TEXT opaque ids, TEXT RFC3339-UTC
-- timestamps, TEXT yyyy-MM-dd calendar dates, INTEGER cents (BIGINT in Postgres).
-- No SQLite-only types, so the schema ports to Postgres unchanged. Created with
-- IF NOT EXISTS so the migration is safe to re-run (the ledger also skips it).

CREATE TABLE IF NOT EXISTS pix_rec (
    id_rec        TEXT NOT NULL,
    tenant_id     TEXT NOT NULL,
    bank_id       TEXT NOT NULL,
    contrato      TEXT NOT NULL,
    devedor_doc   TEXT NOT NULL,
    devedor_nome  TEXT NOT NULL,
    data_inicial  TEXT NOT NULL, -- yyyy-MM-dd, first eligible charge date
    periodicidade TEXT NOT NULL,
    valor_cents   INTEGER NOT NULL DEFAULT 0, -- 0 = unspecified/variable
    status        TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    -- Scoped by tenant so the same id_rec space is isolated per tenant.
    PRIMARY KEY (tenant_id, id_rec)
);

-- Forensic/list query: a tenant's mandates in recency order.
CREATE INDEX IF NOT EXISTS ix_pix_rec_tenant ON pix_rec (tenant_id, created_at);

CREATE TABLE IF NOT EXISTS pix_cobr (
    tx_id       TEXT NOT NULL, -- anti-double-bill anchor
    id_rec      TEXT NOT NULL, -- the mandate this charge belongs to
    tenant_id   TEXT NOT NULL,
    vencimento  TEXT NOT NULL, -- yyyy-MM-dd due date
    valor_cents INTEGER NOT NULL,
    status      TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (tenant_id, tx_id)
);

-- Cycle view: every charge instance of a mandate, in due-date order.
CREATE INDEX IF NOT EXISTS ix_pix_cobr_tenant_rec ON pix_cobr (tenant_id, id_rec, vencimento);
