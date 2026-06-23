# PIX cobv + Checkout — Camada A (stub mode) — matriz de rastreabilidade

Homologação C6, F3 ([SIN-65722](/SIN/issues/SIN-65722), umbrella
[SIN-65683](/SIN/issues/SIN-65683)).

Camada A exercita PIX **cobrança com vencimento** (cobv, grupos 7.5–7.8) e Checkout
(grupos 10–12) **em modo stub** (`PAYMENT_C6_BASE_URL` vazio → in-memory bank,
`cmd/api/main.go`) — sem tocar o C6 real. Cada subitem do roteiro mapeia para um teste
automatizado e a evidência observável (status code / corpo de resposta). Esta matriz
alimenta a Camada B (`.docx` para o C6).

## Cobertura por PR

- **PR-A ([SIN-65724](/SIN/issues/SIN-65724), esta entrega):** domínio `pixcobv`
  (multa/juros pro-rata-die/desconto, devedor PF/PJ, chave do recebedor), port
  `PixDueChargeProvider`, adapter C6 (`CreateDueCharge`/`GetDueCharge`/`UpdateDueCharge`),
  use-case `PixDueChargeService`, rotas `POST/GET/PUT /v1/pix/cobv/{txid}` e webhook
  cobv (grupo 7.8, **reusa** o endpoint C6-D `/webhooks/c6/{tenantRef}`). **Grupos
  7.5–7.8 completos.**
- **PR-B ([SIN-65726](/SIN/issues/SIN-65726), bloqueada por PR-A):** Checkout
  `GET`/`DELETE /v1/checkout/{id}` + webhook de checkout (grupos 10–12). _Esta matriz
  será estendida nessa entrega._

## Endpoints PR-A (multi-tenant, deny-by-default, idempotency obrigatória nos writes)

| Método | Rota                      | Sucesso | Grupos   |
| ------ | ------------------------- | ------- | -------- |
| POST   | `/v1/pix/cobv`            | 201     | 7.5      |
| GET    | `/v1/pix/cobv/{txid}`     | 200     | 7.6      |
| PUT    | `/v1/pix/cobv/{txid}`     | 200     | 7.7      |
| POST   | `/webhooks/c6/{tenantRef}`| 202     | 7.8      |

## Postura de segurança (herdada do imediato + C6-D)

- **Auth deny-by-default** em todas as rotas `/v1/pix/cobv` (grupo autenticado do
  router); tenant derivado do credential autenticado, nunca do input do cliente
  (threat H1/P1). Isolamento cross-tenant testado (`http.TestCobvCrossTenantIsolationHTTP`).
- **Validação no boundary**: `Idempotency-Key` obrigatório nos writes, `due_date`
  RFC3339, `decodeJSON` rejeita campos desconhecidos (anti mass-assignment), invariantes
  cobv (tetos multa 2% / juros 1% a.m., desconto < principal, devedor CPF/CNPJ, chave
  obrigatória, vencimento no futuro) validadas no core antes do banco.
- **Reconcile-before-settle / money** (threat W3): a liquidação lê o estado
  autoritativo do banco; cobv reconcilia via `BankProvider.GetCharge` no mesmo webhook
  C6-D (dedup por `event_key`, mesma-401 anti-enumeração, path não logado em claro).
- **Reserve-before-bank**: cobrança reservada (idempotência) ANTES da chamada ao banco;
  txid + ledger persistidos atomicamente — erro de banco nunca bilheta
  (`app.TestCobvBankErrorDoesNotBill`, `app.TestCobvInvalidDoesNotReserve`).

## Matriz subitem → teste → evidência (PR-A)

| Subitem | Descrição                                       | Teste (camada)                                                                                                  | Evidência |
| ------- | ----------------------------------------------- | -------------------------------------------------------------------------------------------------------------- | --------- |
| 7.5     | Criar cobrança com vencimento (`cobv`)          | `app.TestCobvCreateSuccess`, `app.TestCobvCreateIdempotent`, `http.TestCobvCreateGetUpdateHTTP`, `bank.TestStubCobvCreateAndGet`, `c6.TestCreateDueChargeSuccess` | 201, `status` + QR (copia-e-cola/location) + parâmetros echo; re-submit com mesma chave resolve a mesma cobrança (sem duplicar) |
| 7.5†    | Multa/juros pro-rata-die/desconto (regras core) | `pixcobv.TestFineAndInterest`, `pixcobv.TestDiscount`, `pixcobv.TestDiscountFixed`, `pixcobv.TestExpired`        | multa cobrada integral no 1º dia de atraso; juros = principal×taxa/30×dias; desconto até o vencimento; expira após validade |
| 7.5‡    | Validação de input + devedor PF/PJ + chave      | `app.TestCobvCreateValidation`, `pixcobv.TestNewValidationErrors`, `pixcobv.TestDebtorPJ`, `bank.TestStubCobvRejectsBadInput`, `c6.TestCreateDueChargeRejectsBadInput` | 400/ErrValidation em vencimento passado, valores inválidos, devedor/chave ausentes; CPF→PF, CNPJ→PJ |
| 7.6     | Consultar cobv por `txid`                       | `app.TestCobvGet`, `http.TestCobvGetUnknownTxid`, `bank.TestStubCobvGetUnknown`, `c6.TestGetDueChargeSuccess`, `c6.TestGetDueChargeNotFoundMapping` | 200 com parâmetros reconciliados; txid desconhecido → 404 (tenant-scoped, sem disclosure cross-tenant) |
| 7.7     | Alterar (PUT) cobv                              | `app.TestCobvUpdate`, `app.TestCobvUpdateValidation`, `bank.TestStubCobvUpdate`, `bank.TestStubCobvUpdateUnknown`, `c6.TestUpdateDueChargeSuccess`, `c6.TestUpdateDueChargeNotFoundMapping` | 200 com parâmetros novos; txid desconhecido → 404; não re-bilheta (cobrança já bilhetada na criação) |
| 7.8     | Webhook PIX recebido (cobv)                     | `http.TestCobvWebhookSettlesAndDedups`, `bank.TestStubCobvReconcilableForWebhook`                              | cobv liquida via endpoint C6-D existente `/webhooks/c6/{tenantRef}` (reconcile via `GetCharge`), 202; replay deduplicado por `event_key` |

† regras de domínio puro (sem rede). ‡ guardas de boundary + core.

## Isolamento de tenant

`http.TestCobvCrossTenantIsolationHTTP` e `bank.TestStubCobvIsolation` provam que um
tenant nunca lê/altera a cobv de outro (credencial por-tenant, leitura tenant-scoped).

## Reversibilidade / rollout

- Modo stub é o default de dev/teste; o C6 real só é exercido quando
  `PAYMENT_C6_BASE_URL` é configurado (Camada B / homologação). Sem migração de schema
  nesta entrega.
- Rollback: reverter o PR; rotas `cobv` são aditivas e não alteram o imediato/boleto.
