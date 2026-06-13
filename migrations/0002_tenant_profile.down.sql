-- 0002_tenant_profile.down.sql — rollback of 0002_tenant_profile.up.sql.
DROP INDEX IF EXISTS ix_tenants_cnpj;
ALTER TABLE tenants DROP COLUMN email;
ALTER TABLE tenants DROP COLUMN cnpj;
ALTER TABLE tenants DROP COLUMN display_name;
