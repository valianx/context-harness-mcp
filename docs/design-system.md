# context-harness — design system

Single source of truth for visuals across `callback.html`, `login.html`, email templates, and the public KG viewer. Everything below is canonical — when adding a new surface, lift these tokens rather than re-deriving.

---

## 1. Palette

Dark mode only, no toggle. All values are hex.

| Token              | Value      | Use                                                    |
| ------------------ | ---------- | ------------------------------------------------------ |
| `--ch-bg`          | `#0a0a0d`  | Page background. Near-black, never pure `#000`.        |
| `--ch-bg-panel`    | `#14141a`  | Cards, inputs. One step above page.                    |
| `--ch-bg-code`     | `#050507`  | Code blocks. One step below page.                      |
| `--ch-fg`          | `#e6e6ea`  | Primary text.                                          |
| `--ch-fg-mute`     | `#8a8a96`  | Secondary text, labels, placeholders.                  |
| `--ch-fg-dim`      | `#56565f`  | Tertiary, captions, separator labels.                  |
| `--ch-border`      | `#1f1f27`  | Hairline dividers, input borders at rest.              |
| `--ch-accent`      | `#f59e0b`  | The action accent. Amber — the **hub**.                |
| `--ch-accent-soft` | `#f59e0b22`| Accent at 13% alpha — glows, focus rings, badge bg.    |
| `--ch-signal`      | `#a78bfa`  | Iolite. Used **only** for agent-signal pulses in motif.|
| `--ch-error`       | `#ef4444`  | Error states, only.                                    |
| `--ch-success`     | `#f59e0b`  | Success uses accent (same family — no chromatic noise).|

**Why these two together:** the product is a hub serving many agents. Amber anchors the hub (warm, human-side decisions). Iolite is the autonomous signal traffic. Reserve iolite for the motif itself — buttons, focus rings, badges all stay amber.

---

## 2. Type

System stack only — no external fonts (constraint).

```css
--ch-font-sans: "Inter", -apple-system, BlinkMacSystemFont, "SF Pro Text", system-ui, sans-serif;
--ch-font-mono: "JetBrains Mono", "SF Mono", ui-monospace, "Cascadia Code", Menlo, monospace;
```

Two sizes, no more:

| Token         | Size  | Line-height | Weight | Use                       |
| ------------- | ----- | ----------- | ------ | ------------------------- |
| `--ch-text-title` | 24px  | 1.25        | 500    | Page titles, card titles. |
| `--ch-text-body`  | 14px  | 1.5         | 400    | Body, labels, button text.|

Headers in emails bump to **28px** (mobile email constraint). Code blocks override to **13px / mono**. That's it — no h3/h4/h5 invented.

---

## 3. Visual motif — agent constellation

**The single visual element that appears on all 5 surfaces.** One central hub node (amber) connected by edges to four satellite agents (foreground color), with iolite **signal pulses traveling outward** from the hub along each edge in turn. The hub also pulses softly (radius breathing).

The semantic mapping is the whole point: amber = the harness (single-tenant, your team's hub), white nodes = the agents that connect to it, violet dots = signals/queries flowing between them.

```html
<svg class="ch-mark" viewBox="0 0 96 24" role="img" aria-label="context-harness mark"
     xmlns="http://www.w3.org/2000/svg">
  <g stroke="currentColor" stroke-width="1" fill="none">
    <line x1="48" y1="12" x2="12" y2="5"/>
    <line x1="48" y1="12" x2="12" y2="19"/>
    <line x1="48" y1="12" x2="84" y2="5"/>
    <line x1="48" y1="12" x2="84" y2="19"/>
  </g>
  <g fill="currentColor">
    <circle cx="12" cy="5"  r="1.8"/><circle cx="12" cy="19" r="1.8"/>
    <circle cx="84" cy="5"  r="1.8"/><circle cx="84" cy="19" r="1.8"/>
  </g>
  <circle cx="48" cy="12" r="3.5" fill="#f59e0b">
    <animate attributeName="r" values="3.5;4.1;3.5" dur="2.4s" repeatCount="indefinite"/>
  </circle>
  <!-- 4 staggered signal pulses on the edges (animateMotion) -->
  <circle r="1.4" fill="#a78bfa" opacity="0">
    <animateMotion path="M48,12 L12,5" dur="2.4s" repeatCount="indefinite" begin="0s"/>
    <animate attributeName="opacity" values="0;1;1;0" keyTimes="0;0.1;0.85;1"
             dur="2.4s" repeatCount="indefinite" begin="0s"/>
  </circle>
  <!-- + 3 more, begins at 0.6 / 1.2 / 1.8s -->
</svg>
```

**Variants applied across surfaces (same atoms, different density):**
- **Auth pages (callback/login):** mark sits top-left, ~96×24px. The background carries a **swarm lattice** — 3 hub+satellite constellations connected by dashed inter-hub lines, at 6% opacity. Implies a network of teams.
- **Emails:** mark only, centered in header, 120×30px. Static (no SMIL — most email clients drop animations). Two violet dots sit on the edges to hint at signal traffic at rest.
- **Viewer:** mark in top-left header (animated). Empty state uses a single zoomed constellation — same five nodes, two static signal dots.

**Companion wordmark** in JetBrains Mono, always together with the mark:

```
[hub] context-harness
```

The brackets stay literal type — never substitute with a graphic.

**On SMIL:** we use `<animateMotion>` because it's compact, declarative, and supported across all current browsers. The lattice background is static SVG (no animation) to keep paint cost minimal.

---

## 4. Components (8)

Each below has the CSS class and the behavioral signature. All classes are namespaced `.ch-*` so they survive being dropped into another product.

### 4.1 `.ch-mark` — brand mark
**What.** SVG node-graph as defined in §3. `currentColor` ties non-accent nodes to surrounding text; accent node is hardcoded amber.
**Where used.** Header of all 5 surfaces.

### 4.2 `.ch-btn-primary` / `.ch-btn-secondary` — buttons
**What.** 36px tall, 16px horiz padding, 6px radius, 14px sans, 500 weight.
- Primary: `bg: --ch-accent`, `color: #0a0a0d`. Hover: `box-shadow: 0 0 0 4px --ch-accent-soft`.
- Secondary: `bg: transparent`, `border: 1px solid --ch-accent`, `color: --ch-accent`. Hover: `bg: --ch-accent-soft`.
- All transitions: `150ms ease`.
- `:focus-visible` adds a 2px accent outline.

### 4.3 `.ch-input` — text field
**What.** `bg: --ch-bg-panel`, `color: --ch-fg`, `border: 1px solid --ch-border`, 6px radius, 10px×12px padding, 14px sans. Placeholder `--ch-fg-mute`.
**Focus:** `border-bottom: 1px solid --ch-accent` (the other three borders stay neutral). No full focus ring — keep it subtle. Label is `--ch-fg-mute`, 12px, mono, uppercase letterspaced.

### 4.4 `.ch-code` — code/snippet block
**What.** `bg: --ch-bg-code`, 8px radius, 16px padding, 13px mono. `Copy` button top-right (icon-only, clipboard SVG), text-only — no fill, no border. Click → copy + toast.

### 4.5 `.ch-card` — content card
**What.** `bg: --ch-bg-panel`, 8px radius, 1px border `--ch-border`, 20px padding. Hover (when interactive): border becomes `--ch-accent` at 40% alpha. Used by the viewer for nodes.

### 4.6 `.ch-badge` — pill badge
**What.** Mono 11px, uppercase, 4px×8px padding, 4px radius, `bg: --ch-accent-soft`, `color: --ch-accent`. Used for `nodeType` in viewer.

### 4.7 `.ch-toast` — ephemeral notification
**What.** Bottom-center, 32px from bottom. `bg: --ch-bg-panel`, border `--ch-border`, 6px radius, 8px×14px padding, 13px sans. Slide-up + fade-in 200ms; auto-fade after 1500ms.

### 4.8 `.ch-state-*` — entrance states
**What.** Three modifier classes — `.ch-state-loading`, `.ch-state-error`, `.ch-state-success` — that share a single entry animation: `opacity 0→1` + `translateY 6px→0` over 200ms ease-out. Loading uses a 12px square spinner (CSS-only, accent-colored). Error icons are `--ch-error`; success icons reuse the accent (per §1).

---

## 5. Motion

| Trigger                     | Duration | Easing      |
| --------------------------- | -------- | ----------- |
| Page entrance               | 200ms    | ease-out    |
| Hover / focus               | 150ms    | ease        |
| State transitions (forms)   | 200ms    | ease-out    |
| Accent node pulse (mark)    | 2400ms   | linear loop |
| Toast in/out                | 200ms    | ease-out    |

Nothing else animates. Resist.

---

## 6. Accessibility (non-negotiable)

- All text ≥4.5:1 contrast against its background. Amber-on-black is 7.2:1 — we're fine.
- Every `:focus-visible` has a visible outline (2px accent).
- Labels with `for=`/`id=` on all inputs; `aria-label` on icon-only buttons.
- Tab order matches visual order.
- SVGs decorative → `aria-hidden="true"`; SVGs that carry meaning → `role="img"` + `aria-label`.
- Reduced motion: `@media (prefers-reduced-motion: reduce)` kills the node pulse and entrance animation.

---

## 7. File budget

| Surface              | Budget | Strategy                                 |
| -------------------- | ------ | ---------------------------------------- |
| `callback.html`      | <15KB  | Single file, all inline.                 |
| `login.html`         | <10KB  | Single file.                             |
| `invite-email.html`  | <40KB  | Tables, inline CSS, no external assets.  |
| `recovery-email.html`| <40KB  | Same as invite — clone, swap copy.       |
| `viewer.html`        | <40KB  | Single file (justifies more state).      |

All surfaces include the design-system tokens inline (it's ~1.2KB minified). Duplicated by design — these files ship via `go:embed` and shouldn't depend on each other.
