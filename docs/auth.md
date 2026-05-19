# Autenticación — context-harness-mcp

> Runbook de administración, flujo de desarrollador, configuración del webhook, revocación y FAQ.
>
> **Referencias cruzadas:** `session-docs/v0.2.0-team-features/01-architecture.md` (diseño completo),
> `docs/knowledge.md` (decisiones y restricciones durables), `CLAUDE.md §3/§4/§5` (stack y convenciones).

---

## (a) Admin Runbook — Configuración inicial de Supabase Auth

Esta sección cubre los pasos que el operador ejecuta una sola vez al deployar una nueva instancia de `context-harness-mcp` con auth habilitada.

### Prerrequisitos

- Proyecto Supabase activo con la DB de `context-harness-mcp` conectada.
- Acceso al dashboard de Supabase como `Owner` o `Admin`.
- Variables de entorno disponibles (ver §G completo en `session-docs/v0.2.0-team-features/01-architecture.md`):

| Variable | Requerida | Descripción |
|---|---|---|
| `MCP_AUTH` | sí | `none` o `enabled`. Garbage value → falla en boot. |
| `MCP_JWT_SECRET` | cuando `enabled` | Hex-encoded 32+ bytes. Firmar y verificar JWTs MCP. |
| `MCP_JWT_ISSUER` | no | Default: `context-harness-mcp`. |
| `MCP_JWT_EXPIRY` | no | Default: `8760h` (1 año). |
| `MCP_STDIO_RATE_LIMIT` | no | Default: `1000` writes/s para stdio. |
| `MCP_PUBLIC_URL` | recomendado | URL base pública del server, ej. `https://mcp.ejemplo.com`. Usado en `auth_login_url` de error responses. Fallback al header `Host:`. |
| `SUPABASE_PROJECT_URL` | cuando `enabled` | `https://<ref>.supabase.co`. Para `GET /auth/v1/user` y callback HTML. |
| `SUPABASE_ANON_KEY` | cuando `enabled` | Anon JWT de Supabase. Diseñada para ser pública (embedded en callback.html). |
| `MCP_WEBHOOK_SECRET` | cuando `enabled` | Hex 32+ chars. Comparada contra header `X-Webhook-Secret` con `hmac.Equal`. |
| `SUPABASE_SERVICE_ROLE_KEY` | solo en cron | Solo para `khctl sync-users` (GH Action). El server NO la necesita. |

### Paso 1 — Habilitar Supabase Auth en el proyecto

1. Navegar a **Authentication → Providers** en el dashboard de Supabase.
2. Verificar que el provider **Email** esté habilitado y que "Enable email confirmations" esté activado.
3. En **Authentication → URL Configuration**:
   - Agregar `<MCP_PUBLIC_URL>/auth/callback` a la lista de **Redirect URLs** permitidas.
   - Ej.: `https://mcp.ejemplo.com/auth/callback`.
4. En **Authentication → SMTP Settings**: configurar el proveedor de email para que los invites y los recovery links lleguen a los usuarios.
   - Si no se configura SMTP, Supabase usará el relay interno (límite 2 emails/hora en proyectos gratuitos).

> **Importante:** `/auth/callback` debe estar en la whitelist de Redirect URLs o Supabase rechazará el redirect tras el click del magic link.

### Paso 2 — Invitar usuarios (proceso continuo)

El admin invita a los desarrolladores desde el dashboard:

1. Navegar a **Authentication → Users** → botón **Invite User**.
2. Ingresar el email del desarrollador.
3. Supabase envía el invite email automáticamente (si el SMTP está configurado).
4. El desarrollador sigue el flujo descrito en la sección **(b) Dev Flow**.

> El server MCP **NO necesita `SUPABASE_SERVICE_ROLE_KEY`** para el flujo de invitación. Solo el cron `khctl sync-users` lo requiere.

### Paso 3 — Configurar el Database Webhook (revocación)

Ver sección **(c) Configuración del Webhook** para los pasos detallados.

### Paso 4 — GitHub Actions secrets

Para el cron de sincronización de 6 horas (`khctl sync-users`), configurar los siguientes secrets en el repositorio de GitHub:

| Secret | Valor |
|---|---|
| `DATABASE_URL` | Connection string de Postgres de Supabase (mismo que el server). |
| `SUPABASE_SERVICE_ROLE_KEY` | Service role JWT del proyecto Supabase (en **Project Settings → API**). |

El workflow `.github/workflows/users_sync.yml` corre cada 6 horas y llama:

```
khctl sync-users --dsn "$DATABASE_URL" --supabase-service-role-key "$SUPABASE_SERVICE_ROLE_KEY"
```

Este cron es el **fallback** de revocación si el webhook de Supabase falla (ver §K Risk R2 en `session-docs/v0.2.0-team-features/01-architecture.md`).

---

## (b) Dev Flow — Cómo un desarrollador obtiene su token MCP

El flujo completo desde la invitación hasta el primer uso de Claude Code:

```
Admin invita email en dashboard de Supabase
   ↓
Desarrollador recibe email "You're invited"
   ↓
Click en el link → browser navega a /auth/callback#access_token=...
   ↓
Página HTML (embedded en el server) parsea el fragment
   ↓
Formulario pide nueva contraseña → PUT /auth/v1/user en Supabase
   ↓
JS hace POST /auth/exchange con {access_token}
   ↓
Server valida con Supabase, upserta en users, emite JWT MCP 1 año
   ↓
Response 200: {token, expires_at, snippet}
   ↓
Dev pega el snippet en ~/.claude.json
   ↓
Reinicia Claude Code → ready
```

### Pasos detallados

1. **Recibir el invite**: El admin te invitó; revisá el email "You're invited to context-harness-mcp" (o similar según la config SMTP del proyecto Supabase).

2. **Click en el link**: El link lleva al `/auth/callback` del MCP server. La página se carga con un formulario de nueva contraseña.

3. **Setear contraseña**: Ingresá tu nueva contraseña y hacé click en "Activar cuenta". La página llama automáticamente a Supabase para setear la contraseña y luego a `/auth/exchange`.

4. **Copiar el snippet**: La página muestra un bloque JSON similar a:
   ```json
   {
     "mcpServers": {
       "context-harness": {
         "transport": {
           "type": "http",
           "url": "https://<server-host>/mcp",
           "headers": {
             "Authorization": "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
           }
         }
       }
     }
   }
   ```
   Copiar el bloque completo.

5. **Pegar en `~/.claude.json`**: Abrir `~/.claude.json` (o crearlo si no existe) y pegar el bloque bajo la clave `mcpServers`. Si ya tenés otros servers, mergeá el objeto `context-harness` dentro del existente `mcpServers`.

6. **Reiniciar Claude Code**: Cerrar y volver a abrir Claude Code. Claude Code leerá el nuevo header y lo enviará en cada request a `/mcp`.

7. **Verificar**: En Claude Code, intentá una operación de lectura (`read_graph` o similar). Si recibís resultado, el auth round-trip fue exitoso.

> **JWT válido por 1 año.** El token no necesita renovación hasta el vencimiento (o revocación por el admin). Cuando venza, repetí el flujo desde `/auth/login`.

---

## (c) Configuración del Webhook

### Por qué Database Webhooks (no Auth Hooks)

Supabase Auth Hooks solo cubren eventos de interceptación durante el flujo de login (`CustomAccessToken`, `SendEmail`, etc.). **No existen eventos `user.deleted` ni `user.banned` en Auth Hooks.** Para reaccionar a bans y deletes, se usa un **Database Webhook** sobre la tabla `auth.users`.

> Referencia: `session-docs/v0.2.0-team-features/00-research.md §2.5` — validación de este hallazgo.

### Pasos para configurar el Database Webhook

1. En el dashboard de Supabase, navegar a **Database → Webhooks**.

2. Hacer click en **Create a new hook**.

3. Completar el formulario:
   - **Name**: `mcp-user-revocation` (o el nombre que prefieran).
   - **Table**: `auth.users` (schema `auth`, tabla `users`).
   - **Events**: tildar `DELETE` y `UPDATE` (solo estos dos — INSERT no es necesario).
   - **Type**: `HTTP Request`.
   - **URL**: `https://<MCP_PUBLIC_URL>/auth/webhook`
     - Ejemplo: `https://mcp.ejemplo.com/auth/webhook`
   - **HTTP Method**: `POST`.
   - **Headers**: agregar un header personalizado:
     - **Name**: `X-Webhook-Secret`
     - **Value**: el valor de `MCP_WEBHOOK_SECRET` (mismo secret del env del server).

4. Hacer click en **Confirm** para guardar.

5. **Verificar inmediatamente** que el header `X-Webhook-Secret` quedó seteado (ver gotcha abajo).

> **Nota sobre HMAC:** Supabase Database Webhooks no implementan firma HMAC nativa. La verificación se hace comparando el header `X-Webhook-Secret` contra `MCP_WEBHOOK_SECRET` con `hmac.Equal` (constant-time) en el server.

### GOTCHA CRÍTICO: Bug #38848 — Header `X-Webhook-Secret` se borra al editar

**Supabase bug [#38848](https://github.com/supabase/supabase/issues/38848):** cuando se edita un Database Webhook existente desde el dashboard (cambiar la URL, los eventos, etc.), Supabase silently dropea el header `X-Webhook-Secret`. El webhook queda sin autenticación y el server empieza a rechazar todos los POSTs con `401 auth/webhook-invalid-signature`.

**Regla de operación:** después de CADA edición del Database Webhook en el dashboard, re-verificar que el header `X-Webhook-Secret` sigue configurado. Si no aparece, volver a agregarlo antes de guardar.

> Esta es la razón por la que se usa `X-Webhook-Secret` (header custom) en lugar de `Authorization` — ambos tienen el mismo bug, pero `X-Webhook-Secret` hace más evidente que es un secret de webhook y no un token de autenticación estándar.

### Cómo verificar que el webhook está funcionando

Después de configurar, banear un usuario de prueba desde **Authentication → Users** y verificar:

1. En los logs del MCP server (`docker compose logs`, o los logs que exponga tu plataforma de hosting), debe aparecer una entrada con `auth_webhook_received`.
2. En la DB, `SELECT revoked_at FROM users WHERE email = '<email-del-test>'` debe retornar un timestamp.

Si no aparece nada en los logs en 30 segundos:
- Verificar que la URL del webhook es correcta y que el server está corriendo.
- Verificar que `X-Webhook-Secret` está configurado en el webhook.
- Revisar los logs de `pg_net` en Supabase: `SELECT * FROM net._http_response ORDER BY created DESC LIMIT 10;`

---

## (d) Runbook de Revocación y Rotación Nuclear

### Revocación de un usuario (caminos primario y fallback)

**Camino primario — via Supabase dashboard (efectivo en ≤2s):**

1. En **Authentication → Users**, buscar el usuario a revocar.
2. Hacer click en **Ban User** (o **Delete User** para eliminación permanente).
3. Supabase dispara el Database Webhook inmediatamente.
4. El server recibe el webhook (`POST /auth/webhook`), actualiza `users.revoked_at = now()`, e invalida la entrada en el revocation cache vía `Invalidate(sub)`.
5. El próximo request del usuario revocado a `/mcp` recibe `HTTP 403 auth/revoked`.

**Latencia end-to-end con webhook funcionando:** ≤2 segundos desde el ban hasta que la próxima request recibe 403.

**Camino fallback — sin webhook (cron 6h):**

Si el webhook está caído, mal configurado, o se perdió el evento (por ejemplo, cold start del container host durante el firing del webhook):

- El cron `khctl sync-users` corre cada 6 horas.
- Consulta la Supabase Admin API y reconcilia `users.revoked_at` contra `banned_until` / `deleted_at` de `auth.users`.
- **Worst case sin webhook:** ≤6 horas del cron + ≤1 hora de cache TTL ≈ **~7 horas** hasta revocación efectiva.

Este trade-off es aceptado para equipos internos de 5-15 personas (ver `session-docs/v0.2.0-team-features/01-architecture.md §K` Risk R2, R4, R13).

**Revocación de emergencia inmediata:** si el webhook está caído Y necesitás revocar inmediatamente, la única opción es la rotación nuclear del `MCP_JWT_SECRET` (ver sección siguiente).

### Rotación Nuclear de `MCP_JWT_SECRET`

> **Advertencia:** la rotación invalida TODOS los tokens MCP existentes. Todos los usuarios deben re-autenticarse vía magic link. Usar solo cuando sea necesario (rotación anual planificada o compromiso del secret).

**Procedimiento:**

**1. Anunciar con 24 horas de anticipación (rotación planificada):**

```
Asunto: [context-harness-mcp] JWT Secret rotation el {fecha}

El {fecha} a las {hora} rotaremos el MCP JWT Secret.
Todos los tokens MCP van a invalidarse. Tendrán que re-loginear en:
  https://<MCP_PUBLIC_URL>/auth/login

El proceso toma ~2 minutos. Guarden el snippet nuevo que genera la página.
```

Para **rotación de emergencia** (secret comprometido): saltar el aviso y ejecutar inmediatamente. Notificar después.

**2. Generar el nuevo secret:**

```sh
openssl rand -hex 32
```

Guardar el output — es el nuevo `MCP_JWT_SECRET`. No lo logueen, no lo peguen en Slack, no lo commiten al repo.

**3. Actualizar la variable de entorno en el hosting:**

Actualizar `MCP_JWT_SECRET` en la sección de env vars / secrets de tu plataforma de hosting (Railway → Variables, Render → Environment, Fly → `fly secrets set`, Coolify → Environment, server propio → archivo `.env` o systemd unit, etc.).

**4. Hacer deploy (trigger):**

```sh
# Via el deploy hook que use tu plataforma, manual deploy desde el dashboard,
# o empujar un commit vacío para triggear CI/CD:
git commit --allow-empty -m "chore: trigger deploy for JWT secret rotation"
git push
```

**5. Monitorear:**

- Observar el endpoint `/auth/login` en los logs — debería haber un pico de tráfico en los próximos 30-60 minutos mientras los usuarios re-loguean.
- Si algún usuario no aparece en ~2 horas, contactarlos directamente.

**6. Post-mortem (opcional para rotación anual):**

- Anotar en el canal del equipo: fecha de rotación, próxima fecha planificada, users que re-logearon vs total.
- Si fue una rotación de emergencia, documentar el motivo del compromiso.

**SIN overlap window:** `MCP_JWT_SECRET` es un slot único. Los tokens firmados con el secret anterior quedan **inválidos al instante** del deploy. No existe `MCP_JWT_SECRET_OLD`. Este es el trade-off elegido (simplicidad vs zero-downtime rotation) — ver `session-docs/v0.2.0-team-features/00-state.md §Locked Decisions`.

---

## (e) FAQ y Troubleshooting

### ¿Cómo detecta el callback si es PKCE o implicit?

El callback HTML tiene lógica dual:

- **Implicit flow (primario):** Supabase redirige con `#access_token=...&type=...` en el URL fragment. El JS lee `window.location.hash` y extrae los parámetros.
- **PKCE flow (fallback):** Supabase redirige con `?code=...` como query param. El JS detecta la presencia de `?code=` en la URL y llama `POST /auth/v1/token?grant_type=pkce` para canjearlo.

La página maneja ambos automáticamente. Si el proyecto Supabase está configurado para PKCE (default en proyectos nuevos 2026), el callback lo detecta y rutea al camino correcto.

### ¿Qué significa `MCP_AUTH=none`?

Con `MCP_AUTH=none` (el default), el middleware de auth es un no-op: todos los requests a `/mcp` pasan sin verificación de bearer. Las columnas `created_by_user_id` y `created_by_email` se persisten como `NULL`.

> **Advertencia:** no usar `MCP_AUTH=none` en producción si el servidor es accesible desde internet. El server emite un log warning cada 1 hora cuando detecta `MCP_AUTH=none` + transporte HTTP + DSN remoto (no localhost). El warning no bloquea el boot — es back-compat para un release de transición.

Para habilitar auth: setear `MCP_AUTH=enabled` + `MCP_JWT_SECRET` + `SUPABASE_PROJECT_URL` + `SUPABASE_ANON_KEY` + `MCP_WEBHOOK_SECRET` en el entorno del server.

### ¿Por qué el cache de revocación tiene TTL de 1 hora?

Trade-off elegido:

- **Beneficio:** órdenes de magnitud menos queries a la DB en el happy path (tokens válidos). Con TTL 1h, la mayoría de requests sobre un token válido no hacen query a DB.
- **Costo:** sin webhook, la revocación puede tardar hasta 1h (antes de que el cache expire y la próxima query a DB detecte `revoked_at`).
- **Mitigación:** el webhook handler llama `Invalidate(sub)` después de actualizar `users.revoked_at`, reduciendo la latencia efectiva de revocación a ~1s cuando el webhook funciona.

El TTL de 1h era 30s en el diseño inicial y fue subido a 1h por decisión del usuario (ver `session-docs/v0.2.0-team-features/00-state.md §Locked Decisions`).

### Troubleshooting de error codes

| Código | HTTP | Cuándo ocurre | Remediación |
|---|---|---|---|
| `auth/unauthenticated` | 401 | Falta el header `Authorization: Bearer ...` o tiene el formato incorrecto. | El dev no configuró el snippet en `~/.claude.json`, o lo configuró mal. Re-pegar el snippet desde `/auth/login`. |
| `auth/invalid-token` | 401 | Firma JWT incorrecta, `alg` distinto a HS256, `iss` no matchea, o el token está mal formado. | Token corrupto o generado con un secret diferente. Re-loginear en `/auth/login`. |
| `auth/expired` | 401 | El JWT expiró (`exp < now()`). Con expiry de 1 año, esto ocurre ~1 año después del login. | Re-loginear en `/auth/login` para obtener un token nuevo. |
| `auth/revoked` | 403 | El usuario fue baneado o eliminado de Supabase. `users.revoked_at IS NOT NULL`. | Contactar al admin. Si fue en error, el admin puede des-banear en Supabase dashboard — el webhook o el cron sincronizarán. |
| `auth/invalid-supabase-token` | 401 | Durante `/auth/exchange`, el access_token de Supabase fue rechazado por `GET /auth/v1/user`. | El access_token expiró antes del exchange (< 1 hora de vida). Reiniciar el flujo desde `/auth/login`. |
| `auth/email-not-confirmed` | 403 | El email del usuario en Supabase no está confirmado. | El usuario no confirmó el email antes de intentar el exchange. Verificar el inbox y completar la confirmación. |
| `auth/exchange-malformed` | 400 | El body del POST a `/auth/exchange` no tiene el campo `access_token` o el JSON está malformado. | Error del cliente HTML — debería ser automático. Si ocurre manualmente, verificar el shape del request. |
| `auth/jwt-issuance-failed` | 500 | Error interno al emitir el MCP JWT (`IssueMCPToken` falló). La transacción fue rolled back — no quedó row en `users`. | Error del server. Revisar logs del server. Reintentar el flujo de login. Si persiste, verificar `MCP_JWT_SECRET`. |
| `auth/webhook-invalid-signature` | 401 | El header `X-Webhook-Secret` del webhook no coincide con `MCP_WEBHOOK_SECRET`. | Verificar que el header está correctamente configurado en el Dashboard de Supabase (ver bug #38848 en sección (c)). |

### ¿El viewer necesita auth?

No. `/viewer/*` permanece público read-only por decisión de diseño (v0.1, sin cambio en v0.2.0). No se envía cookie, no se verifica bearer. El viewer solo expone operaciones de lectura del KG.

### ¿Qué pasa si el webhook no llega (Supabase cold start, red, etc.)?

El cron `khctl sync-users` corre cada 6 horas como fallback. Worst case de revocación sin webhook: ~7 horas (6h cron + 1h cache TTL). Este trade-off es aceptado para equipos pequeños internos.

Si necesitás revocación inmediata y el webhook está caído: usar la rotación nuclear del `MCP_JWT_SECRET` (sección (d)).

### ¿Cómo saber si el webhook está funcionando?

```sql
-- En Supabase SQL Editor: ver logs de pg_net
SELECT id, method, url, status_code, error_msg, created
FROM net._http_response
ORDER BY created DESC
LIMIT 20;
```

Si `status_code` es `200` para POST a `<MCP_PUBLIC_URL>/auth/webhook`, el webhook está llegando. Si hay `error_msg` o status 4xx/5xx, investigar la URL y el header `X-Webhook-Secret`.

En el server, el metric counter `auth_webhook_received_total` (expvar, accesible en `/debug/vars` si `MCP_EXPOSE_EXPVAR=1`) muestra el conteo total de webhooks recibidos desde el último restart.
