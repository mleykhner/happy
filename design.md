# Design — Happy

Shared design system for the gateway panel. Existing routes, component ownership and API behaviour remain the implementation boundary.

## Audience and intent
Home-server enthusiasts. The overview leads with actual connection state and exit selection. Data, addresses and protocol names are real service values. Never imply verified Internet reachability from a running process alone.

## Genre and theme
Modern-minimal, technical application variant. **Happy Terminal** preserves the existing dark green brand and uses a sharper phosphor-lime accent. It is a functional console with a little personality, not a simulated terminal or marketing page.

## Macrostructure family
App: Workbench adapted to real controls. Connection and server selection lead; traffic, subscription information, client counts and events follow. Other tabs retain subscriptions, clients and settings. No marketing or content-page family is needed.

Navigation: labelled side rail on desktop; a two-by-two tab grid at narrow widths and a four-item row on tablets. No numbered navigation or hidden primary actions. Footer: compact inline service label.

## Typography
Local, OFL-licensed Manrope 400–700 for body and controls; IBM Plex Mono 400–600 for display, technical values and ASCII. Cyrillic coverage included. Fonts are served with the app, never fetched from an external CDN at runtime. Headings are upright. Two-family ceiling. Token definitions live in tokens.css.

## Colour and spacing
OKLCH tokens in tokens.css. Lime indicates the active route and primary actions; blue distinguishes upload and keyboard focus; red is reserved for errors and destructive actions. Tinted borders and surfaces remain subordinate to the data. A named 4px spacing scale governs form groups, page padding and controls.

## Controls
44px minimum touch targets and field heights; main connection action 52px. Visible labels, helper text, stable borders, immediate focus rings, readable native selects, grouped fieldsets. Toggles remain native checkbox semantics with a full 44px focusable box. Read-only service-managed settings use dashed borders and explanation. Async checks expose aria-busy and inline loading. Error and success messages remain textual and accessible.

## Motion
Only opacity and transform. Button press: 1px, 120ms. Toggle thumb: 220ms. Route ASCII runs a short two-pass packet animation on connection-state changes; pending operations have a functional loader. No animation is restarted by periodic data refresh. No scroll reveals, parallax, blinking backdrop or layout animation. Reduced-motion disables spatial and repeating motion. ASCII is aria-hidden because nearby text supplies the actual state.

## Responsive layout
Mobile-first at 320 / 375 / 414 / 768px, plus 1440px desktop. Connection precedes servers on mobile; two columns start at 64rem. Forms collapse to one column, actions wrap as whole buttons, technical values wrap anywhere. No horizontal scrolling or two-line button labels. Root overflow uses clip, never hidden. Browser verification lives outside production files; evidence summary recorded separately in .hallmark/verification.json.

## Page allowances
Shared palette, control geometry, font roles, focus treatment and spacing across all tabs. Function carries the interface: no external illustrations, fake browser frames, decorative charts or invented metrics. ASCII may be used only for actual route/loading/empty states.

## Exports

### tokens.css
```css
/* Hallmark · macrostructure: Workbench · genre: modern-minimal · design-system: design.md · designed-as-app
 * theme: Happy Terminal · audience: home-server enthusiasts · intent: connection and exit selection
 * pre-emit critique: P5 H5 E4 S5 R4 V4 · contrast: pass (40–41) · mobile: pass
 */
:root {
  --color-paper: oklch(16% 0.014 151);
  --color-paper-2: oklch(19.5% 0.016 151);
  --color-paper-3: oklch(23% 0.018 151);
  --color-paper-raised: oklch(26% 0.021 151);
  --color-ink: oklch(95% 0.012 130);
  --color-ink-2: oklch(84% 0.016 140);
  --color-muted: oklch(72% 0.021 145);
  --color-rule: oklch(32% 0.025 150);
  --color-control-rule: oklch(54% 0.025 150);
  --color-accent: oklch(88% 0.19 127);
  --color-accent-ink: oklch(17% 0.022 140);
  --color-accent-soft: oklch(27% 0.044 135);
  --color-focus: oklch(59% 0.18 263);
  --color-info: oklch(77% 0.10 250);
  --color-warning: oklch(84% 0.12 75);
  --color-error: oklch(79% 0.12 28);
  --color-error-soft: oklch(24% 0.032 28);
  --color-transparent: oklch(0% 0 0 / 0);
  --color-qr-paper: oklch(100% 0 0);
  --font-display: "IBM Plex Mono", ui-monospace, monospace;
  --font-body: "Manrope", sans-serif;
  --font-mono: var(--font-display);
  --space-2xs: 0.25rem;
  --space-xs: 0.5rem;
  --space-sm: 0.75rem;
  --space-md: 1rem;
  --space-lg: 1.5rem;
  --space-xl: 2rem;
  --space-2xl: 3rem;
  --space-3xl: 4rem;
  --text-xs: 0.75rem;
  --text-sm: 0.8125rem;
  --text-md: 0.9375rem;
  --text-lg: 1.125rem;
  --text-xl: 1.375rem;
  --text-2xl: 1.75rem;
  --text-display: clamp(1.75rem, 2.5vw, 2.5rem);
  --ease-out: cubic-bezier(0.16, 1, 0.3, 1);
  --ease-in: cubic-bezier(0.7, 0, 0.84, 0);
  --ease-in-out: cubic-bezier(0.65, 0, 0.35, 1);
  --dur-micro: 120ms;
  --dur-short: 220ms;
  --dur-long: 420ms;
  --rule-fine: 1px;
  --rule-strong: 2px;
  --radius-card: 12px;
  --radius-input: 6px;
  --radius-pill: 999px;
  --control-height: 44px;
  --z-base: 1;
  --z-sticky: 10;
  --border: var(--color-rule);
  --surface: var(--color-paper-2);
}
```

### Tailwind v4
```css
@theme inline {
  --color-paper: oklch(16% 0.014 151);
  --color-paper-2: oklch(19.5% 0.016 151);
  --color-paper-3: oklch(23% 0.018 151);
  --color-paper-raised: oklch(26% 0.021 151);
  --color-ink: oklch(95% 0.012 130);
  --color-ink-2: oklch(84% 0.016 140);
  --color-muted: oklch(72% 0.021 145);
  --color-rule: oklch(32% 0.025 150);
  --color-control-rule: oklch(54% 0.025 150);
  --color-accent: oklch(88% 0.19 127);
  --color-accent-ink: oklch(17% 0.022 140);
  --color-accent-soft: oklch(27% 0.044 135);
  --color-focus: oklch(59% 0.18 263);
  --color-info: oklch(77% 0.10 250);
  --color-warning: oklch(84% 0.12 75);
  --color-error: oklch(79% 0.12 28);
  --color-error-soft: oklch(24% 0.032 28);
  --color-transparent: oklch(0% 0 0 / 0);
  --color-qr-paper: oklch(100% 0 0);
  --font-display: "IBM Plex Mono", ui-monospace, monospace;
  --font-body: "Manrope", sans-serif;
  --font-mono: var(--font-display);
  --text-xs: 0.75rem;
  --text-sm: 0.8125rem;
  --text-md: 0.9375rem;
  --text-lg: 1.125rem;
  --text-xl: 1.375rem;
  --text-2xl: 1.75rem;
  --text-display: clamp(1.75rem, 2.5vw, 2.5rem);
  --ease-out: cubic-bezier(0.16, 1, 0.3, 1);
  --ease-in: cubic-bezier(0.7, 0, 0.84, 0);
  --ease-in-out: cubic-bezier(0.65, 0, 0.35, 1);
}
```

### DTCG tokens.json
```json
{
  "color": {
    "paper": {
      "$value": "oklch(16% 0.014 151)",
      "$type": "color"
    },
    "paper-2": {
      "$value": "oklch(19.5% 0.016 151)",
      "$type": "color"
    },
    "paper-3": {
      "$value": "oklch(23% 0.018 151)",
      "$type": "color"
    },
    "paper-raised": {
      "$value": "oklch(26% 0.021 151)",
      "$type": "color"
    },
    "ink": {
      "$value": "oklch(95% 0.012 130)",
      "$type": "color"
    },
    "ink-2": {
      "$value": "oklch(84% 0.016 140)",
      "$type": "color"
    },
    "muted": {
      "$value": "oklch(72% 0.021 145)",
      "$type": "color"
    },
    "rule": {
      "$value": "oklch(32% 0.025 150)",
      "$type": "color"
    },
    "control-rule": {
      "$value": "oklch(54% 0.025 150)",
      "$type": "color"
    },
    "accent": {
      "$value": "oklch(88% 0.19 127)",
      "$type": "color"
    },
    "accent-ink": {
      "$value": "oklch(17% 0.022 140)",
      "$type": "color"
    },
    "accent-soft": {
      "$value": "oklch(27% 0.044 135)",
      "$type": "color"
    },
    "focus": {
      "$value": "oklch(59% 0.18 263)",
      "$type": "color"
    },
    "info": {
      "$value": "oklch(77% 0.10 250)",
      "$type": "color"
    },
    "warning": {
      "$value": "oklch(84% 0.12 75)",
      "$type": "color"
    },
    "error": {
      "$value": "oklch(79% 0.12 28)",
      "$type": "color"
    },
    "error-soft": {
      "$value": "oklch(24% 0.032 28)",
      "$type": "color"
    },
    "transparent": {
      "$value": "oklch(0% 0 0 / 0)",
      "$type": "color"
    },
    "qr-paper": {
      "$value": "oklch(100% 0 0)",
      "$type": "color"
    }
  },
  "font": {
    "display": {
      "$value": "\"IBM Plex Mono\", ui-monospace, monospace",
      "$type": "fontFamily"
    },
    "body": {
      "$value": "\"Manrope\", sans-serif",
      "$type": "fontFamily"
    },
    "mono": {
      "$value": "var(--font-display)",
      "$type": "fontFamily"
    }
  },
  "space": {
    "2xs": {
      "$value": "0.25rem",
      "$type": "dimension"
    },
    "xs": {
      "$value": "0.5rem",
      "$type": "dimension"
    },
    "sm": {
      "$value": "0.75rem",
      "$type": "dimension"
    },
    "md": {
      "$value": "1rem",
      "$type": "dimension"
    },
    "lg": {
      "$value": "1.5rem",
      "$type": "dimension"
    },
    "xl": {
      "$value": "2rem",
      "$type": "dimension"
    },
    "2xl": {
      "$value": "3rem",
      "$type": "dimension"
    },
    "3xl": {
      "$value": "4rem",
      "$type": "dimension"
    }
  }
}
```

### shadcn/ui variables
```css
:root {
  --background: var(--color-paper);
  --foreground: var(--color-ink);
  --card: var(--color-paper-2);
  --card-foreground: var(--color-ink);
  --primary: var(--color-accent);
  --primary-foreground: var(--color-accent-ink);
  --secondary: var(--color-paper-3);
  --secondary-foreground: var(--color-ink-2);
  --muted: var(--color-paper-3);
  --muted-foreground: var(--color-muted);
  --border: var(--color-rule);
  --input: var(--color-control-rule);
  --ring: var(--color-focus);
  --destructive: var(--color-error);
  --radius: var(--radius-input);
}
```
