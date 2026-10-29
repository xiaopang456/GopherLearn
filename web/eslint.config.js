import js from '@eslint/js'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'

export default [
  {
    ignores: ['dist/**', 'node_modules/**', 'coverage/**'],
  },
  js.configs.recommended,
  // `essential` is chosen over `recommended` on purpose: it keeps every
  // correctness/security rule and drops the purely stylistic ones so the
  // original markup below is not pushed toward a boilerplate shape.
  ...pluginVue.configs['flat/essential'],
  {
    files: ['**/*.{js,vue}'],
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      globals: {
        ...globals.browser,
      },
    },
    rules: {
      // Model output must never become an HTML sink. `no-v-html` covers
      // templates; the selector below covers the equivalent JavaScript forms.
      'vue/no-v-html': 'error',
      'vue/no-v-text-v-html-on-component': 'error',
      'no-restricted-syntax': [
        'error',
        {
          selector:
            'MemberExpression[property.name="innerHTML"], MemberExpression[property.name="outerHTML"], CallExpression[callee.property.name="insertAdjacentHTML"], CallExpression[callee.property.name="createContextualFragment"], CallExpression[callee.name="eval"], CallExpression[callee.property.name="setTimeout"][arguments.0.type!="Literal"]',
          message:
            'Rendering untrusted text as markup is not allowed in this client.',
        },
      ],
      'vue/require-explicit-emits': 'error',
      'vue/attribute-hyphenation': 'error',
      'vue/multi-word-component-names': ['error', { ignores: ['App'] }],
      'no-console': 'error',
      'no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
    },
  },
  {
    files: ['test/**/*.js', 'vite.config.js', 'eslint.config.js'],
    languageOptions: {
      globals: {
        ...globals.node,
      },
    },
  },
]
