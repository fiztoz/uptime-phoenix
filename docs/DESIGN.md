# Phoenix UI Design System — DESIGN.md

> **Agent rule:** Read this file before building any Phoenix screen. Every color, spacing value, component recipe, and motion spec here comes directly from the shipped code. Do not approximate, invent, or derive from first principles — use what is documented below.

---

## 1. Purpose & How to Use This Doc

This document is the single source of truth for the Phoenix "Premium dark console" UI system. It captures every design decision that was made when the core admin experience was redesigned — the exact token values, canonical Tailwind class strings, and component recipes as implemented in production code.

**Tokens-first rule:** Never hardcode colors (`#fff`, `rgb(…)`), raw spacing integers, or font sizes in component markup. Everything goes through the token layer (CSS custom properties exposed as Tailwind utility classes). If a token does not exist for what you need, add it to `app.css` first, then reference it.

**Dark is the default.** The `<html>` element carries `.dark` by default (added by `themeStore` on mount). All component recipes are written for dark mode. The light mode is a clean, neutral counterpart that activates when `.dark` is absent — the token system handles the switch automatically; components need no extra logic.

**Stack:** SvelteKit 5 (runes — `$state`, `$derived`, `$effect`, `$props`), Tailwind CSS v4 (`@theme` directive, `.dark` class override pattern), Lucide icons (`@lucide/svelte`), bits-ui for headless primitives, LayerCake for SVG charts.

---

## 2. Design Principles

### Premium dark console
Phoenix presents as a calm, professional monitoring console — the antithesis of flashy SaaS dashboards. Key characteristics:

- **Layered surfaces, not flat.** Every depth level has its own distinct OKLCH lightness value. Cards sit above background, elevated sits above surface. The eye reads hierarchy through surface contrast, not decoration.
- **One ember accent, used sparingly.** The primary color (the "ember" — a warm orange-red) is reserved for: the brand mark, active nav state, focus rings, primary CTA buttons, and metric values that need emphasis. It should not appear on every element.
- **Calibrated status colors.** Green (success), amber (warning), red (danger) are only used when they carry semantic meaning. They are never used for decoration.
- **8px spacing rhythm.** All padding, gap, and margin values follow multiples of 8px (Tailwind's `2`, `3`, `4`, `5`, `6`, `8`, `10`, `12` scale).
- **Real type hierarchy.** Page titles, section headings, body text, eyebrow labels, and metric numbers are visually distinct — not uniform 14px gray text at every level.
- **Tasteful motion.** Transitions are 150–200ms. Nothing bounces or spins. Hover states lift cards by 0.5px (`hover:-translate-y-0.5`). Loading indicators pulse gently.

### Anti-slop: Never do these things

| Rule | Why |
|---|---|
| No uniform `border border-gray-700` on every element | Borders should be hairlines from `--color-border` (translucent white in dark, translucent dark in light). Overriding with a gray hex breaks the adaptive system. |
| No rainbow accent colors | Only ember (`primary`), success, warning, and danger exist. No purple CTAs, no blue badges, no teal anything. |
| No flat equal-weight cards | Every card/panel needs `bg-card` surface + `border-border` hairline. Flat `bg-neutral-800` blocks are not the design. |
| No jagged angular sparklines | All sparklines use Catmull-Rom smoothing via `smoothLine()` — never raw SVG polylines. |
| No full-opacity status chips | Status pill backgrounds are always `bg-{status}/10`, borders `border-{status}/25`. Never `bg-green-500 text-white`. |
| No tight, cramped layouts | Section spacing is at minimum `space-y-6`; page padding is `px-4 py-6 md:px-6 md:py-8`. |
| No inline hex colors in class strings | Use token utilities: `text-primary`, `bg-card`, `border-border`. Never `text-[#ff4326]`. |
| No Tailwind `gray-*` or `slate-*` color scale | The design uses a custom OKLCH token set. Tailwind's built-in color scales have wrong hue and chroma for this palette. |
| No pure white surfaces in light mode | `oklch(1 0 0)` causes maximum screen glare and eye strain during extended use. Use `oklch(0.975 0.003 286)` for `surface`/`card`, never `#fff` or `oklch(1 0 0)`. |
| No `font-bold` for eyebrow labels | Eyebrow uses `font-weight: 600` via `.eyebrow` class — not `font-bold` (700). |
| No equal icon sizes everywhere | Standard inline: `h-4 w-4`. Page section icons / empty states: `h-5 w-5`. Never `h-6 w-6` in tight rows. |

---

## 3. Brand & Logo

### The Phoenix mascot mark

The brand mark is a compact baby phoenix holding a circular live-status node. Its round silhouette makes the monitoring product approachable, while the three-feather crest and flame-shaped tail retain a clear Phoenix connection without using a literal flame body. The canonical expression is alert and watchful.

**File:** `web/src/lib/components/BrandMark.svelte`

**Artwork:** `web/static/brand/phoenix-mascot.png` is the approved transparent RGBA master. `BrandMark.svelte` places it inside the frozen 32×32 SVG viewport. The green-screen PNG and its auto-traced SVG live only in `design-demos/confirm-mascot/` and must never be used by the application.

**Ember gradient** (applied to stroke via `url(#gid)` unless `mono` prop is true):
- `linearGradient` from `x1="8" y1="3"` to `x2="25" y2="29"` (userSpaceOnUse)
- Stop 0%: `#ff8a3c` (warm amber-orange)
- Stop 100%: `#ff4326` (deep ember red)

**Props:**

| Prop | Type | Default | Description |
|---|---|---|---|
| `size` | `number` | `28` | SVG width + height in px |
| `mono` | `boolean` | `false` | Render in `currentColor` instead of the ember gradient. Use in monochrome contexts (loading states, disabled states). |
| `class` | `string` | `''` | Pass-through CSS class |

**Usage rules:**
- In the sidebar brand lockup: `<BrandMark size={22} />` inside a `h-9 w-9 rounded-xl border border-border bg-elevated shadow-lg shadow-primary/20` container.
- In loading state: `<BrandMark size={22} />` + `<span class="animate-pulse-dot">Loading…</span>`.
- As favicon: use `/brand/phoenix-mascot.png`; the dynamic badge utility composes its down-state badge over this same transparent source.
- `mono={true}` for any context where the ember gradient would clash (e.g., inside a danger-toned surface).

### Wordmark

**File:** `web/src/lib/components/Wordmark.svelte`

"PHOENIX" drawn as hand-built rounded letterforms (SVG paths — no font dependency): plump round-capped strokes filled with the mascot's body gradient (`#ffa03a → #fd6a26 → #f2491c`), plus the mascot's golden three-feather crest (`#ffc14d → #fc9d2c`) above the I. Same `size`/`mono`/`class` props as `BrandMark`; `size` is the rendered height and width follows the viewBox aspect (≈4:1). Both gradients are `userSpaceOnUse` — bounding-box gradients vanish on zero-width paths like the I stem; do not change them back.

**Lockup (sidebar):**
```
<Wordmark size={30}>   ← ember letterforms
Uptime monitoring      ← text-[11px] text-muted-foreground
```
The wordmark is always accompanied by the mark in the sidebar. Never show the wordmark alone on an admin screen. On the login screen it sits centered below `<BrandMark size={72} />` at `size={46}`.

### Clearspace
The brand mark container (`h-9 w-9`) provides built-in clearspace. Do not reduce the container size below `h-8 w-8`.

---

## 4. Color System

### Architecture
Colors are defined as CSS custom properties in `web/src/app.css`:
- **`@theme` block:** Declares all tokens + the **light** palette. This is where Tailwind generates the utility classes (`bg-background`, `text-foreground`, etc.).
- **`.dark` block:** Overrides the same CSS custom properties for the **dark** palette. When `.dark` is on `<html>`, all utilities automatically resolve to the dark values.

There is no `light:` variant needed — omitting `.dark` gives light mode.

### Dark palette (primary experience)

| Token | CSS var | OKLCH value | Tailwind utility | Semantic use |
|---|---|---|---|---|
| background | `--color-background` | `oklch(0.165 0.004 286)` | `bg-background` | Page/viewport background — the deepest layer |
| foreground | `--color-foreground` | `oklch(0.97 0 0)` | `text-foreground` | Primary text on dark surfaces |
| surface | `--color-surface` | `oklch(0.205 0.005 286)` | `bg-surface` | Default component surface (inputs, dropdowns) |
| elevated | `--color-elevated` | `oklch(0.235 0.006 286)` | `bg-elevated` | Surfaces that sit above the default layer (topbar, brand mark container) |
| card | `--color-card` | `oklch(0.198 0.005 286)` | `bg-card` | Cards, panels, tables — slightly above background |
| card-foreground | `--color-card-foreground` | `oklch(0.97 0 0)` | `text-card-foreground` | Text within cards |
| muted | `--color-muted` | `oklch(0.27 0.006 286)` | `bg-muted` | Muted/subtle surface tint |
| muted-foreground | `--color-muted-foreground` | `oklch(0.71 0.012 286)` | `text-muted-foreground` | Secondary labels, metadata, placeholder text |
| faint | `--color-faint` | `oklch(0.55 0.01 286)` | `text-faint` | Eyebrow labels, table headers, de-emphasized text |
| primary | `--color-primary` | `oklch(0.68 0.19 35)` | `bg-primary` / `text-primary` | Ember accent — active states, CTAs, brand |
| primary-foreground | `--color-primary-foreground` | `oklch(0.99 0 0)` | `text-primary-foreground` | Text on `bg-primary` buttons |
| secondary | `--color-secondary` | `oklch(0.235 0.006 286)` | `bg-secondary` | Secondary button background |
| secondary-foreground | `--color-secondary-foreground` | `oklch(0.97 0 0)` | `text-secondary-foreground` | Text on secondary surfaces |
| accent | `--color-accent` | `oklch(0.255 0.006 286)` | `bg-accent` | Hover state for nav items and list rows |
| accent-foreground | `--color-accent-foreground` | `oklch(0.98 0 0)` | `text-accent-foreground` | Text color on hover/accent surface |
| border | `--color-border` | `oklch(1 0 0 / 0.09)` | `border-border` | Hairline borders — translucent white |
| input | `--color-input` | `oklch(1 0 0 / 0.13)` | `border-input` | Input field borders |
| ring | `--color-ring` | `oklch(0.68 0.19 35 / 0.5)` | `ring-ring` | Focus ring (ember at 50% opacity) |
| sidebar | `--color-sidebar` | `oklch(0.178 0.004 286)` | `bg-sidebar` | Sidebar background (slightly darker than background) |
| sidebar-foreground | `--color-sidebar-foreground` | `oklch(0.97 0 0)` | `text-sidebar-foreground` | Sidebar text |
| success | `--color-success` | `oklch(0.74 0.17 155)` | `text-success` / `bg-success` | Up/operational status |
| warning | `--color-warning` | `oklch(0.8 0.16 80)` | `text-warning` / `bg-warning` | Pending/degraded status |
| info | `--color-info` | `oklch(0.72 0.13 250)` | `text-info` / `bg-info` | Informational / scheduled state — maintenance, announcements |
| danger | `--color-danger` | `oklch(0.64 0.21 25)` | `text-danger` / `bg-danger` | Down/error status |
| destructive | `--color-destructive` | `oklch(0.64 0.21 25)` | `bg-destructive` | Destructive action button background |

### Light palette

Eye-comfort rules applied (2026-07): **no pure white surfaces** — maximum lightness is `0.985`. Pure `oklch(1 0 0)` creates maximum screen luminance that overstimulates the retina during extended use. Text uses soft dark gray (`0.19` lightness) instead of near-black (`0.21`) to reduce contrast-polarity halation — the "glow" effect where sharp black-on-white edges cause visual fatigue.

| Token | OKLCH value | Notes |
|---|---|---|
| background | `oklch(0.985 0.002 286)` | Near-white with a faint cool undertone |
| foreground | `oklch(0.19 0.008 286)` | Soft dark gray — not near-black, reduces halation |
| surface | `oklch(0.975 0.003 286)` | Off-white — no pure white glare on inputs/dropdowns |
| elevated | `oklch(0.985 0.002 286)` | Same as background in light |
| card | `oklch(0.975 0.003 286)` | Off-white cards — no pure white glare |
| muted | `oklch(0.96 0.004 286)` | Pale gray |
| muted-foreground | `oklch(0.5 0.012 286)` | Mid-gray text |
| faint | `oklch(0.62 0.01 286)` | Light gray labels |
| primary | `oklch(0.62 0.2 32)` | Ember (slightly deeper in light) |
| border | `oklch(0.22 0.01 286 / 0.1)` | Translucent dark hairline |
| input | `oklch(0.22 0.01 286 / 0.14)` | Slightly more opaque input border |
| ring | `oklch(0.62 0.2 32 / 0.45)` | Ember ring at 45% |
| sidebar | `oklch(0.99 0.002 286)` | Off-white sidebar |
| success | `oklch(0.6 0.15 150)` | Slightly deeper green for contrast on off-white |
| warning | `oklch(0.7 0.15 75)` | Amber on off-white |
| info | `oklch(0.56 0.13 250)` | Informational blue with sufficient contrast on off-white |
| danger | `oklch(0.58 0.21 25)` | Red on off-white |
| radius | `0.75rem` | Shared — not theme-specific |

### Opacity modifiers for status colors (dark)

These are the canonical tint levels used throughout the UI:

| Usage | Class pattern |
|---|---|
| Status pill background | `bg-success/10`, `bg-danger/10`, `bg-warning/10` |
| Status pill border | `border-success/25`, `border-danger/25`, `border-warning/25` |
| Active nav background | `bg-primary/10` |
| Stat tile primary glow blur | `bg-primary/10` (blurred absolute div) |
| Hover border on monitor card | `hover:border-primary/30` |
| Avatar ring | `ring-primary/25` |
| Avatar background | `bg-primary/15` |
| Reconnect banner | `bg-warning/10 border-warning/20 text-warning` |

### Selection highlight
```css
::selection {
  background-color: oklch(0.68 0.19 35 / 0.3);  /* ember at 30% */
  color: var(--color-foreground);
}
```

---

## 5. Typography

### Font stacks
```css
--font-sans: "Inter var", ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
--font-mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, monospace;
```

Inter is self-hosted at `web/static/fonts/InterVariable.woff2`, declared with `@font-face` in `web/src/app.css`, and preloaded from `web/src/app.html`. Only the Roman variable face is shipped because the application currently uses no italic typography; add and preload the italic face if italic styles are introduced.

Font feature settings applied globally: `"cv02", "cv03", "cv04", "cv11"` (Inter's character variants for cleaner numerals and disambiguation). `-webkit-font-smoothing: antialiased` + `text-rendering: optimizeLegibility` are set globally on `body`.

### Type scale

| Role | Tailwind classes | Notes |
|---|---|---|
| Page title | `text-2xl font-semibold tracking-tight` | Dashboard/page headings; ~24px |
| Section heading | `text-sm font-semibold text-muted-foreground` | "Needs attention", "All monitors" section labels |
| Card/panel heading | `text-sm font-semibold tracking-tight` | Panel headers in data tables |
| Body / list item | `text-sm` | Default for all data rows, descriptions |
| Stat metric | `text-3xl font-semibold tracking-tight tnum` | Large KPI numbers in stat tiles |
| Eyebrow label | `.eyebrow` class | See utility below |
| Metadata / subtext | `text-xs text-muted-foreground` | Timestamps, hints, secondary info |
| Table header | `text-[11px] font-semibold uppercase tracking-wider text-faint` | (equivalent to a tighter eyebrow in table context) |
| Mono metric | `font-mono text-xs text-muted-foreground` | Latency numbers, timestamps, target URLs |
| Tag/badge | `text-[11px] font-medium` | Inline tags on monitor rows |

### Custom typography utilities

**`.eyebrow`** — Section/group label above content blocks:
```css
.eyebrow {
  font-size: 0.6875rem;   /* 11px */
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--color-faint);
  font-weight: 600;
}
```
Usage: `<span class="eyebrow">Total monitors</span>`, stat tile labels, nav group dividers.

**`.tnum`** — Tabular (fixed-width) numbers for aligned metrics:
```css
.tnum {
  font-variant-numeric: tabular-nums;
}
```
Usage: any number column in a table, any metric that updates dynamically (ping values, uptime percentages).

---

## 6. Spacing, Radius, Elevation

### 8px grid

All spacing uses Tailwind's default scale at 4px steps, biased toward multiples of 8px in practice:

| Usage | Value | Tailwind |
|---|---|---|
| Card internal padding | 20px | `p-5` |
| Page horizontal padding (mobile) | 16px | `px-4` |
| Page horizontal padding (md+) | 24px | `md:px-6` |
| Page vertical padding (mobile) | 24px | `py-6` |
| Page vertical padding (md+) | 32px | `md:py-8` |
| Sidebar padding | 12px | `p-3` |
| Nav item padding | `py-2 px-3` | 8px vertical, 12px horizontal |
| Table cell padding | `px-4 py-3` | 16px / 12px |
| Gap between tiles | 16px | `gap-4` |
| Gap between page sections | `space-y-6` | 24px |
| Gap between card grid items | `space-y-3` | 12px for list; `gap-4` for grid |
| Topbar padding | `px-4 md:px-6`, `py-3` | |
| Brand lockup height | 64px | `h-16` |
| Sidebar width (expanded) | 256px | `w-64` |
| Sidebar width (collapsed) | 72px | `w-[4.5rem]` |

### Border radius

All radii trace back to `--radius: 0.75rem` (12px):

| Shape | Value | Tailwind class |
|---|---|---|
| Cards, panels, tables | 12px | `rounded-xl` |
| Nav items, buttons, inputs | 8px | `rounded-lg` |
| Brand mark container | 12px | `rounded-xl` |
| Avatar circle | full | `rounded-full` |
| Status pill | full | `rounded-full` |
| Tag / badge | 4px | `rounded` |
| Dot status indicator | full | `rounded-full` (via `.dot`) |
| Status accent bar on card | 0 (edge flush) | `absolute inset-y-0 left-0 w-[3px]` |
| Uptime bar segment | 2px | `rounded-[2px]` |

### Elevation model

Phoenix uses surface lightness + optional shadow for depth, not heavy drop shadows.

| Layer | Token | Shadow |
|---|---|---|
| Page background | `bg-background` | none |
| Sidebar | `bg-sidebar` | none (border-r separates) |
| Cards / panels | `bg-card` | none at rest; `hover:shadow-[0_18px_40px_-24px_rgba(0,0,0,0.8)]` on interactive cards |
| Elevated (topbar, brand container) | `bg-elevated` | none |
| Brand mark icon container | `bg-elevated` | `shadow-lg shadow-primary/20` |
| Mobile sidebar overlay | — | `bg-black/60 backdrop-blur-sm` |
| Dropdown / popover | — | defined by bits-ui; use `bg-elevated border-border` |

### The ambient app glow (`.app-glow`)

Applied on the outermost admin shell div (`<div class="app-glow relative flex h-dvh overflow-hidden">`). Creates a subtle ember radial glow at top-left and a cool indigo tint at top-right — a background ambient effect that gives the dark surface depth without visible UI elements.

```css
/* Dark mode glow (default) */
.app-glow::before {
  content: "";
  position: fixed;
  inset: 0;
  z-index: 0;
  pointer-events: none;
  background:
    radial-gradient(680px 360px at 0% -8%, oklch(0.68 0.19 35 / 0.08), transparent 70%),
    radial-gradient(560px 340px at 100% 0%, oklch(0.62 0.13 250 / 0.06), transparent 72%);
}
/* Light mode override */
:root:not(.dark) .app-glow::before {
  background:
    radial-gradient(680px 360px at 0% -8%, oklch(0.62 0.2 32 / 0.05), transparent 70%),
    radial-gradient(560px 340px at 100% 0%, oklch(0.62 0.13 250 / 0.05), transparent 72%);
}
```

This class is placed **only** on the admin shell. Do not apply it to nested components.

---

## 7. Motion

### Duration & easing
- **Standard transition:** `transition-colors duration-200` or `transition-all duration-200` — 200ms, Tailwind's default ease.
- **Sidebar collapse/expand:** `transition-all duration-200` on the `<aside>` width.
- **Card hover lift:** `hover:-translate-y-0.5` with `transition-all duration-200` — 2px lift, very subtle.
- **Card hover shadow:** Part of the same `transition-all` — shadow fades in at 200ms.
- **Nav items / buttons / row hovers:** `transition-colors` only (no translate).

### Pulse animation (`.animate-pulse-dot`)

Used for loading states and the reconnecting banner dot:
```css
@keyframes phx-pulse {
  0%, 100% { opacity: 1; }
  50%       { opacity: 0.45; }
}
.animate-pulse-dot {
  animation: phx-pulse 1.8s ease-in-out infinite;
}
```
Slower and gentler than Tailwind's built-in `animate-pulse` (which is 2s linear). Use `.animate-pulse-dot` for text/icon fades; use it on the status dot in reconnecting banners.

### What NOT to animate
- Do not add entrance animations to cards or rows — they load from realtime data and animate-in looks jarring.
- Do not use spring physics / bouncy easing.
- Do not animate layout properties (width/height changes should only happen for the sidebar collapse).

---

## 8. Iconography

**Library:** `@lucide/svelte` — consistent stroke-based icon set.

### Sizes
| Context | Class |
|---|---|
| Inline in text / nav items / table rows | `h-4 w-4` |
| Stat tile icons | `h-4 w-4` |
| Empty state illustrations | `h-5 w-5` (or larger for decorative use) |
| Topbar avatar LogOut icon | `h-4 w-4` |

All Lucide icons inherit `currentColor` for stroke. Do not set explicit stroke colors via inline style — use Tailwind text color utilities (`class="h-4 w-4 text-muted-foreground"`).

### Icons used in the shell
| Icon | Usage |
|---|---|
| `LayoutDashboard` | Dashboard nav |
| `Activity` | Monitors nav + monitor row icon |
| `Bell` | Notifications nav |
| `AlertTriangle` | Incidents nav + stat tile (down count) |
| `Globe` | Status Page nav |
| `CalendarClock` | Maintenance nav |
| `Settings` | Settings nav |
| `ChevronLeft` / `ChevronRight` | Sidebar collapse toggle |
| `Sun` / `Moon` | Theme toggle |
| `Menu` / `X` | Mobile sidebar open/close |
| `LogOut` | Sign out button |
| `Server` | Stat tile (total monitors) |
| `CheckCircle` | Stat tile (up count) |
| `Gauge` | Stat tile (avg response) |
| `Search` | Filter bar search input |
| `Plus` | Add monitor button |
| `Edit2` / `Trash2` | Monitor table actions |
| `ArrowUpRight` | Monitor card external link indicator |
| `ArrowRight` | "View all" link |

### Status dot indicators (`.dot`)

The `.dot` + modifier classes are the canonical way to show monitor health inline:

```css
/* Base */
.dot { display: inline-block; width: 0.5rem; height: 0.5rem; border-radius: 999px; flex: none; }

/* Modifiers */
.dot-up   { background: var(--color-success); box-shadow: 0 0 0 3px oklch(0.74 0.17 155 / 0.18); }
.dot-down { background: var(--color-danger);  box-shadow: 0 0 0 3px oklch(0.64 0.21 25 / 0.2);  }
.dot-warn { background: var(--color-warning); box-shadow: 0 0 0 3px oklch(0.8 0.16 80 / 0.18);  }
.dot-info { background: var(--color-info); box-shadow: 0 0 0 3px oklch(0.72 0.13 250 / 0.18); }
.dot-muted{ background: var(--color-muted-foreground); }
```

Usage: `<span class="dot dot-up"></span>`. The glow ring is built into the box-shadow — never add separate glow elements. Combine with `.animate-pulse-dot` on the reconnecting banner.

---

## 9. Component Recipes

### Status Pill (`StatusPill.svelte`)

**File:** `web/src/lib/components/StatusPill.svelte`

Canonical Tailwind class string (outer wrapper):
```
inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {bgClass}
```

Where `bgClass` is:
| Status | `bgClass` | dot class |
|---|---|---|
| `up` | `bg-success/10 text-success border-success/25` | `dot dot-up` |
| `down` | `bg-danger/10 text-danger border-danger/25` | `dot dot-down` |
| `pending` | `bg-warning/10 text-warning border-warning/25` | `dot dot-warn` |
| `maintenance` | `bg-info/10 text-info border-info/25` | `dot dot-info` |
| `paused` | `bg-muted/40 text-muted-foreground border-border` | `dot dot-muted` |

Usage: `<StatusPill status={monitor.status} />`. Never inline the pill — always use the component.

---

### Stat Tile (dashboard)

```svelte
<div class="group relative overflow-hidden rounded-xl border border-border bg-card p-5 transition-colors hover:border-border/80">
  <div class="flex items-center justify-between">
    <span class="eyebrow">{label}</span>
    <Icon class="h-4 w-4 {toneRingClass}" />
  </div>
  <p class="mt-3 text-3xl font-semibold tracking-tight tnum {toneValueClass}">{value}</p>
  <p class="mt-1 text-xs text-muted-foreground">{hint}</p>
  <!-- Only for tone="primary": -->
  <div class="pointer-events-none absolute -right-6 -top-6 h-20 w-20 rounded-full bg-primary/10 blur-2xl"></div>
</div>
```

Tone color mapping:
| Tone | Icon (`toneRingClass`) | Value (`toneValueClass`) |
|---|---|---|
| `default` | `text-muted-foreground` | `text-foreground` |
| `success` | `text-success` | `text-success` |
| `danger` | `text-danger` | `text-danger` |
| `primary` | `text-primary` | `text-primary` |

Grid: `grid grid-cols-2 gap-4 lg:grid-cols-4`

---

### Monitor Card (`MonitorCard.svelte`)

**File:** `web/src/lib/components/MonitorCard.svelte`

```svelte
<a href="/monitors/{id}"
   class="group relative block overflow-hidden rounded-xl border border-border bg-card p-5 transition-all duration-200 hover:-translate-y-0.5 hover:border-primary/30 hover:shadow-[0_18px_40px_-24px_rgba(0,0,0,0.8)]">

  <!-- Left status accent bar — ember when hovered, danger when down -->
  <span class="absolute inset-y-0 left-0 w-[3px] transition-opacity duration-200
    {isDown ? 'bg-danger opacity-100' : 'bg-primary opacity-0 group-hover:opacity-100'}">
  </span>

  <!-- Header row: name + StatusPill -->
  <!-- Subrow: metadata + latency (font-mono text-xs text-muted-foreground) -->
  <!-- Footer: Sparkline (w-full, h=40) -->
</a>
```

Key details:
- Card is a full `<a>` tag (navigates to monitor detail).
- The left bar is 3px wide, flush to the card edge — `absolute inset-y-0 left-0 w-[3px]`.
- Sparkline fills the card width at the bottom.
- Hover shadow: `shadow-[0_18px_40px_-24px_rgba(0,0,0,0.8)]` — deep black shadow that bleeds below card.

---

### Sparkline (`Sparkline.svelte`)

**File:** `web/src/lib/components/Sparkline.svelte`

Renders a LayerCake `<Svg>` with `<Area>` + `<Line>` layers. Color follows majority status of data points:
- `up` majority → `var(--color-success)` for both stroke and fill
- `down` majority → `var(--color-danger)` for both stroke and fill

```svelte
<Sparkline data={sparklineData} height={40} />
```

`sparklineData` shape: `Array<{ time: number (ms), value: number (ms latency), status: string }>`

The sparkline container uses `overflow: hidden` and `position: relative` — always give it a defined width (defaults to 200px but accepts `width="100%"`).

---

### Area Chart (`charts/Area.svelte`)

**File:** `web/src/lib/components/charts/Area.svelte`

Draws a smooth filled area below the line using `smoothLine()` + a vertical gradient:
```
Gradient: fill → stop-opacity 0.28 at top, stop-opacity 0 at bottom
Path: smoothLine(pts) + closing segment to baseline (y=0 of yScale)
```

Props: `fill` (CSS color, default `var(--color-success)`) and `opacity` (multiplier on the 0.28 stop, default 1).

---

### Line Chart (`charts/Line.svelte`)

**File:** `web/src/lib/components/charts/Line.svelte`

Draws a smooth SVG path with no fill:
```
stroke-linecap="round", stroke-linejoin="round"
Default stroke: var(--color-success), strokeWidth: 2
```

---

### Smooth path utility (`smooth.ts`)

**File:** `web/src/lib/utils/smooth.ts`

```typescript
smoothLine(points: Array<[number, number]>, tension = 1): string
```

Converts `[x, y]` point pairs to an SVG `M…C…` path string using Catmull-Rom → cubic-bézier conversion. This is why sparklines look premium rather than jagged. Always use this for any custom SVG chart paths — never raw `L` (lineto) commands for data series.

---

### Uptime Bar (`UptimeBar.svelte`)

**File:** `web/src/lib/components/UptimeBar.svelte`

90-day uptime visualization — 90 equal-width segments, each colored by daily status:

```svelte
<div class="flex h-9 w-full items-stretch gap-[2px]">
  {#each displayData as day}
    <div class="h-full flex-1 rounded-[2px] {getColor(day.status)}
                opacity-80 transition-all hover:opacity-100 hover:brightness-110"
         title="{day.date}: {day.status}">
    </div>
  {/each}
</div>
```

Color mapping:
| Status | Class |
|---|---|
| `up` | `bg-success` |
| `down` | `bg-danger` |
| `pending` | `bg-warning` |
| `none` (no data) | `bg-muted` |

Data shape: `Array<{ date: string, status: 'up' | 'down' | 'pending' | 'none' }>`. Sorted chronologically, padded to 90 days with `none` from the left.

---

### Primary Button

```svelte
<button class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
  <Icon class="h-4 w-4" />
  Label
</button>
```

---

### Ghost Button

```svelte
<button class="inline-flex items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground">
  <Icon class="h-4 w-4" />
  Label
</button>
```

Used for: sign out button, theme toggle, sidebar collapse toggle, table row actions.

Small icon-only variant (topbar actions):
```
grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground
```

---

### Text Input

```svelte
<input type="text"
  class="w-full rounded-lg border border-border bg-surface py-2 pl-9 pr-3 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring" />
```

With leading icon (search): wrap in `relative`, position icon with `absolute left-3 top-2.5 h-4 w-4 text-muted-foreground pointer-events-none`.

---

### Select

```svelte
<select class="rounded-lg border border-border bg-surface px-3 py-2 text-sm focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring">
```

---

### Data Table

```svelte
<!-- Wrapper — only visible on md+ -->
<div class="hidden overflow-x-auto rounded-xl border border-border bg-card md:block">
  <table class="w-full text-sm">
    <thead>
      <tr class="border-b border-border text-left">
        <th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">
          Column Name
        </th>
      </tr>
    </thead>
    <tbody>
      <tr class="border-b border-border transition-colors last:border-0 hover:bg-accent/40">
        <td class="px-4 py-3 font-medium">…</td>
      </tr>
    </tbody>
  </table>
</div>
```

On mobile, replace with card list (`space-y-3 md:hidden`) using `MonitorCard` or a simplified row card.

---

### Nav Item (sidebar)

```svelte
<a href={item.href}
   class="group relative flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors
          {active
            ? 'bg-primary/10 text-primary'
            : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground'}">

  <!-- Active indicator bar -->
  {#if active}
    <span class="absolute inset-y-2 left-0 w-0.5 rounded-full bg-primary"></span>
  {/if}

  <Icon class="h-4 w-4 shrink-0" />
  {#if !sidebarCollapsed}
    <span class="truncate">{label}</span>
  {/if}
</a>
```

Active state: `bg-primary/10 text-primary` background + `w-0.5 bg-primary` left indicator bar (2px wide, `inset-y-2` to inset 8px from top/bottom).

---

### Badge / Tag Chip

Inline tag on monitor rows:
```svelte
<span class="rounded px-1.5 py-0.5 text-[11px] font-medium"
      style="background-color: {color}22; color: {color}; border: 1px solid {color}44;">
  {tagName}
</span>
```

Where `{color}` is the tag's hex color from the API. The `22` and `44` hex suffixes provide ~13% and ~26% opacity.

Generic status badge (non-StatusPill contexts):
```
rounded-full px-2 py-0.5 text-xs font-medium bg-muted text-muted-foreground
```

---

### Attention / Health Banners

**Reconnecting banner** (shown when WebSocket is disconnected — in admin shell):
```svelte
<div class="flex items-center gap-2 border-b border-warning/20 bg-warning/10 px-4 py-2 text-sm text-warning md:px-6">
  <span class="dot dot-warn animate-pulse-dot"></span>
  Reconnecting…
</div>
```

**Needs attention section** (dashboard — list of down monitors):
```svelte
<section class="space-y-3">
  <h2 class="text-sm font-semibold text-muted-foreground">Needs attention</h2>
  <div class="overflow-hidden rounded-xl border border-border bg-card">
    <a href="/monitors/{id}"
       class="flex items-center gap-3 border-b border-border px-4 py-3 transition-colors last:border-0 hover:bg-accent/50">
      <Activity class="h-4 w-4 shrink-0 text-muted-foreground" />
      <span class="min-w-0 flex-1 truncate font-medium">{mon.name}</span>
      <span class="hidden font-mono text-xs text-muted-foreground sm:block">{mon.target}</span>
      <StatusPill status={mon.status} />
    </a>
  </div>
</section>
```

---

### Skeleton

Use `web/src/lib/components/Skeleton.svelte`. Skeleton compositions must mirror the loaded layout's geometry, including the same card wrapper, radius, border, and padding. Wrap each loading region in `role="status"` with a visually hidden loading label.

```svelte
<div class="rounded-xl border border-border bg-card p-5" role="status">
  <span class="sr-only">Loading…</span>
  <Skeleton class="h-4 w-40" />
  <Skeleton class="mt-3 h-8 w-24" />
</div>
```

### Empty State

Use `web/src/lib/components/EmptyState.svelte` for empty collections and missing resources. Pass a Lucide component through `icon`; optional actions use a snippet. Authorization checks remain in the caller, never inside `EmptyState`.

```svelte
{#snippet action()}
  {#if canManage}<button class={primaryBtn}>Create monitor</button>{/if}
{/snippet}
<EmptyState icon={Activity} title="No monitors found." description="Create your first monitor." {action} />
```

---

### Global Status Chip (topbar)

Indicates overall system health in the top bar. Color adapts to `overall` state:

```svelte
<span class="inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium
  {overall === 'up'   ? 'border-success/25 bg-success/10 text-success' :
   overall === 'down' ? 'border-danger/25 bg-danger/10 text-danger'   :
                        'border-border bg-muted/40 text-muted-foreground'}">
  <span class="dot {overall === 'up' ? 'dot-up animate-pulse-dot' :
                    overall === 'down' ? 'dot-down' : 'dot-muted'}"></span>
  {overall === 'up' ? 'All Systems Operational' :
   overall === 'down' ? 'Incident Detected' : 'No monitors'}
</span>
```

---

### Filter Bar (monitors page)

```svelte
<div class="flex flex-col gap-3 sm:flex-row">
  <!-- Search input with leading icon -->
  <div class="relative flex-1">
    <Search class="pointer-events-none absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
    <input type="text" placeholder="Search monitors…"
      class="w-full rounded-lg border border-border bg-surface py-2 pl-9 pr-3 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring" />
  </div>
  <!-- Type select -->
  <select class="rounded-lg border border-border bg-surface px-3 py-2 text-sm focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring">…</select>
  <!-- Tag select -->
  <select class="rounded-lg border border-border bg-surface px-3 py-2 text-sm focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring">…</select>
</div>
```

---

### User Avatar (topbar)

```svelte
<div class="grid h-8 w-8 place-items-center rounded-full bg-primary/15 text-xs font-semibold text-primary ring-1 ring-primary/25"
     title={username}>
  {initials}  <!-- 2-char uppercase slice of username -->
</div>
```

---

## 10. App Shell Layout

### Structure

```
<div class="app-glow relative flex h-dvh overflow-hidden">  ← root, ambient glow
  <aside class="bg-sidebar border-r border-border flex flex-col
                w-64 (collapsed: w-[4.5rem]) transition-all duration-200">
    <!-- Brand lockup: h-16 border-b border-border px-4 -->
    <!-- Nav: flex-1 overflow-y-auto p-3 space-y-1 -->
    <!-- Bottom: collapse toggle -->
  </aside>

  <div class="flex flex-1 flex-col min-w-0 overflow-hidden">
    <header class="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border
                   bg-elevated/80 backdrop-blur-lg backdrop-saturate-150 px-4 md:px-6">
      <!-- page title | spacer | global status chip | theme toggle | avatar | logout -->
    </header>

    <!-- Reconnecting banner (conditional, below header) -->

    <main class="flex-1 overflow-y-auto px-4 py-6 md:px-6 md:py-8">
      <div class="mx-auto max-w-7xl space-y-6">
        <!-- page content -->
      </div>
    </main>
  </div>
</div>
```

### Key shell details

**Sidebar** (`bg-sidebar`):
- `w-64` expanded, `w-[4.5rem]` collapsed
- On mobile: `fixed inset-y-0 left-0 z-40`, slides in with `translate-x-0` / `-translate-x-full`
- Mobile overlay: `fixed inset-0 z-30 bg-black/60 backdrop-blur-sm`
- Mobile hamburger: `fixed top-3 left-3 z-50 rounded-lg border border-border bg-surface/80 p-2 shadow-lg backdrop-blur`

**Topbar** (`<header>`):
- `sticky top-0 z-20` — stays at top during scroll
- Background: `bg-elevated/80 backdrop-blur-lg backdrop-saturate-150` — glassy frosted effect
- Height: `h-14` (56px)
- Separator: `border-b border-border`

**Content area:**
- `flex-1 overflow-y-auto` — the only scrolling container
- Inner wrapper: `mx-auto max-w-7xl space-y-6` — 1280px max width, 24px section gap
- Page padding: `px-4 py-6 md:px-6 md:py-8`

---

## 11. Layout Patterns

### Responsive tile grid (dashboard stats)
```
grid grid-cols-2 gap-4 lg:grid-cols-4
```
2 columns on mobile/tablet, 4 on large screens.

### Monitor card grid (dashboard)
```
grid gap-4 sm:grid-cols-2 xl:grid-cols-3
```

### Page section structure
```svelte
<div class="mx-auto max-w-7xl space-y-6">
  <!-- Section heading row -->
  <div class="flex items-center justify-between">
    <h1 class="text-2xl font-semibold tracking-tight">Page Title</h1>
    <PrimaryButton />
  </div>

  <!-- Stat tiles -->
  <div class="grid grid-cols-2 gap-4 lg:grid-cols-4">…</div>

  <!-- Content section with eyebrow label -->
  <section class="space-y-3">
    <h2 class="text-sm font-semibold text-muted-foreground">Section name</h2>
    <!-- content -->
  </section>
</div>
```

### Table + mobile card dual pattern (monitors page)
```
<!-- Desktop only -->
<div class="hidden md:block overflow-x-auto rounded-xl border border-border bg-card">
  <table>…</table>
</div>

<!-- Mobile only -->
<div class="space-y-3 md:hidden">
  {#each monitors as mon}
    <MonitorCard monitor={mon} />
  {/each}
</div>
```

---

## 12. Focus Ring & Scrollbar

### Focus ring (global)
```css
:focus-visible {
  outline: none;
  box-shadow: 0 0 0 2px var(--color-background), 0 0 0 4px var(--color-ring);
  border-radius: var(--radius);
}
```
A 2px background-colored gap ring, then a 4px ember ring. Never use browser default blue outline.

### Scrollbar (webkit)
```css
*::-webkit-scrollbar { width: 10px; height: 10px; }
*::-webkit-scrollbar-thumb {
  background-color: oklch(1 0 0 / 0.1);
  border-radius: 999px;
  border: 2px solid transparent;
  background-clip: content-box;
}
*::-webkit-scrollbar-thumb:hover { background-color: oklch(1 0 0 / 0.18); }
```
Firefox thin scrollbar: `scrollbar-width: thin; scrollbar-color: oklch(1 0 0 / 0.12) transparent;` (set on `*`).

---

## 13. File Map

| What | Where |
|---|---|
| All design tokens (CSS vars + Tailwind) | `web/src/app.css` |
| Phoenix mascot brand mark component | `web/src/lib/components/BrandMark.svelte` |
| Approved mascot artwork and favicon source | `web/static/brand/phoenix-mascot.png` |
| Static fallback favicon SVG | `web/static/favicon.svg` |
| Skeleton primitive | `web/src/lib/components/Skeleton.svelte` |
| Empty-state primitive | `web/src/lib/components/EmptyState.svelte` |
| Branded root error page | `web/src/routes/+error.svelte` |
| Admin shell (sidebar, topbar, layout) | `web/src/routes/(admin)/+layout.svelte` |
| Status pill component | `web/src/lib/components/StatusPill.svelte` |
| Monitor card component | `web/src/lib/components/MonitorCard.svelte` |
| Sparkline component | `web/src/lib/components/Sparkline.svelte` |
| Uptime bar component | `web/src/lib/components/UptimeBar.svelte` |
| Area chart layer (LayerCake) | `web/src/lib/components/charts/Area.svelte` |
| Line chart layer (LayerCake) | `web/src/lib/components/charts/Line.svelte` |
| Catmull-Rom smooth path utility | `web/src/lib/utils/smooth.ts` |
| Dashboard page | `web/src/routes/(admin)/dashboard/+page.svelte` |
| Monitors list page | `web/src/routes/(admin)/monitors/+page.svelte` |
| Approved design mockup | `design-demos/premium-dark.html` |
| Brand mark concept exploration | `design-demos/icons.html` |
| Favicon badge logic | `web/src/lib/utils/favicon-badge.js` |
| Theme store (manages `.dark` on `<html>`) | `web/src/lib/stores/theme.svelte.ts` |

---

## 14. Redesign Status

### Shipped (matches this design system)
- Admin shell: sidebar, topbar, app glow, nav active states, avatar, theme toggle — `+layout.svelte`
- Dashboard: stat tiles, attention banner, monitor card grid — `dashboard/+page.svelte`
- Monitors list: filter bar, dual table/card responsive layout, premium table — `monitors/+page.svelte`
- Public status page: metadata, password form, skeletons, empty state, uptime bars, service list — `(public)/[domain]/+page.svelte`
- Monitor detail / edit — `(admin)/monitors/[id]/+page.svelte`
- Incidents — `(admin)/incidents/+page.svelte` (hardcoded English remains a known, accepted i18n gap)
- Maintenance windows — `(admin)/maintenance/+page.svelte`
- Notifications — `(admin)/notifications/+page.svelte`
- Settings — `(admin)/settings/+page.svelte`
- Login / auth — `web/src/routes/login/+page.svelte`
- Monitor create/edit form — `web/src/lib/components/MonitorForm.svelte`
- Branded root 404/error experience — `web/src/routes/+error.svelte`

The premium-dark redesign wave is complete. New screens must continue using the tokens and component recipes in this document.
