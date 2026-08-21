# Runbook — Resposta a incidente com notificação ao C6 (C12 imediata / C13 judicial 48h)

- **Escopo:** procedimento de resposta a incidente de segurança/privacidade que
  **exige notificar o C6 Bank**, com canais, SLAs, template de notificação e
  registro na trilha de auditoria. Cobre uso abusivo/suspeito/não autorizado das
  APIs (C12) e ordem judicial de divulgação de Informação Confidencial (C13),
  amarrando com direitos do titular (C14) e confidencialidade (D15).
- **Fonte contratual:** Termo de Uso de APIs C6, cláusulas **5.v / 6.3** (C12 —
  reportar **imediatamente** uso abusivo, suspeito ou não autorizado) e **6.4**
  (C13 — ordem judicial que obrigue a divulgar Informação Confidencial → **avisar
  o C6 em até 48h** e divulgar **apenas o estritamente exigido**). Regras
  codificadas em `docs/compliance/c6-termo-apis-regras.md` ([SIN-68740](/SIN/issues/SIN-68740)).
- **Lentes:** Observability · Defense in depth · Fail securely · LGPD (minimização
  na divulgação) · Least disclosure.
- **Dono:** SecurityEngineer. **Aprovação:** CTO.

> ⚠️ **Confidencialidade do incidente.** Detalhes sensíveis de um incidente
> (payloads, PII de titular, indicadores de comprometimento, conteúdo de ordem
> judicial) **NÃO** vão para thread pública de issue, PR, chat aberto ou commit.
> Use o canal privado combinado e **escale ao CEO** para o handling confidencial.
> Na issue pública registram-se apenas: classe do incidente, timestamps, decisões
> e ponteiros para o local privado — nunca a evidência.

---

## 0. Papéis e pré-requisitos

| Papel | Responsabilidade no incidente |
|---|---|
| **SecurityEngineer** | Detecção→classificação, decisão de notificar, redige a notificação, registra a trilha. |
| **CTO** | Aprova a comunicação externa ao C6; decisor técnico de contenção. |
| **CEO** | Handling confidencial; aprova risco aceito; ponto de contato para ordem judicial/jurídico. |
| **Encarregado (DPO)** | Titular dos canais `encarregado@` (LGPD); obrigatório para C14 e componente-LGPD de C13. |

**Antes de começar** confirme que tem acesso ao canal privado de incidente e ao
registro de contatos. Se não tiver, **pare e escale ao CEO** — não improvise canal.

---

## 1. Canais oficiais C6

| Canal | Uso | Cláusula |
|---|---|---|
| `assistentespj@c6bank.com` | Suporte/operacional PJ; notificação de uso abusivo/suspeito/não autorizado (C12) quando não houver canal de segurança dedicado. | 5.v / 6.3 |
| `encarregado@c6bank.com` | Encarregado/DPO do C6: incidentes com **dados pessoais**, direitos do titular (C14), componente LGPD de ordem judicial. | 6.4 (C13-LGPD) / 7.10 (C14) |
| `homologacaoapi@c6bank.com` | Técnico/homologação de APIs; use apenas para o aspecto técnico/de integração, nunca para conteúdo sensível de incidente. | — |

> Confirme os endereços contra a versão vigente do Termo antes de enviar (contratos
> são atualizados). Se o Termo indicar um canal de segurança dedicado mais
> específico, ele **prevalece** sobre `assistentespj@`.

---

## 2. Fluxo: Detecção → Classificação → Notificação → Registro

### 2.1 Detecção (fontes de sinal)

- Alertas de anomalia: pico de erros 401/403 no webhook C6 (`/webhooks/c6/*`),
  falhas de reconcile-before-settle (`settlement.amount_mismatch` no `audit_log`),
  divergência de credencial, uso fora do horário/perfil do tenant.
- Relato de terceiro (C6, tenant, titular) de uso indevido.
- Achado de revisão/pentest com exploração ativa.

### 2.2 Classificação (decide SLA e canal)

| Classe | Exemplos | SLA de notificação C6 | Canal | Cláusula |
|---|---|---|---|---|
| **A — Abuso / uso suspeito / não autorizado** | credencial de tenant vazada/rotacionada sob suspeita; chamadas não autorizadas às APIs C6; comprometimento de token OAuth; webhook forjado explorado. | **Imediata** (assim que confirmada a suspeita — não esperar certeza total). | `assistentespj@` (+ `encarregado@` se envolver PII). | 5.v / 6.3 (C12) |
| **B — Ordem judicial p/ divulgar Info Confidencial** | intimação/ofício que obrigue a divulgar dados/credenciais/tráfego do C6. | **≤ 48h** após ciência da ordem, **antes** de divulgar quando legalmente possível; divulgar **só o estritamente exigido**. | `encarregado@` + jurídico via CEO. | 6.4 (C13) |
| **C — Incidente com dados pessoais (LGPD)** | vazamento/exposição de PII de titular (ex.: `devedor_doc`/`devedor_nome` de `pix_rec`, ver [ADR-0008](../security/adr-0008-pii-read-access-log.md)). | Imediata ao C6 (`encarregado@`); avaliar ANPD/titular pelo processo LGPD interno. | `encarregado@`. | C14 / LGPD |

Regra de ouro C12: **na dúvida entre "suspeito" e "confirmado", notifique** — o
gatilho contratual é *suspeita*, não prova. Fail securely = comunicar cedo.

### 2.3 Contenção (em paralelo à notificação — não sequencial)

- Rotacionar/revogar a credencial C6 do tenant afetado (evict imediato do cache de
  token por-tenant — ver [ADR-0003](../security/adr-0003-oauth2-token-revocation-lag.md)).
- Se webhook forjado/abusado: rotacionar a URL opaca por-tenant `/webhooks/c6/{tenantRef}`.
- Isolar o tenant (suspender) se o abuso persistir. Preservar evidência **antes**
  de mudar estado (snapshot do `audit_log`/logs no local privado).

### 2.4 Notificação (usar template §3)

- Redige SecurityEngineer, **aprova CTO** antes do envio (comunicação externa).
- Componente confidencial/jurídico: **CEO** no loop.
- **Minimização (C13/D15):** envie apenas o estritamente necessário; nunca anexe
  PII de titular ou segredo que não seja indispensável ao C6 entender/agir.

### 2.5 Registro na trilha de auditoria

Todo incidente e notificação são registrados de forma durável e atribuível:

- **Trilha da aplicação (`audit_log`, SIN-66016/66025):** onde o incidente
  corresponder a uma ação já modelada (ex. `settlement.amount_mismatch`,
  `credential.set` na rotação de contenção), a trilha durável já a captura
  server-side (operador + ação + tenant + timestamp). **Não** injetar PII nem
  conteúdo de incidente no `audit_log` (é vocabulário fechado, zero-segredo).
- **Registro do incidente (fora-de-banda, no canal privado):** timeline com
  timestamps UTC, classe, decisões, quem aprovou, para quem/quando se notificou o
  C6, e ponteiros para a evidência. Esse é o registro forense do incidente em si.
- Na issue Paperclip: apenas o resumo não-sensível + ponteiro para o privado.

---

## 3. Template de notificação ao C6

> Preencher, remover comentários `<...>`, aprovar com CTO (e CEO se confidencial),
> enviar do endereço corporativo para o canal da classe (§1). Português.

```
Para: <assistentespj@c6bank.com | encarregado@c6bank.com>
Assunto: [Sindireceita/Super Inteligente] Notificação de incidente — <Classe A/B/C> — <id-interno>

Prezados,

Nos termos do Termo de Uso de APIs (cláusula <5.v/6.3 | 6.4>), comunicamos:

1. Natureza do evento: <uso não autorizado / suspeito / ordem judicial / incidente com dados pessoais>.
2. Data/hora da ciência (UTC): <YYYY-MM-DDTHH:MM:SSZ>.
3. Escopo afetado: <tenant(s) / integração / APIs C6 envolvidas — SEM PII de titular>.
4. Situação atual: <em contenção / contido / em investigação>.
5. Medidas já adotadas: <rotação de credencial / rotação de webhook / suspensão de tenant / etc.>.
6. Informação que precisamos do C6 / ação solicitada: <ex.: revogar credencial no lado C6, confirmar bloqueio>.
7. [Classe B — ordem judicial] Divulgaremos apenas o estritamente exigido pela ordem.
   Anexamos <somente o mínimo necessário>. Prazo da ordem: <...>.

Contato: <nome> — <email corporativo> — <telefone>.
Encarregado (DPO): encarregado@<dominio> (para aspectos de dados pessoais).

Atenciosamente,
<Nome / Sindireceita — Super Inteligente>
```

**Nunca** cole no e-mail: CPF/nome de titular, segredos/credenciais, tokens,
payloads brutos, conteúdo integral da ordem judicial além do exigido.

---

## 4. C14 — Direitos do titular (amarração)

Solicitação de direito do titular (acesso, correção, eliminação/erasure,
portabilidade) que dependa de dados sob custódia/originados no C6:

- Encaminhar/coordenar com o **Encarregado** via `encarregado@c6bank.com`.
- Erasure de titular na base local: aplicar sobre a PII em repouso (`pix_rec.devedor_doc`/
  `devedor_nome`) conforme política de retenção; o log de acesso ([ADR-0008](../security/adr-0008-pii-read-access-log.md))
  usa `subject_ref` pseudônimo, então não vira segunda cópia a apagar.
- Registrar a solicitação e a resposta na trilha (sem duplicar a PII).

## 5. D15 — Confidencialidade (amarração)

- Informação Confidencial do C6 (credenciais, contratos, detalhes de integração)
  só trafega em canal autorizado e **nunca** em repositório público/PoC/screenshot.
- Divulgação compelida por lei (C13): notificar C6 ≤48h, minimizar o divulgado,
  registrar a base legal.

---

## 6. Checklist rápido (imprimir no incidente)

- [ ] Incidente detectado — timestamp UTC anotado.
- [ ] Classificado (A/B/C) → SLA e canal definidos.
- [ ] Evidência preservada **antes** da contenção (local privado).
- [ ] Contenção iniciada (rotação de credencial/webhook; suspensão se preciso).
- [ ] Notificação redigida (template §3), **aprovada pelo CTO** (CEO se confidencial).
- [ ] Notificação enviada ao canal correto dentro do SLA (imediato / ≤48h).
- [ ] Encarregado no loop se há PII/titular.
- [ ] Trilha registrada (`audit_log` + registro privado do incidente); issue com
      resumo não-sensível + ponteiro.
- [ ] Pós-incidente: causa-raiz + follow-up como issue de 1ª classe.

---

## Referências

- Termo C6 — regras codificadas: `docs/compliance/c6-termo-apis-regras.md` ([SIN-68740](/SIN/issues/SIN-68740)).
- [ADR-0003](../security/adr-0003-oauth2-token-revocation-lag.md) — revogação/rotação de token OAuth2 C6.
- [ADR-0008](../security/adr-0008-pii-read-access-log.md) — registro de acesso a dados pessoais (art.13).
- Trilha durável de auditoria — SIN-66016 / SIN-66025 (`internal/domain/audit`).
- `ingress-runbook.md`, `c6-smoke-e2e-runbook.md` — demais runbooks de ops.
