# Custódia conjunta da KEK board↔org (Opção A) — runbook

Como a org (`ia-dev-sindireceita/payment`, go-live Verz — SIN-69118) passa a
materializar **a mesma** KEK `PAYMENT_BANK_VAULT_KEY` que o board, e o que isso
exige em custódia, entrega, rotação e revogação. Escrito para SIN-70400, a partir
da decisão **Opção A** do board em SIN-70324 (voto A do CEO, interação
`ask:SIN-70324:kek-custody:v1`, resolvida 2026-08-31).

> **Gate.** Este item **precede** o cutover Postgres real da org. Ele **não**
> bloqueia o merge de código (SIN-70319 já `done`). O passo de provisionar o
> AppRole é **operador-gated**: quem controla o Vault do board (Pericles/board)
> executa; a SecEng escreve o escopo + procedimento + verificação, abaixo.

## Opção A em uma frase: não há cópia de chave

Org e board apontam para a **mesma** produção — a instância `pre-prod` na infra
lmhost, sobre o **mesmo** PostgreSQL e o **mesmo** Vault (`minio`
`172.18.2.63:8200`, KV v2 `payment/prod`). Não existe cluster próprio da org, não
existe ETL, não existe reseal (aquilo seria Opção B). Portanto o "handoff da KEK"
**não é copiar os 32 bytes da chave** de um lugar para outro.

A KEK vive **só no Vault**. A cada boot o `payment-api` autentica por AppRole,
lê `payment/prod`, e materializa a KEK em tmpfs (`/run/payment/<inst>/`, 0600,
apagado quando o serviço para) — nunca em disco, nunca em env de CI, nunca no
repo. Ver [`vault-lmhost.md`](vault-lmhost.md) para a mecânica completa
(`vault-materialize.py` → `payment-run.sh` → `exec payment-api`).

O que a org recebe, então, é **capacidade de leitura da mesma KEK**: um AppRole
(`role_id` + `secret_id`) cuja policy dá `read` **apenas** em `payment/prod`. Com
ele, o `payment-api` da org materializa a **mesma** KEK do **mesmo** Vault, e os
cofres selados (credenciais bancárias, certificados mTLS, credencial do console,
segredo HMAC de webhook) abrem byte a byte. É custódia **conjunta** porque as duas
partes passam a deter material de acesso à mesma chave-mestra.

## Mapa de custódia — quem detém o quê

| Artefato | Onde vive | Quem detém | Quem pode rotacionar | Quem revoga em incidente |
|---|---|---|---|---|
| **KEK** `PAYMENT_BANK_VAULT_KEY` (32 B AES-256) | só no Vault `payment/prod`; em tmpfs no host durante o boot | ninguém "em mãos" — é materializada por AppRole | board (reescreve o KV) — evento raro, quebra todos os cofres se feito sem reseal | board (sela o Vault / revoga a leitura) |
| **Policy** `payment-prod` (`read` em `payment/prod`) | Vault (board) | board | board | board |
| **AppRole `payment-prod`** — `role_id` | Vault (board); estável, não é segredo forte por si só | board + org | não se rotaciona por rotina | board (deleta o role) |
| **AppRole `payment-prod`** — `secret_id` | host: `/etc/payment/approle-<inst>`, `0640 root:payment` | board (host) e, na Opção A, a org compartilha o **mesmo** host | board (procedimento abaixo) | board (`secret-id/destroy`) |
| **Unseal keys + `root_token`** | hoje em `/root/vault-init.json` na `minio` | board | board | board |

Observações de custódia conjunta:

- Como org e board **compartilham o mesmo host `pre-prod` e o mesmo processo
  `payment-api`**, na prática a org adota o AppRole `payment-prod` que **já
  existe** no host — não se cria um segundo AppRole redundante para o mesmo path.
  O "handoff" é o ato de o board **reconhecer a org como co-custodiante** desse
  AppRole e do seu `secret_id`, e documentar canal/rotação/revogação (este
  arquivo). Se e quando a org operar um **host próprio** apontando para o mesmo
  Vault, aí sim ela recebe um `secret_id` **próprio** do **mesmo** AppRole
  `payment-prod` (mesma policy, mesmo path) — nunca uma policy mais ampla.
- **Least Privilege é inegociável no escopo da policy.** Qualquer `secret_id`
  entregue à org é do AppRole `payment-prod`, cuja policy dá `read` **só** em
  `payment/prod`. Sem `payment/sbx`, sem `transit/`, sem o path da NFS-e no mesmo
  Vault. Uma policy por instância, exatamente como o board (ver
  [`vault-lmhost.md`](vault-lmhost.md) — "Uma policy por instância, com `read`
  apenas no próprio caminho").

## Escopo da policy `payment-prod` (o que a org pode ler)

A policy anexada ao AppRole entregue à org deve ser **idêntica** à do board e nada
além disto:

```hcl
# policy: payment-prod  — read-only no próprio caminho, e nada mais
path "payment/data/prod" {
  capabilities = ["read"]
}
path "payment/metadata/prod" {
  capabilities = ["read"]   # opcional; só metadados de versão do KV v2
}
# SEM payment/sbx, SEM transit/, SEM o path da NFS-e, SEM list no mount.
```

Regra de aceitação (SecEng): antes de o board entregar o AppRole, confirmar que
`vault policy read payment-prod` **não** contém nenhum `path` fora de
`payment/*/prod`. Um `list` no mount ou um curinga (`payment/*`) reprova a
entrega — enumeraria `sbx`.

## Canal de entrega do AppRole (fora de banda)

`role_id` sozinho não autentica; `secret_id` é o segredo. **Nunca** trafegue o
`secret_id` (nem o par) por:

- repositório git (nem cifrado — não é lugar de segredo de auth);
- logs de CI / `GITHUB_STEP_SUMMARY` / env de workflow;
- thread de issue do Paperclip, comentário de PR, chat público, e-mail em claro.

Entrega aceita:

1. **Preferida — sem trânsito do `secret_id`.** O board gera o `secret_id`
   **diretamente no host** e o grava em `/etc/payment/approle-<inst>` (`0640
   root:payment`) via sessão SSH do próprio operador do board. O segredo nunca
   deixa a fronteira host↔Vault. É o modelo atual do `pre-prod` e o de menor
   superfície.
2. **Aceita — quando o host é da org e não do board.** Entrega do `secret_id` por
   canal cifrado ponto-a-ponto de uso único (ex.: `age`/`gpg` para a chave
   pública do operador da org, ou um one-time secret com TTL curto), e o operador
   da org o instala em `/etc/payment/approle-<inst>` `0640 root:payment`. Apagar o
   material do canal após a instalação. **Confirmar recepção fora do canal** (o
   `secret_id` nunca é confirmado ecoando-o de volta).

Em ambos os casos o `role_id` pode ir por canal menos sensível (ele não
autentica sozinho), mas por higiene trate o par junto e fora de banda.

## Rotação do `secret_id` (custódia conjunta ⇒ rotação combinada)

O `secret_id` é emitido **sem TTL** (`secret_id_ttl=0`) de propósito, para não
derrubar o serviço no meio da noite — a rotação é **manual** (ver "Pendências
conhecidas" em [`vault-lmhost.md`](vault-lmhost.md)). Com custódia conjunta, a
cadência e a responsabilidade precisam ser explícitas:

- **Cadência de rotina: a cada 90 dias**, e **imediatamente** em qualquer um
  destes gatilhos: saída de pessoa com acesso ao host ou ao canal de entrega;
  suspeita de vazamento do `/etc/payment/approle-<inst>`; troca de host da org;
  incidente (ver revogação). 90 dias é o piso de higiene para segredo de longa
  vida sem TTL; encurtar se a auditoria pedir.
- **Quem rotaciona:** o board (detém o Vault). A org **solicita** rotação por
  canal combinado; o board executa e re-entrega pelo canal acima.
- **Procedimento** (na `minio`, depois instalar no host e reiniciar a unit):

  ```sh
  # 1) emitir novo secret_id do MESMO AppRole (mesma policy payment/prod)
  sudo bash -c 'source /root/vault-env.sh && vault write -f -field=secret_id \
    auth/approle/role/payment-prod/secret-id'
  # 2) instalar no host, atomically, como o board já faz:
  #    grava em /etc/payment/approle-<inst> (0640 root:payment) o par role_id+secret_id
  # 3) reiniciar a unit — a materialização só acontece no start:
  #    systemctl restart payment-api        # (ou payment-api-sbx para sbx, que NÃO é este escopo)
  # 4) só depois de confirmar o boot fail-closed (abaixo), DESTRUIR o secret_id antigo:
  sudo bash -c 'source /root/vault-env.sh && vault write \
    auth/approle/role/payment-prod/secret-id-accessor/destroy \
    secret_id_accessor=<accessor-do-antigo>'
  ```

  Ordem importa: **emitir → instalar → reiniciar → confirmar → destruir o antigo**.
  Destruir antes de confirmar arrisca deixar o serviço sem credencial válida e,
  como é fail-closed, sem subir.

## Revogação em incidente

Se houver suspeita de comprometimento do `secret_id`, do host, ou do canal:

1. **Revogar o `secret_id`** imediatamente (não esperar a janela de rotação):
   `vault write auth/approle/role/payment-prod/secret-id-accessor/destroy
   secret_id_accessor=<accessor>`. Isso corta a autenticação daquele segredo; um
   boot subsequente com ele recebe **HTTP 403** e o `payment-api` **não sobe**
   (fail-closed).
2. Se a suspeita é sobre a **KEK** em si (e não só sobre o `secret_id`), o
   escopo é maior: selar o Vault e tratar como incidente de chave-mestra —
   **cross-ref** o runbook de resposta a incidente
   [`../ops/incident-response-c6.md`](../ops/incident-response-c6.md)
   (SIN-68745). Girar
   a KEK exige reseal de **todos** os cofres com a chave nova nos dois campos
   (`cmd/vault-reseal`, AAD row-binding `(tenantID, bankID)` — ver
   SIN-69369/69372); é operação de janela, não de rotina.
3. Registrar quem revogou, quando e o accessor destruído. A custódia é conjunta:
   **qualquer** das partes (board ou org) pode **pedir** revogação; a **execução**
   é do board (detém o Vault). Comunicar a contraparte fora de banda.

## Confirmação: o CD/host da org usa AppRole `payment/prod` (mesma KEK)

Aceite #2 de SIN-70400. Duas propriedades, ambas verificáveis no repo:

**(a) O CD não carrega a KEK — o binário sim, a chave não.** O workflow
[`cd-stg.yml`](../../.github/workflows/cd-stg.yml) compila `cmd/api`, envia o
binário por SSH sobre um comando forçado, reinicia a unit e faz smoke no
`/healthz`. Os **únicos** segredos que ele consome são de transporte de deploy —
`PAYMENT_STG_SSH_KEY`, `PAYMENT_STG_HOST`, `PAYMENT_STG_USER`,
`PAYMENT_STG_HOST_KEY`, `PAYMENT_STG_SMOKE_URL`. **Não há `PAYMENT_BANK_VAULT_KEY`
no env do workflow**, e o job de deploy é **owner-gated** a `pericles-luz/payment`
(o fork `ia-dev-sindireceita/payment` é no-op no CD e não deriva). A KEK entra
**só** no host, no boot, via AppRole → Vault. Grep de prova:

```sh
grep -n 'VAULT_KEY\|payment/prod\|secret_id' .github/workflows/cd-stg.yml   # sem resultado
```

**(b) Sem fallback SQLite legado em produção.** A seleção de engine vive em um só
lugar (`internal/platform/persistence`): **PostgreSQL quando `PAYMENT_DB_DSN` está
setado, SQLite caso contrário** (`cmd/api/main.go` — `persistence.Open(ctx,
cfg.DBDSN, cfg.DBPath)`). Em produção o `PAYMENT_DB_DSN` é **materializado do
Vault** apontando para o Postgres do `pre-prod`, e o boot-guard de transporte
(SIN-70355, `assertTransportSecurity`) **falha fechado** se o DSN não exigir TLS
(`sslmode`) — cobrindo primary e todos os fallbacks multi-host. Portanto o host da
org **não** cai para SQLite: sem DSN de Postgres não há produção; com DSN sem TLS
não há boot. O SQLite fica restrito a dev/test.

> **Cuidado de secure-by-default (documentado aqui porque é a razão de o
> provisionamento ser fail-closed no host, não no binário).** O **binário** Go,
> se subisse com `PAYMENT_BANK_VAULT_KEY` **ausente**, **degradaria
> silenciosamente para cofres in-memory** (log de aviso em `cmd/api/main.go`:
> "bank secret vault is IN-MEMORY … credentials/certificates do NOT survive a
> restart") — e, sem `PAYMENT_DB_DSN`, para SQLite. O fail-closed de **produção**
> vem do **wrapper de boot**, não do binário: `vault-materialize.py` **sai
> diferente de zero** se o segredo não trouxer `PAYMENT_BANK_VAULT_KEY`, então o
> serviço nem chega a rodar sem a KEK (ver [`vault-lmhost.md`](vault-lmhost.md) —
> "Fail-closed, e o que isso custa"). **Consequência para a org:** o host da org
> **tem de** subir o `payment-api` **pelo wrapper** (`payment-run.sh` via
> systemd), materializando KEK **e** DSN do Vault — nunca `go run`/binário cru com
> env parcial, que abriria a porta para o modo in-memory/SQLite sem alarme.

## Passo operador-gated (owner: board / Pericles) + verificação SecEng

**Provisionamento (executa: board / Pericles — detém o Vault do board):**

1. Confirmar que a policy `payment-prod` dá `read` **só** em `payment/*/prod`
   (checar contra o bloco HCL acima; reprova se houver `sbx`, `transit/`, NFS-e,
   `list` no mount ou curinga).
2. Emitir/entregar o AppRole à custódia da org pelo **canal fora de banda** acima
   (preferir o modelo 1 — gerar o `secret_id` direto no host, sem trânsito).
   Instalar em `/etc/payment/approle-<inst>` `0640 root:payment`.
3. Subir/reiniciar o `payment-api` da org **pelo wrapper** (`payment-run.sh` via
   systemd), que materializa KEK + DSN do Vault.

**Verificação pós-provisionamento (executa: SecEng — este é o passo que fecha o
Aceite #3):**

- **V1 — fail-closed contra a KEK compartilhada.** Confirmar que o serviço subiu
  **com** KEK: nos logs do boot, `journalctl -u payment-api | grep vault-materialize`
  **sem** `segredo sem PAYMENT_BANK_VAULT_KEY`, e a linha
  `durable encrypted-at-rest bank secret vault ENABLED` no log da app (prova de
  que o cofre é o durável cifrado, não o in-memory). Negativo obrigatório: a
  linha `bank secret vault is IN-MEMORY` **não** pode aparecer.
- **V2 — a MESMA KEK abre os cofres existentes.** Confirmar que uma credencial ou
  certificado já selado por `(tenantID, bankID)` **abre** no host da org (mesma
  KEK, mesmo Vault) — sem erro de AAD/decrypt no log. Isso prova materialização
  da chave idêntica, não de uma chave diferente que "também sobe".
- **V3 — Least Privilege efetivo.** Com o `secret_id` da org, tentar
  `vault kv get payment/sbx` deve dar **403** (a policy não alcança `sbx`).
  Prova negativa de que o escopo é `payment/prod`-only.
- **V4 — negativo do fail-closed.** (Opcional, em janela) revogar o `secret_id` e
  confirmar que o boot subsequente **não sobe** (HTTP 403 no materializer), depois
  re-emitir. Prova a reversibilidade da revogação.

Enquanto o board não executar o provisionamento, o Aceite #3 fica **pendente de
passo operador-gated** com owner nomeado (board/Pericles); a SecEng roda V1–V4 e
carimba assim que o AppRole estiver no host.

## Risco residual

- **`root_token` + unseal keys na própria `minio`** (`/root/vault-init.json`):
  quem tem root na máquina do Vault desela e lê a KEK, anulando o split de Shamir.
  Pré-existente ao handoff (ver "Pendências conhecidas" em
  [`vault-lmhost.md`](vault-lmhost.md)); custódia conjunta **não piora** isso, mas
  aumenta o número de partes que dependem daquela máquina — tirar o
  `vault-init.json` de lá continua sendo a mitigação certa e vira mais urgente
  com dois custodiantes.
- **`secret_id` sem TTL** troca resiliência (não derruba à noite) por janela de
  exposição maior — mitigado pela cadência de 90 dias + rotação por gatilho acima.
- **Dependência de boot entre máquinas** (`minio` selada ⇒ `payment` não sobe) é
  compartilhada com a custódia A1 da NFS-e no mesmo Vault; agora também com a org.
  É o custo aceito do fail-closed.
- **Host único compartilhado** (Opção A): board e org sobre o mesmo `pre-prod`
  significa que um comprometimento do host expõe as duas partes. É inerente à
  decisão A (prod compartilhada); a Opção B (prod própria da org) era a alternativa
  e foi preterida pelo CEO.
