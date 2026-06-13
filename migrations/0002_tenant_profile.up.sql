-- 0002_tenant_profile.up.sql — admin console tenant profile fields.
-- Additive and backward-compatible: new columns are NOT NULL DEFAULT '' so rows
-- created by the foundation (name-only) remain valid. Portable to Postgres
-- (TEXT + DEFAULT ''). cnpj is stored normalised (digits only); '' means unset.
ALTER TABLE tenants ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE tenants ADD COLUMN cnpj         TEXT NOT NULL DEFAULT '';
ALTER TABLE tenants ADD COLUMN email        TEXT NOT NULL DEFAULT '';

-- Non-unique lookup index on cnpj (digits only). CNPJ uniqueness enforcement is
-- deferred to a follow-up so the SQLite and in-memory adapters stay behaviourally
-- identical (no divergent upsert semantics) for this slice.
CREATE INDEX IF NOT EXISTS ix_tenants_cnpj ON tenants (cnpj) WHERE cnpj <> '';
