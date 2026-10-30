/**
 * Naive UI theme overrides.
 *
 * The library supplies behaviour and accessibility (keyboard handling, focus
 * management, ARIA); the *look* still comes from this project's own tokens.
 * Every value below mirrors `styles/base.css` — that file is the source of
 * truth for the palette, and this one keeps the library in the same visual
 * language rather than letting a second palette leak in.
 *
 * They are written out instead of read from the CSS custom properties on
 * purpose: reading them would need a live document, which breaks unit tests
 * and server rendering. If a colour changes in `base.css`, change it here too.
 */

const PALETTE = {
  canvas: '#ffffff',
  side: '#fbfbfc',
  soft: '#f6f6f7',
  line: '#e9e9ec',
  lineStrong: '#dbdbe0',
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

const FOCUS_SHADOW = '0 0 0 3px rgba(15, 118, 110, 0.22)'

export const naiveThemeOverrides = {
  common: {
    // The brand teal is an *accent* here, not the primary action colour: the
    // main action is ink, and teal is spent on the active navigation item,
    // links, focus and the brand mark. That is what keeps a screen with a
    // dozen buttons from turning into a wall of teal.
    primaryColor: PALETTE.ink,
    primaryColorHover: '#000000',
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

    borderColor: PALETTE.line,
    dividerColor: PALETTE.line,
    border: `1px solid ${PALETTE.line}`,
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
    fontWeightStrong: '600',

    heightSmall: '30px',
    heightMedium: '36px',
    heightLarge: '44px',

    opacityDisabled: '0.5',
    boxShadow1: '0 1px 2px rgba(23, 23, 26, 0.04)',
    boxShadow2: '0 1px 2px rgba(23, 23, 26, 0.04), 0 4px 12px rgba(23, 23, 26, 0.04)',
    boxShadow3: '0 12px 40px rgba(23, 23, 26, 0.14)',
  },

  Button: {
    fontWeight: '500',
    heightSmall: '30px',
    heightMedium: '36px',
    heightLarge: '44px',
    borderRadiusSmall: '6px',
    borderRadiusMedium: '8px',
    borderRadiusLarge: '8px',
    fontSizeSmall: '13px',
    fontSizeMedium: '14px',
    paddingSmall: '0 12px',
    paddingMedium: '0 14px',
    color: PALETTE.canvas,
    colorHover: PALETTE.soft,
    colorPressed: PALETTE.soft,
    textColor: PALETTE.ink,
    textColorHover: PALETTE.ink,
    textColorDisabled: PALETTE.inkFaint,
    // A secondary button still has to read as a button: the hairline is the
    // only thing separating it from the page.
    border: `1px solid ${PALETTE.lineStrong}`,
    borderHover: `1px solid ${PALETTE.inkFaint}`,
    borderPressed: `1px solid ${PALETTE.inkFaint}`,
    borderFocus: `1px solid ${PALETTE.ink}`,
    borderHoverPrimary: `1px solid ${PALETTE.ink}`,
    borderPressedPrimary: `1px solid ${PALETTE.ink}`,
    borderFocusPrimary: `1px solid ${PALETTE.ink}`,
    colorHoverPrimary: '#000000',
    textColorHoverPrimary: '#ffffff',
    textColorPressedPrimary: '#ffffff',
    textColorFocusPrimary: '#ffffff',
    textColorPrimary: '#ffffff',
  },

  Input: {
    heightMedium: '36px',
    borderRadius: '8px',
    fontSize: '14px',
    color: PALETTE.canvas,
    colorFocus: PALETTE.canvas,
    colorDisabled: PALETTE.soft,
    textColor: PALETTE.ink,
    placeholderColor: '#9b9ba6',
    border: `1px solid ${PALETTE.line}`,
    borderHover: `1px solid ${PALETTE.lineStrong}`,
    borderFocus: `1px solid ${PALETTE.brand}`,
    boxShadowFocus: FOCUS_SHADOW,
    boxShadowActive: FOCUS_SHADOW,
  },

  InputNumber: {
    heightMedium: '36px',
    borderRadius: '8px',
    fontSize: '14px',
    color: PALETTE.canvas,
    colorFocus: PALETTE.canvas,
    border: `1px solid ${PALETTE.line}`,
    borderHover: `1px solid ${PALETTE.lineStrong}`,
    borderFocus: `1px solid ${PALETTE.brand}`,
    boxShadowFocus: FOCUS_SHADOW,
  },

  Select: {
    peers: {
      InternalSelection: {
        heightMedium: '36px',
        heightSmall: '30px',
        borderRadius: '8px',
        fontSize: '14px',
        color: PALETTE.canvas,
        colorActive: PALETTE.canvas,
        colorFocus: PALETTE.canvas,
        textColor: PALETTE.ink,
        placeholderColor: '#9b9ba6',
        border: `1px solid ${PALETTE.line}`,
        borderHover: `1px solid ${PALETTE.lineStrong}`,
        borderActive: `1px solid ${PALETTE.brand}`,
        borderFocus: `1px solid ${PALETTE.brand}`,
        boxShadowActive: FOCUS_SHADOW,
        boxShadowFocus: FOCUS_SHADOW,
      },
    },
  },

  Tag: {
    borderRadius: '999px',
    fontSizeSmall: '11px',
    fontSizeMedium: '12px',
    heightSmall: '20px',
    heightMedium: '22px',
  },

  Card: {
    borderRadius: '12px',
    border: `1px solid ${PALETTE.line}`,
    color: PALETTE.canvas,
  },

  Alert: {
    borderRadius: '10px',
    fontSize: '13px',
  },

  Empty: {
    fontSize: '14px',
    color: PALETTE.inkFaint,
  },

  Popover: {
    borderRadius: '10px',
    boxShadow: '0 12px 40px rgba(23, 23, 26, 0.14)',
  },

  Tooltip: {
    borderRadius: '6px',
    fontSize: '12px',
  },

  Drawer: {
    borderRadius: '0px',
    color: PALETTE.canvas,
  },
}

export { PALETTE as naivePalette }
