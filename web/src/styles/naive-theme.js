/**
 * Naive UI theme overrides.
 *
 * The library supplies behaviour and accessibility (keyboard handling, focus
 * management, ARIA); the *look* still comes from this project's own tokens.
 *
 * Everything is set on `common` and nowhere else, on purpose: Naive derives
 * each component's colours from those few variables (a primary button's fill
 * is `common.primaryColor`, an input's border is `common.borderColor`, and so
 * on). Overriding a component's own keys means guessing at a hundred derived
 * names and getting a `undefined` colour the moment one of them is missed —
 * so the palette lives in one place and the components follow.
 *
 * The values mirror `styles/base.css`, which is the source of truth for the
 * app's visual language. They are written out rather than read from the CSS
 * custom properties because reading them needs a live document, which breaks
 * unit tests. If a colour changes in `base.css`, change it here too.
 */

const PALETTE = {
  canvas: '#ffffff',
  soft: '#f6f6f7',
  line: '#e0e0e4',
  ink: '#17171a',
  inkSoft: '#5b5b66',
  inkFaint: '#6e6e79',
  brand: '#0f766e',
  brandSoft: '#e6f1ef',
  brandInk: '#0b5a54',
  danger: '#b3261e',
  warn: '#8a5a00',
}

const FONT_STACK =
  "'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', system-ui, -apple-system, sans-serif"

export const naiveThemeOverrides = {
  common: {
    // The brand teal is an *accent* here, not the primary action colour: the
    // main action is ink, and teal is spent on the active navigation item,
    // links, focus and the brand mark. That is what keeps a screen with a
    // dozen buttons from turning into a wall of teal.
    primaryColor: PALETTE.ink,
    primaryColorHover: '#3a3a40',
    primaryColorPressed: '#000000',
    primaryColorSuppl: PALETTE.brand,

    successColor: PALETTE.brand,
    successColorHover: '#0c6a63',
    successColorPressed: PALETTE.brandInk,
    warningColor: PALETTE.warn,
    warningColorHover: '#7a5100',
    warningColorPressed: '#6a4700',
    errorColor: PALETTE.danger,
    errorColorHover: '#9c211a',
    errorColorPressed: '#8a1d17',
    infoColor: PALETTE.inkSoft,

    textColorBase: PALETTE.ink,
    textColor1: PALETTE.ink,
    textColor2: PALETTE.inkSoft,
    textColor3: PALETTE.inkFaint,
    textColorDisabled: '#a4a4ae',
    placeholderColor: '#9b9ba6',
    placeholderColorDisabled: '#c2c2ca',

    // A hairline still has to be visible to do its job: at #e9e9ec on white
    // it read as nothing at all, and every input looked unstyled.
    borderColor: PALETTE.line,
    dividerColor: PALETTE.line,

    borderRadius: '8px',
    borderRadiusSmall: '6px',

    fontFamily: FONT_STACK,
    fontFamilyMono: "'Cascadia Mono', 'JetBrains Mono', Consolas, 'Liberation Mono', monospace",
    fontSize: '14px',
    fontSizeSmall: '13px',
    fontSizeMedium: '14px',
    fontSizeLarge: '15px',
    fontSizeHeading1: '22px',
    fontSizeHeading2: '17px',
    fontSizeHeading3: '14px',
    fontWeight: '500',
    fontWeightStrong: '600',

    heightSmall: '30px',
    heightMedium: '36px',
    heightLarge: '44px',

    opacityDisabled: '0.5',
    boxShadow1: '0 1px 2px rgba(23, 23, 26, 0.04)',
    boxShadow2: '0 1px 2px rgba(23, 23, 26, 0.04), 0 4px 12px rgba(23, 23, 26, 0.04)',
    boxShadow3: '0 12px 40px rgba(23, 23, 26, 0.14)',
  },
}

export { PALETTE as naivePalette }
