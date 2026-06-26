# SIN-66017 — UX spec: credenciais e certificados por banco + consumo por tenant

> **Stack:** HTMX + Go `html/template` + design-system tokens (`internal/adapters/adminweb/static/tokens.css` / `app.css`). Server renders HTML; `hx-*` drive partial swaps; progressive enhancement (no-JS fallback via full-page POST). **Not** a SPA. Every pattern below is HTMX-correct (swap targets + OOB), never React/SPA state.
>
> **Author:** UXDesigner (Principal Product Designer) · **Status:** proposed, awaiting CEO approval + CTO/SecurityEngineer sign-off on the certificate envelope (§7).
> **Depends on:** [SIN-66015] multi-bank data model `(tenant_id, bank_id/alias)` (CTO). Design lands first; UI implementation is spawned after the backend port stabilizes.

---

## 0. Constraints inherited from the merged console (non-negotiable)

These are facts of the existing surface (`console_handlers.go`, `server.go`, `views.go`) and the spec respects all of them:

- **RBAC at the boundary.** Reads admit `RoleAdmin` + `RoleOperator`; every mutation requires `RoleAdmin` (`requireRole`). No new role. Operators see bank/cert config read-only; only Admin writes.
- **CSRF double-submit** on every browser POST (`CSRFProtect`, `<input type="hidden" name="csrf_token">` + `hx-headers` `X-CSRF-Token`). Every new form carries it.
- **Write-only secrets.** A secret is gravado no cofre once and **never re-rendered**. Forms echo back only non-secret fields (e.g. `client_id`) and only after a successful write. This rule now extends to **private keys** (§7).
- **CSP is strict self-only:** `default-src 'none'; script-src 'self'; style-src 'self'; form-action 'self'`. No inline JS/CSS, no third-party. File upload posts same-origin (`form-action 'self'` permits it). No client-side crypto/JS parsing of certs — all cert introspection happens server-side and only **metadata** is rendered.
- **Tokens only.** Colors/spacing/type/radii/motion come from `tokens.css`. No off-scale value. New tokens are proposed explicitly (§9), none required for v1.
- **Existing components reused:** `.card`, `.tabs`, `.field`/`.help`/`.error`, `.banner banner--{success,danger}`, `.btn btn--{primary,secondary,ghost}`, `.badge badge--{success,warning}`, `.table-wrap`/`.data`, `toast_oob`, `status_header_oob` OOB pattern. Brazilian PT-BR copy, `R$ 1.234,56` currency, `dd/mm/aaaa` dates.

---

## 1. Problem & the three gaps

Today a tenant has a **flat, single-bank** identity: one `Credenciais` tab posts one `client_id`+`secret`. The board wants, per tenant: **more than one bank**, **credentials *and* certificates per bank**, and a confirmed **consumption** view.

| # | Gap | Today | Target |
|---|-----|-------|--------|
| 1 | Multi-bank | `Credenciais` tab = 1 bank, no selector | A tenant holds N banks; each bank has its own identity |
| 2 | Certificates | mTLS PEM provisioned via runbook §8, **outside the UI** | Upload/rotate cert per bank in the console, write-only, metadata-visible |
| 3 | Consumption | `/consumption` aggregates by endpoint, tenant-wide | Confirm it answers "consumo de cada tenant"; add bank dimension (§8) |

---

## 2. IA decision — introduce the **Bank** as the unit of credential identity

**Mental model (Norman conceptual model):** a tenant *has banks*; a bank *has* one credential + (optionally) one client certificate + a creditor key. Credentials and certs are **properties of a bank**, not of the tenant. So the flat `Credenciais` tab is replaced by a **`Bancos`** section that drills into a per-bank detail.

**New tab bar** (replaces line 12–20 of `tenant_detail.html` and the `<nav class="tabs">` in every tenant sub-page):

```
Visão geral │ Bancos │ Tarifação │ Consumo
```

`Credenciais` → `Bancos`. Rationale: Jakob's Law (the operator already reads tabs as "sections of this tenant"); Recognition over Recall (the bank list shows what's configured, operator doesn't recall which bank); Information Scent ("Bancos" + a count badge signals "configure banks here").

> **Back-compat:** the existing single-bank credential becomes the tenant's first bank with a default alias (`c6` / "C6 Bank"). No data migration visible to the operator; the old `/credentials` route 301→ `/banks` (or renders the bank list). Coordinated with SIN-66015's "default retrocompatível".

---

## 3. Screen map (screen → owner role → operator job-to-be-done)

| Screen | Route (GET) | Mutations (POST, Admin) | Role | JTBD |
|--------|-------------|-------------------------|------|------|
| **Bank list** | `/console/tenants/{id}/banks` | `POST .../banks` (add) | Admin write / Operator read | "Which banks does this tenant use, and are they healthy (cred set? cert valid?)" |
| **Bank detail** | `/console/tenants/{id}/banks/{bankId}` | — | Admin/Operator | "Configure this one bank end-to-end" |
| ↳ Credential card | (within detail) | `POST .../banks/{bankId}/credential` | Admin | "Set/rotate the API client_id+secret for this bank, without ever seeing the old secret" |
| ↳ Certificate card | (within detail) | `POST .../banks/{bankId}/certificate` (multipart) | Admin | "Upload / rotate the mTLS cert before it expires; confirm fingerprint & validity" |
| ↳ Creditor key field | (within detail) | `POST .../banks/{bankId}/creditor-key` | Admin | "Pin the PIX chave do recebedor for fund-routing safety" |
| **Consumption** | `/console/tenants/{id}/consumption` | — | Admin/Operator | "See this tenant's usage; optionally per bank" (§8) |

Removing a bank is **out of v1 scope** (destructive, needs reconcile/settle audit). Documented as deferred — see §10.

---

## 4. Screen — Bank list (`banks.html`)

**Purpose:** at-a-glance health of every bank the tenant uses. Dense table (operator dashboard density), one row per bank.

**Layout (reuses `.tabs`, `.table-wrap`, `.data`, `.badge`, `.btn`):**

```
‹ {{Tenant.Name}} / Bancos
[Visão geral] [Bancos] [Tarifação] [Consumo]

┌─ card ────────────────────────────────────────────────────────────────┐
│  Bancos do tenant                                   [ + Adicionar banco ]│
│  ┌──────────────────────────────────────────────────────────────────┐ │
│  │ Banco        Apelido   Credencial   Certificado          Status   │ │
│  │ C6 Bank      c6        ✓ configurada ⚠ expira em 12 dias  ● Ativo  │ │
│  │ Banco X      x-prod    — pendente    — não exigido        ● Ativo  │ │
│  └──────────────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────────────┘
```

**Columns:** Banco (display name) · Apelido (`alias`/`bank_id`, `<code>`) · Credencial (badge: `✓ configurada` success / `— pendente` warning — never the value) · Certificado (status badge, §7.3) · row links into bank detail.

**States (all get equal polish — Visual quality bar):**
- **Empty** (tenant has zero banks — only on brand-new tenants): `.empty` block, headline "Nenhum banco configurado", subtext "Adicione o primeiro banco para o tenant começar a transacionar.", primary `+ Adicionar banco`. Mirrors `tenant_rows` empty state.
- **Loading:** `hx-indicator` on the add-bank swap; row insert is OOB `afterbegin`.

**Add bank** — Progressive Disclosure via inline form (no modal needed; matches `tenant_new` inline pattern). Fields:
- **Banco** — `<select name="bank_type">` populated server-side from the registry of implemented adapters (v1: only `C6 Bank`; the select is present-but-single so the IA already scales — Postel's Law / future-proof). Disabled-looking single option is acceptable; do **not** free-text a bank name.
- **Apelido** — `<input name="alias">`, optional, defaults to the bank_type slug; lets a tenant hold two accounts at the same bank later (`c6-cobranca`, `c6-pix`). Validated server-side (slug, unique per tenant).

On success → OOB-insert the new row + `toast_oob` "Banco adicionado." + the bank-type select shrinks if the type is now exhausted. On error → inline `.error` under the offending field (reuse `fieldErrors`).

---

## 5. Screen — Bank detail (`bank_detail.html`)

One screen, **three stacked cards** (Common Region groups each concern; single-column so the operator reads top-to-bottom, Serial Position puts the highest-stakes card — credential — first):

```
‹ {{Tenant.Name}} / Bancos / C6 Bank
[Visão geral] [Bancos] [Tarifação] [Consumo]            ● Ativo

┌─ Card 1: Credencial de API ───────────────────┐
│  Client ID *  [____________]                  │
│  Secret    *  [•••• write-only]   ⓘ helper     │
│               [ Salvar credencial ]           │
│  Última gravação: 21/06/2026 (sem read-back)  │
└───────────────────────────────────────────────┘
┌─ Card 2: Certificado mTLS ────────────────────┐  (§7)
│  Status: ⚠ expira em 12 dias (até 08/07/2026) │
│  Emitido p/: CN=tenant-c6.example             │
│  Fingerprint SHA-256: AB:CD:…:9F               │
│  [ Enviar/rotacionar certificado ]            │
└───────────────────────────────────────────────┘
┌─ Card 3: Chave PIX do recebedor ──────────────┐
│  Creditor key  [____________]  ⓘ não é segredo │
│               [ Salvar chave ]                │
└───────────────────────────────────────────────┘
```

Cards 1 and 3 are the existing credential/creditor patterns scoped to `{bankId}`. Card 2 is the new certificate pattern (§7). Each card posts independently and swaps only itself back (`hx-target` = that card's id), so saving the credential doesn't blow away an in-progress cert upload (Forgiveness; no lost work — Zeigarnik).

---

## 6. Credential card — write-only, now per bank

Identical to today's `credentials.html` form, scoped to the bank and re-targeted to swap the card, not `#main`:

- `hx-post="/console/tenants/{id}/banks/{bankId}/credential"` `hx-target="#cred-card" hx-swap="outerHTML"`.
- Same rules: `secret` is `type="password" autocomplete="off"`, never re-rendered; only `client_id` echoes on error; success banner "Credencial salva. O segredo foi gravado no cofre e não é exibido novamente."
- **Add:** show **`Última gravação: dd/mm/aaaa`** (metadata only, no value) so the operator has Feedback that a secret exists without Recall — answers "is this bank configured?" Source: credential-store write timestamp (request from CTO in SIN-66015; if unavailable in v1, show only the `✓ configurada` badge).
- **Rotation = re-submit.** No separate "rotate" verb; saving a new secret replaces the old. `hx-confirm` on submit when a credential already exists: "Substituir a credencial atual? Chamadas em andamento usam a credencial nova a partir de agora." (Forgiveness on a destructive-ish action.)

---

## 7. Certificate card — the new pattern (write-only mTLS) **[needs CTO + SecurityEngineer sign-off]**

The C6 mTLS client certificate (today provisioned via runbook §8, file on disk `0600`) moves into the console as a **write-only artifact with visible metadata**. The private key follows the same rule as a secret: **written to the vault once, never rendered, no read-back**. Only the *public* certificate's derived metadata is shown — none of it is sensitive.

### 7.1 Envelope — RECOMMENDED, pending sign-off

C6 mTLS needs a **cert chain + private key**. Proposed upload form (`enctype="multipart/form-data"`):

- **Certificado (PEM)** — `<input type="file" name="cert_pem" accept=".pem,.crt,application/x-pem-file">` — the public client cert (+ chain). Not secret.
- **Chave privada (PEM)** — `<input type="file" name="key_pem" accept=".pem,.key">` — **write-only**, vault-stored, never echoed.
- **Passphrase** (optional) — `<input type="password" name="key_passphrase" autocomplete="off">` — if the key is encrypted; write-only.

> **Open question for CTO/SecurityEngineer (blocks final approval of §7 only):**
> (a) Single combined PEM bundle (cert+key in one file) vs. two separate fields? Recommendation: **two fields** — clearer signifier of "this half is public, this half is secret", and avoids a bundle that mixes a secret into a field the operator might think is public.
> (b) Where is the key stored — same `CredentialStore`/vault as `secret`, keyed by `(tenant, bank)`? (SIN-66015.)
> (c) Server parses the cert to extract metadata (CN, validity, SHA-256 fingerprint) — confirm a vetted std-lib path (`crypto/x509`) and that parsing happens **before** vault write, rejecting malformed/expired-on-upload inputs with an inline error.
> (d) Validate the key matches the cert (public-key correspondence) server-side before accepting — reject mismatched pairs with a named error, not a silent 500.

### 7.2 Validation & errors (forms-and-errors lens — name the fix)

Server-side, before any vault write:
- Not a PEM / unparseable → "Arquivo não é um certificado PEM válido. Envie o `.pem` emitido pelo banco."
- Key doesn't match cert → "A chave privada não corresponde ao certificado. Confirme que enviou o par correto."
- Already expired (`notAfter` < now) → reject: "Certificado já expirado (venceu em dd/mm/aaaa). Envie um certificado vigente."
- File too large → reuse the webhook body-cap pattern (413 / inline "Arquivo excede o limite.").

### 7.3 Certificate status — the metadata-only display

After a successful upload, the card shows (no secret, all derived from the public cert):

| Field | Source | Example |
|-------|--------|---------|
| Status badge | `notAfter` vs now | `● Válido` (success) / `⚠ Expira em 12 dias` (warning) / `✕ Expirado` (danger) |
| Emitido para | cert Subject CN | `CN=tenant-c6.example` |
| Validade | `notBefore`–`notAfter` | `01/05/2026 – 08/07/2026` |
| Fingerprint | SHA-256 of DER | `AB:CD:…:9F` (`<code>`, wraps) |

**Expiry signaling (Loss Aversion + Goal-Gradient):** warning badge when `notAfter` within **30 days**; danger when expired. Surface the same warning **as a row badge in the bank list (§4)** and as an OOB count on the `Bancos` tab so an operator scanning the tenant list sees "cert expiring" without drilling in. (Drives proactive rotation — the whole point of moving certs into the UI.)

### 7.4 Rotation

Same form = rotation. New cert atomically replaces old in vault. `hx-confirm`: "Rotacionar o certificado? A conexão mTLS passa a usar o novo certificado imediatamente." Success → swap the cert card to its metadata view + `toast_oob` "Certificado atualizado."

### 7.5 Accessibility for file upload

Native `<input type="file">` (keyboard-operable, ≥44px target, visible focus). `<label>` association, `aria-describedby` for the format hint, errors via `role="alert"`. No custom drag-drop JS (CSP forbids inline JS; native input is the inclusive-design default — curb-cut).

---

## 8. Consumption — confirmation + proposed enhancement

**Confirmation:** `/console/tenants/{id}/consumption` (per the route + `consumption.html`) **does** answer "ver o consumo de cada tenant": it aggregates the append-only ledger (autoritativo p/ faturamento) by endpoint, with per-row Calls + Total and a grand-total `tfoot`, read-only, with a polished empty state. ✔ The board requirement is met by the existing screen.

**Proposed enhancements (severity: minor / polish — non-blocking):**
1. **Bank dimension** (minor) — once multi-bank lands, a tenant's usage spans banks. Add an optional **Banco** column or a `hx-get` filter chip set (`Todos · c6 · x-prod`) that swaps the table body (`#consumption-rows`) — same partial-swap pattern as tenant search. Requires the ledger to carry `bank_id` (request to CTO; if absent, defer). Without it, the tenant-wide totals still answer the board ask.
2. **Date range** (polish) — `start_date`/`end_date` inputs (the ledger already supports a window elsewhere) so billing can pull a month. Default last 30 days. Swap `#consumption-rows`.
3. **CSV export** (polish) — `GET .../consumption.csv` link for finance. `form-action`/`connect-src 'self'` already allow a same-origin download link.

These are filed as child polish items, not gates on this spec.

---

## 9. Design-system deltas

**No new tokens required for v1.** Everything composes from existing tokens/components. Two **new template partials** (system-level additions, written down so the next screen inherits them):

1. `cert_status_badge` — maps `{Valid|ExpiringSoon|Expired}` → `badge--{success|warning|danger}`. Mirrors `status_badge`. Reusable anywhere a cert state shows.
2. `cred_status_badge` — `{Set|Pending}` → `badge--{success|warning}` with "configurada / pendente". Reusable.

**New view structs** (in `views.go`, projection-only, never carry secrets — extend the existing pattern):
- `BankRow{ DisplayName, Alias, CredentialSet bool, Cert CertMeta, Active bool }`
- `CertMeta{ Status string; SubjectCN string; NotBefore, NotAfter time.Time; FingerprintSHA256 string }` — **metadata only; no key bytes, ever.** Add `LogValue()` redaction discipline consistent with `BankCredential`.
- `BankListView{ Base; Tenant TenantView; Banks []BankRow; BankTypes []BankTypeOption; Form, Errors map[string]string }`
- `BankDetailView{ Base; Tenant TenantView; Bank BankRow; Cred CredentialView-like; Cert CertMeta; Errors map[string]string; Saved bool }`

---

## 10. Phasing & handoff (HTMX → Coder; backend gates on SIN-66015)

All UI is HTMX/Go templates → **Coder** (not FrontendCoder; no React surface here). Each child blocks on SIN-66015's `(tenant, bank)` port + (for §7) the SecurityEngineer-confirmed cert envelope.

| Child | Scope | Owner | Blocks on | Severity |
|-------|-------|-------|-----------|----------|
| C-1 | `Bancos` tab + bank list (`banks.html`) + add-bank + back-compat route | Coder | SIN-66015 | blocker (entry point) |
| C-2 | Bank detail (`bank_detail.html`): credential + creditor-key cards scoped to `{bankId}` | Coder | SIN-66015 | blocker |
| C-3 | Certificate card: upload/rotate, metadata display, expiry badges | Coder | SIN-66015 + §7 sign-off | major |
| C-4 | Consumption: bank filter + date range + CSV | Coder | ledger `bank_id` | minor/polish |

SecurityEngineer is looped in on **C-3** (write-only key handling, vault path, x509 parse, cert/key match) before that child starts. CTO owns the data-model shape feeding C-1/C-2.

---

## 11. Acceptance criteria (this spec)

- [x] Covers credentials **and** certificates **per bank**, plus consumption, aligned to the existing HTMX pattern (tabs, cards, OOB toasts, CSRF, write-only).
- [x] Security constraints honored: secrets **and** private keys write-only, never displayed; only public cert metadata shown; RBAC read/write split preserved; CSP-compatible (no inline JS, same-origin upload).
- [x] Tokens/components reused; deltas (2 badges, view structs) called out as system additions; **no off-scale values**.
- [x] Stack-correct: 100% HTMX/Go-template, partial swaps + OOB, no SPA assumptions; no-JS fallback via full POST.
- [ ] **CEO approval** of the spec.
- [ ] **CTO + SecurityEngineer sign-off** on the certificate envelope (§7.1 open questions a–d).
- [ ] Implementation child issues created (C-1..C-4), each blocked on SIN-66015 / §7 sign-off, owner Coder.

## 12. Visual-truth note

This is a **design/IA spec** (the issue's deliverable), authored against the live templates/tokens/handlers — not a verdict on a rendered build. When C-1..C-4 implement, the visual-truth gate applies to *those* PRs: render at 1440×900 desktop, screenshot each new screen + every state (empty/loading/error/expired-cert), keyboard-only walkthrough + axe pass. The Sindireceita no-Docker constraint may require the human owner to run `make up` and post screenshots for the final smoke.
