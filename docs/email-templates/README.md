# Email templates

Source-of-truth for the transactional emails Supabase Auth sends on behalf of this deployment. Kept in-repo so we can iterate, review diffs, and roll back without leaving the dashboard as the only audit trail.

## Files

| File | Supabase template slot | When it's sent |
|---|---|---|
| `magic-link.html` | **Magic Link** | Every sign-in. Triggered by `POST /auth/v1/otp` from `login.html`. The only template actually wired into the live auth flow. |
| `invite-email.html` | **Invite user** | Admin-initiated invite via the Supabase dashboard or `khctl sync-users`. |
| `recovery-email.html` | **Reset password** | Not currently wired — kept for parity in case the password-reset path is re-introduced. |

All three share the same visual identity: light-mode-default with `prefers-color-scheme: dark` overrides, the `team-harness` mono wordmark, the orbital constellation mark in SVG, amber CTA (`#f59e0b`), and a fallback URL section for clients that don't render the button.

Each template's `<head>` contains a comment block with the subject line and a plain-text fallback. Supabase generates the plain-text variant from the HTML automatically; the comment is there if you ever need to override it manually.

## How to apply

1. **Supabase dashboard** → your project → **Authentication** → **Email Templates**.
2. Pick the template slot (Magic Link / Invite user / Reset password).
3. Paste the **subject** from the `<!-- Subject: … -->` comment at the top of the file.
4. Paste the full HTML into the **Message body** field.
5. **Save**. Send a test from the Supabase dashboard or by triggering the live flow with your own email.

Repeat for each template you want to update.

## Iteration workflow

- Edit the file in this directory.
- Render locally to sanity-check the markup. Open it in any browser, or pipe it through a tool like `htmlhint` or `mjml-validate` if you want strict email-client compatibility checks.
- Open a PR with the diff. Reviewers compare side-by-side against the previous version.
- After the PR merges, manually paste the new template into the Supabase dashboard. There is no automated sync today — Supabase doesn't expose an Email Templates API.

## Design constraints

Email clients strip a lot of modern CSS. Stick to these rules when editing:

- **Inline styles only.** External stylesheets and `<style>` blocks are inconsistent across Outlook, Gmail, Apple Mail.
- **Table-based layout.** Outlook desktop ignores most flexbox/grid. Use `<table role="presentation">` for any non-trivial alignment.
- **No `<script>`.** Stripped by every major client.
- **Solid background colors.** Transparent backgrounds can invert badly in dark-mode clients.
- **Test in dark mode.** The `prefers-color-scheme` block at the top of each template forces the dark variant when supported. Gmail's auto-dark-mode is unpredictable; do not rely on it.
- **Brand consistency.** Wordmark stays `team-harness` (lowercase mono). Mark colors stay amber (`#f59e0b`) for the hub and violet (`#a78bfa`) for the satellites — same palette as the landing page.

## Supabase template variables

Available in any template (the `{{ . }}` syntax is Supabase's Go-template forwarding):

| Variable | Meaning |
|---|---|
| `{{ .ConfirmationURL }}` | The signed callback URL the user must click. Always the primary CTA target. |
| `{{ .Email }}` | The recipient's email. Used in the outside footer (`sent to {{ .Email }} · team-harness`) to confirm the address. |
| `{{ .SiteURL }}` | Your configured Site URL (Authentication → URL Configuration). Not currently used in any template. |
| `{{ .Token }}` / `{{ .TokenHash }}` | The OTP token. Only useful if you build a "paste the code" flow instead of a clickable link. Not used. |

Full reference: https://supabase.com/docs/guides/auth/auth-email-templates
