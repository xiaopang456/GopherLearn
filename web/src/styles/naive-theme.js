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
  paper: '#f4f2ee',
  surface: '#ffffff',
  surfaceSoft: '#faf9f6',
  line: '#e3dfd6',
  ink: '#1d2b2a',
  inkSoft: '#5c6b69',
  inkFaint: '#6f7d7b',
  accent: '#0f766e',
  accentSoft: '#d8ece9',
  accentInk: '#0a4f49',
  danger: '#a32d2a',
  warn: '#8a5a00',
}

const FONT_STACK =
  "'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', system-ui, -apple-system, sans-serif"

const FOCUS_SHADOW = '0 0 0 3px rgba(15, 118, 110, 0.35)'

export const naiveThemeOverrides = {
  common: {
    primaryColor: PALETTE.accent,
    primaryColorHover: '#0d6a63',
    primaryColorPressed: PALETTE.accentInk,
    primaryColorSuppl: PALETTE.accent,

    successColor: PALETTE.accent,
    successColorHover: '#0d6a63',
    successColorPressed: PALETTE.accentInk,
    warningColor: PALETTE.warn,
    warningColorHover: '#7a5100',
    warningColorPressed: '#6a4700',
    errorColor: PALETTE.danger,
    errorColorHover: '#932925',
    errorColorPressed: '#83241f',
    infoColor: PALETTE.inkSoft,

    textColorBase: PALETTE.ink,
    textColor1: PALETTE.ink,
    textColor2: PALETTE.inkSoft,
    textColor3: PALETTE.inkFaint,
    textColorDisabled: '#9aa5a3',
    placeholderColor: '#8a9694',
    placeholderColorDisabled: '#b3bcba',

    borderColor: PALETTE.line,
    dividerColor: PALETTE.line,
    border: `1px solid ${PALETTE.line}`,
    borderRadius: '8px',
    borderRadiusSmall: '6px',

    fontFamily: FONT_STACK,
    fontFamilyMono: "'Cascadia Mono', 'JetBrains Mono', Consolas, 'Liberation Mono', monospace",
    fontSize: '15px',
    fontSizeSmall: '13px',
    fontSizeMedium: '15px',
    fontSizeLarge: '16px',
    fontSizeHeading1: '24px',
    fontSizeHeading2: '18px',
    fontSizeHeading3: '15px',
    fontWeightStrong: '600',

    heightSmall: '32px',
    heightMedium: '40px',
    heightLarge: '48px',

    opacityDisabled: '0.55',
    boxShadow1: '0 1px 2px rgba(29, 43, 42, 0.06)',
    boxShadow2: '0 1px 2px rgba(29, 43, 42, 0.06), 0 8px 24px rgba(29, 43, 42, 0.06)',
    boxShadow3: '0 8px 32px rgba(29, 43, 42, 0.14)',
  },

  Button: {
    fontWeight: '600',
    heightSmall: '32px',
    heightMedium: '40px',
    heightLarge: '48px',
    borderRadiusSmall: '6px',
    borderRadiusMedium: '8px',
    borderRadiusLarge: '8px',
    fontSizeSmall: '13px',
    fontSizeMedium: '15px',
    paddingSmall: '0 12px',
    paddingMedium: '0 16px',
    color: PALETTE.surface,
    colorHover: PALETTE.surfaceSoft,
    colorPressed: PALETTE.surfaceSoft,
    textColor: PALETTE.ink,
    textColorHover: PALETTE.accentInk,
    textColorDisabled: PALETTE.inkFaint,
    border: `1px solid ${PALETTE.line}`,
    borderHover: `1px solid ${PALETTE.accent}`,
    borderPressed: `1px solid ${PALETTE.accentInk}`,
    borderFocus: `1px solid ${PALETTE.accent}`,
    borderHoverPrimary: `1px solid ${PALETTE.accent}`,
    borderPressedPrimary: `1px solid ${PALETTE.accentInk}`,
    borderFocusPrimary: `1px solid ${PALETTE.accent}`,
    colorHoverPrimary: PALETTE.accentInk,
    textColorHoverPrimary: '#ffffff',
    textColorPressedPrimary: '#ffffff',
    textColorFocusPrimary: '#ffffff',
  },

  Input: {
    heightMedium: '40px',
    borderRadius: '8px',
    fontSize: '15px',
    color: PALETTE.surface,
    colorFocus: PALETTE.surface,
    colorDisabled: PALETTE.surfaceSoft,
    textColor: PALETTE.ink,
    placeholderColor: '#8a9694',
    border: `1px solid ${PALETTE.line}`,
    borderHover: `1px solid ${PALETTE.accent}`,
    borderFocus: `1px solid ${PALETTE.accent}`,
    boxShadowFocus: FOCUS_SHADOW,
    boxShadowActive: FOCUS_SHADOW,
  },

  InputNumber: {
    heightMedium: '40px',
    borderRadius: '8px',
    fontSize: '15px',
    color: PALETTE.surface,
    colorFocus: PALETTE.surface,
    border: `1px solid ${PALETTE.line}`,
    borderHover: `1px solid ${PALETTE.accent}`,
    borderFocus: `1px solid ${PALETTE.accent}`,
    boxShadowFocus: FOCUS_SHADOW,
  },

  Select: {
    peers: {
      InternalSelection: {
        heightMedium: '40px',
        heightSmall: '32px',
        borderRadius: '8px',
        fontSize: '15px',
        color: PALETTE.surface,
        colorActive: PALETTE.surface,
        colorFocus: PALETTE.surface,
        textColor: PALETTE.ink,
        placeholderColor: '#8a9694',
        border: `1px solid ${PALETTE.line}`,
        borderHover: `1px solid ${PALETTE.accent}`,
        borderActive: `1px solid ${PALETTE.accent}`,
        borderFocus: `1px solid ${PALETTE.accent}`,
        boxShadowActive: FOCUS_SHADOW,
        boxShadowFocus: FOCUS_SHADOW,
      },
    },
  },

  Tag: {
    borderRadius: '999px',
    fontSizeSmall: '12px',
    fontSizeMedium: '13px',
    heightSmall: '22px',
    heightMedium: '24px',
  },

  Card: {
    borderRadius: '12px',
    border: `1px solid ${PALETTE.line}`,
    color: PALETTE.surface,
  },

  Alert: {
    borderRadius: '8px',
    fontSize: '14px',
  },

  Empty: {
    fontSize: '15px',
    color: PALETTE.inkFaint,
  },

  Popover: {
    borderRadius: '8px',
    boxShadow: '0 8px 32px rgba(29, 43, 42, 0.14)',
  },

  Tooltip: {
    borderRadius: '6px',
    fontSize: '13px',
  },

  Drawer: {
    borderRadius: '0px',
    color: PALETTE.surface,
  },
}

export { PALETTE as naivePalette }
