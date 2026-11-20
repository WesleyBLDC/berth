# Berth brand kit

Final logo: **Sleeping key**, a berth that is also a closed eye, holding a key. The idea: Berth keeps the secret, and the agent never sees it.

## What's in here

| Folder | Use it for |
|---|---|
| `logo/svg/` | The master files. Use SVG anywhere you can (web, Figma, slides). |
| `logo/png/` | The same logos as PNGs with transparent backgrounds, in 1x and @2x. |
| `app-icon/` | The macOS app icon (`Berth.icns`, plus PNGs from 16 to 1024 px). `app-icon-fullbleed` is the square version for platforms that round the corners themselves (iOS, Android, App Store listings). |
| `favicon/` | Website icons (`favicon.ico`, `favicon.svg`, `apple-touch-icon.png`, `icon-192/512.png`). |
| `menubar/` | macOS menu bar template image (`berthTemplate.png` / `@2x`). |
| `colors/` | `tokens.css` and `tokens.json` with every brand color and both fonts. |

### Which logo file to pick

- **On dark or navy backgrounds:** `berth-lockup-dark-bg` / `berth-mark-dark-bg` (white mark, brass key)
- **On white or cream backgrounds:** `berth-lockup-light-bg` / `berth-mark-light-bg` (ink mark, deep-brass key)
- **One color only** (stamps, embossing, single-ink print, photos): `-white` or `-black`
- **Logo on a ready-made background** (slides, social posts, press): `berth-lockup-on-navy` / `berth-lockup-on-cream`
- **Small spaces** (avatars, favicons, the dock): the mark alone. Below 24 px, use the app icon or favicon tile, not the bare mark.

The wordmark in the SVGs is converted to outlines, so it renders correctly without Space Grotesk installed.

## Colors

**Primary (dark):** this is the main brand look.

| Name | Hex | Role |
|---|---|---|
| Navy 800 | `#16263F` | Backgrounds, top of the gradient |
| Navy 900 | `#0A1424` | Deepest background, bottom of the gradient |
| Mist | `#F4F7FA` | Logo and main text on navy |
| Brass | `#F2C46D` | The key. Accent only: buttons, highlights, focus rings |

**Light partner:** for docs, the website's light mode and print.

| Name | Hex | Role |
|---|---|---|
| Cream | `#FBFAF6` | Light background |
| Ink | `#14213D` | Logo and main text on cream |
| Deep brass | `#B7791F` | The key on light backgrounds |

**Secondary text:** `#C9D2DC` on navy, `#4A5568` on cream.

Rules:

- The key is always brass (or deep brass on light backgrounds). The brass key is the signature, so don't recolor it.
- Use brass sparingly, as one accent per screen. If everything is gold, nothing stands out.
- Deep brass on cream passes contrast for graphics and large text (3:1), but not for small body text. Use Ink for body copy.
- App background gradient: `linear-gradient(180deg, #16263F 0%, #0A1424 100%)`.

## Typography

| Use | Font | Weight |
|---|---|---|
| Wordmark, headings | **Space Grotesk** | 700 for the wordmark and H1, 600 for H2/H3 |
| Body, UI, docs | **Inter** | 400 body, 500 labels, 600 buttons |
| Code, keys, tokens | **JetBrains Mono** or system mono | 400 |

- The wordmark is always lowercase **berth**, Space Grotesk Bold, tracking -3%. In body copy, write the name as "Berth".
- Both fonts are free on Google Fonts (SIL Open Font License):

```html
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&family=Space+Grotesk:wght@600;700&display=swap" rel="stylesheet">
```

## Using the logo

- **Clear space:** keep empty space around the logo equal to the height of the keyhole on every side.
- **Minimum size:** lockup 96 px wide on screen (25 mm in print); mark alone 16 px.
- **Don't:** stretch or skew it, rotate it, add shadows or glows, change the key color, outline the mark, put the light version on a busy photo, or re-typeset the wordmark in another font.

## Setup snippets

**Website `<head>`:**

```html
<link rel="icon" href="/favicon.ico" sizes="any">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<meta name="theme-color" content="#16263F">
```

For a web manifest, use `icon-192.png` and `icon-512.png`.

**CSS tokens:**

```css
@import "colors/tokens.css";
body { background: var(--berth-gradient); color: var(--berth-mist); font-family: var(--berth-font-body); }
h1, h2 { font-family: var(--berth-font-display); }
.accent { color: var(--berth-brass); }
```

**macOS app (Xcode):** drag `app-icon-macos-*.png` into the AppIcon slots in `Assets.xcassets` (16, 32, 64, 128, 256, 512, 1024). Or set `Berth.icns` as the app icon for non-Xcode builds (Electron, Tauri and similar).

**macOS menu bar (Swift):** add `berthTemplate.png` and `berthTemplate@2x.png` to your assets. Because the name ends in `Template`, macOS tints the icon automatically for light mode, dark mode and the highlighted state:

```swift
statusItem.button?.image = NSImage(named: "berthTemplate")
statusItem.button?.image?.isTemplate = true
```

**Raycast / Electron tray:** use `menubar/berthTemplate.png` the same way. For a colored extension icon, use `app-icon/app-icon-macos-512.png`.
