# Observabilidad — Runbook

> Guía operacional para la integración de Axiom vía OpenTelemetry en
> `context-harness-mcp`. Audience: operador o SRE que administra el deployment.

---

## Setup

### 1. Crear el dataset en Axiom

1. Ir a [app.axiom.co](https://app.axiom.co) → **Datasets** → **New dataset**.
2. Nombre: `context-harness-mcp` (exactamente este valor; es el default de `AXIOM_DATASET`).
3. Hacer click en **Create**. El segundo slot del free tier queda libre para otros proyectos.

### 2. Generar el API token con scope Ingest-only

1. **Settings** → **API tokens** → **New API token**.
2. Nombre sugerido: `context-harness-mcp-ingest`.
3. Permissions: seleccionar únicamente **Ingest** → scope `context-harness-mcp`.
4. **Nunca usar scope Query ni Manage** en el token de producción — principio de mínimo privilegio.
5. Copiar el token; no se puede ver nuevamente después de cerrar el modal.

### 3. Configurar env vars en el host de deploy

En el dashboard de tu hosting (Railway, Render, Fly, Coolify, VPS, etc.) agregar:

| Variable | Valor | Notas |
|---|---|---|
| `CH_OBSERVABILITY_ENABLED` | `true` | Master switch; `false` = no-op sin overhead |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `https://api.axiom.co` | Sin slash final |
| `AXIOM_TOKEN` | `<token-generado>` | Solo scope Ingest |
| `AXIOM_DATASET` | `context-harness-mcp` | Exactamente este nombre |
| `OTEL_SERVICE_NAME` | `context-harness-mcp` | Default; cambiar si corres múltiples instancias |
| `OTEL_RESOURCE_ATTRIBUTES` | `deployment.environment=production,service.version=<git-sha>` | El pipeline de deploy inyecta el SHA automáticamente |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` | No cambiar |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | 100% durante los primeros 60 días; bajar a `0.1` si >50% del budget en una semana |
| `OTEL_LOG_LEVEL` | `info` | Solo en sesiones debug cortas elevar a `debug` |

> **IMPORTANTE:** No setear `OTEL_SDK_DISABLED=true` en producción.
> El servidor lo setea automáticamente solo para el transporte `stdio` (kill-switch de Claude Code).

### 4. Reiniciar el contenedor

Después de agregar las variables, hacer restart del container. El servidor logueará:

```
{"time":"...","level":"INFO","msg":"observability enabled","endpoint":"https://api.axiom.co","sampler":"parentbased_traceidratio","ratio":"1.0"}
```

---

## Validation

### Verificar en Axiom UI

Una vez configurado y con al menos un request al servidor, ir a Axiom → **Datasets** → `context-harness-mcp` → **Query** y ejecutar:

```apl
// ¿Llegaron trazas?
['service.name'] == 'context-harness-mcp'
| count()
```

```apl
// Ver los últimos spans por tool MCP
['service.name'] == 'context-harness-mcp'
| where ['mcp.tool_name'] != ""
| project _time, ['mcp.tool_name'], ['mcp.tool_outcome'], duration
| sort by _time desc
| limit 20
```

```apl
// Ver logs correlacionados con trace_id
['service.name'] == 'context-harness-mcp'
| where isnotnull(['trace_id'])
| project _time, level, message, ['trace_id'], ['user.id']
| sort by _time desc
| limit 50
```

### Smoke test command

```bash
# Con servidor corriendo en localhost:7654 con observabilidad activada:
go run ./scripts/smoke/observability_smoke.go

# O con el servidor en otra URL:
MCP_URL=https://tu-host.example.com/mcp go run ./scripts/smoke/observability_smoke.go

# Con Axiom real (opcional — requiere AXIOM_TOKEN + AXIOM_DATASET):
AXIOM_TOKEN=<tok> AXIOM_DATASET=context-harness-mcp go run ./scripts/smoke/observability_smoke.go
```

El script verifica:
- Que el servidor responde en `/healthz`.
- Que una llamada a `search_nodes` se procesa.
- Que el boot guard (AC-A.8) está activo — falla si PR-F no fue mergeada.
- Que el scrubber real está registrado (PR-F merged).

---

## Troubleshooting

### La telemetría no llega a Axiom

**Síntoma:** Axiom UI muestra 0 eventos para `context-harness-mcp`.

**Causas comunes:**

1. `CH_OBSERVABILITY_ENABLED` no está en `true` → revisar env vars del container.
2. `AXIOM_TOKEN` expirado o revocado → regenerar (ver §Recovery).
3. `AXIOM_DATASET` con typo → verificar que el dataset existe en Axiom UI con ese nombre exacto.
4. `OTEL_EXPORTER_OTLP_ENDPOINT` tiene slash final o protocolo incorrecto → debe ser `https://api.axiom.co`.
5. El container tiene egress bloqueado hacia `api.axiom.co:443` → revisar firewall / VPN del hosting.

**Diagnóstico rápido:**

```bash
# Ver logs del servidor en busca de errores de export
docker logs <container> 2>&1 | grep -i "otlp\|axiom\|export\|error"

# Verificar que el servidor logueó "observability enabled"
docker logs <container> 2>&1 | grep "observability"
```

### La telemetría llega pero falta scrubbing

**Síntoma:** Spans en Axiom contienen tokens, emails o claves en texto plano.

**Causa:** PR-F (#55) no está en la rama mergeada, o `internal/observability/scrub.go` tiene un bug.

**Acción:**
1. Verificar que el commit de PR-F está en `main`: `git log --oneline | grep "PR-F"`.
2. Correr los tests del scrubber: `go test ./internal/observability/ -run TestRealScrubber -v`.
3. Si el scrubber falla, **deshabilitar observabilidad** de inmediato (`CH_OBSERVABILITY_ENABLED=false`) y abrir un issue.

### Los batches se pierden (spans llegan incompletos)

**Síntoma:** En Axiom las trazas están incompletas — algunos spans aparecen, otros no.

**Causas comunes:**

1. El proceso fue terminado (SIGKILL) sin dar tiempo al flush de 10s → usar SIGTERM; el servidor tiene un `defer shutdown(ctx)` con 10s de gracia.
2. Queue del batch processor saturado (`queue_size=2048`) — implica un volumen de tráfico muy alto. Verificar en logs: `level=WARN msg="dropping data" reason="full"`.
3. Timeout de red hacia Axiom > 10s → Axiom tiene SLA de <500ms; si hay timeouts repetidos, revisar latencia de red desde el hosting.

**Acción para el escenario de shutdown:**

Asegurarse de que el proceso maneja SIGTERM. El `docker stop` envía SIGTERM por defecto (con 10s de gracia antes de SIGKILL). Si tu plataforma envía SIGKILL directamente, el último batch puede perderse. Solución: configurar el período de gracia en el hosting para ≥15s.

### El sampling no aplica

**Síntoma:** `OTEL_TRACES_SAMPLER_ARG=0.1` seteado pero Axiom sigue recibiendo el 100% de las trazas.

**Causas:**

1. El env var no llegó al container — verificar con `docker inspect <container> | grep SAMPLER`.
2. El sampler es `parentbased_traceidratio`: si el request llega con un `traceparent` header que indica "sampled=1", el `ParentBased` lo honra y **no downsamplea**. Esto es correcto — el sampler respeta la decisión del caller.

**Verificar sampler activo:**

```bash
docker logs <container> 2>&1 | grep "sampler"
# Debe mostrar: "sampler":"parentbased_traceidratio","ratio":"0.1"
```

---

## Recovery

### Rotar el token de Axiom

Pasos para rotar el token sin downtime de observabilidad:

1. **Generar el nuevo token** en Axiom UI → Settings → API tokens → New API token (mismo scope: Ingest → `context-harness-mcp`). Anotar el nuevo valor antes de cerrar el modal.

2. **Actualizar el GitHub Secret:**
   - Ir a `github.com/<owner>/context-harness-mcp` → Settings → Secrets and variables → Actions.
   - Editar `AXIOM_TOKEN` → pegar el nuevo valor → Save.

3. **Actualizar el env var en el hosting:**
   - En el dashboard del host de deploy (Railway / Render / Fly / etc.) → Environment → editar `AXIOM_TOKEN` → save.
   - Trigger un redeploy (o el host lo hace automáticamente al guardar).

4. **Revocar el token viejo** en Axiom UI → Settings → API tokens → buscar el token anterior → Revoke.

5. **Verificar** en Axiom que los eventos siguen llegando después del redeploy.

> El orden importa: generar primero, revocar después. Si se revoca antes de que el nuevo token esté activo, habrá un gap de telemetría durante el redeploy.

---

## Kill-switch

### Desactivar observabilidad sin re-deploy

Si necesitás apagar la telemetría de urgencia (por ejemplo, sospecha de fuga de datos):

1. En el dashboard del hosting → Environment variables → cambiar `CH_OBSERVABILITY_ENABLED` de `true` a `false`.
2. Trigger restart del container (o esperar al restart automático del hosting).
3. Verificar en los logs del container que el servidor logueó:
   ```
   {"level":"INFO","msg":"observability disabled","reason":"CH_OBSERVABILITY_ENABLED not true"}
   ```
4. La telemetría se detiene inmediatamente después del restart. No quedan conexiones abiertas hacia Axiom.

**Sin restart (kill-switch en caliente):**
No hay hot-reload del SDK. El SDK se inicializa una sola vez en `Init()` durante el arranque del proceso. Para desactivar sin restart, se puede setear `OTEL_SDK_DISABLED=true` en env y hacer restart — el servidor lo leerá en el próximo arranque.

### Kill-switch duro para stdio (PR-H)

El transporte `stdio` (`-transport=stdio`) **siempre** fuerza `OTEL_SDK_DISABLED=true` independientemente de `CH_OBSERVABILITY_ENABLED`. Esto es una garantía hard: los subprocesos de Claude Code nunca envían telemetría, incluso si alguien setea ambas variables en el entorno.

---

## Budget Alert

### Configurar alarma al 70% del budget en Axiom

El free tier de Axiom incluye **500 GB/mes** de ingesta combinada. La alarma al 70% (350 GB) da margen para bajar el sampling antes de llegar al límite.

1. En Axiom UI → **Settings** → **Billing** → **Alerts**.
2. Click en **Create alert**.
3. Configurar:
   - **Alert type:** Ingestion budget
   - **Threshold:** `350` GB (70% de 500 GB)
   - **Notification:** email del operador (o webhook si tenés PagerDuty / Slack configurado)
4. Guardar.

**Si la alarma dispara:**

1. Evaluar el tráfico de la semana en Axiom UI → Datasets → `context-harness-mcp` → Overview → Ingestion chart.
2. Si el patrón es sostenido, bajar el sampling: `OTEL_TRACES_SAMPLER_ARG=0.1` en el hosting + restart.
3. Si es un spike puntual (ataque, carga inusual), puede ser suficiente esperar.
4. El free tier no cobra overage — simplemente deja de ingestar cuando llega al cap. Los spans se pierden silenciosamente en el lado de Axiom.

---

## Cutover Checklist

Lista de verificación obligatoria antes de flipar `CH_OBSERVABILITY_ENABLED=true` en producción.
Marcar cada ítem antes de proceder.

- [ ] **1. PR-F (#55) en main** → Confirmar en GitHub que el commit de PR-F está en la rama principal.
  ```bash
  git log --oneline origin/main | grep "PR-F"
  # Debe aparecer: "feat(observability): app-side scrubbing de PII y secretos"
  ```

- [ ] **2. Smoke test pasó** → Correr el smoke test contra el servidor de staging/pre-prod:
  ```bash
  go run ./scripts/smoke/observability_smoke.go
  # Debe terminar con: "=== PASS ==="
  ```

- [ ] **3. Boot guard de AC-A.8 activo** → Confirmar que los tests unitarios del boot guard pasan:
  ```bash
  go test ./internal/observability/ -run TestInit -v
  # Buscar: "PASS: TestInit_BootGuard_FailsWithNoopScrubber"
  ```

- [ ] **4. Dataset `context-harness-mcp` existe en Axiom UI** → Abrir [app.axiom.co](https://app.axiom.co) → Datasets → verificar que `context-harness-mcp` aparece en la lista.

- [ ] **5. `AXIOM_TOKEN` y `AXIOM_DATASET` configurados en GitHub Secrets + host del deploy**
  - GitHub: Settings → Secrets and variables → Actions → verificar `AXIOM_TOKEN` y `AXIOM_DATASET` presentes.
  - Host del deploy: verificar en el dashboard del hosting que ambas variables están en el environment.

- [ ] **6. Budget Alert configurada al 70%** → Verificar en Axiom UI → Settings → Billing → Alerts que existe una alerta a 350 GB.

- [ ] **7. Flip `CH_OBSERVABILITY_ENABLED=true`** → Solo después de que los 6 ítems anteriores estén marcados.
  - En el host: Environment → `CH_OBSERVABILITY_ENABLED` = `true` → save → restart.
  - Verificar en logs del container: `"msg":"observability enabled"`.
  - Verificar en Axiom UI 5 minutos después: al menos 1 evento en el dataset.
