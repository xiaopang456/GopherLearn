# GopherLearn web client

Vue 3 + Vite implementation of the frontend contract in `../spec/API.md`.

The client is part of GopherLearn, which is developed on top of
[GopherAI](https://github.com/youngyangyang04/GopherAI); see the repository's
[NOTICE.md](../NOTICE.md) for provenance and licensing.

The end-to-end setup - configuration, database, backend, proxy - is in the
repository's [README.md](../README.md). This file only documents the client
itself.

## Node version

`package.json` declares

```
"engines": { "node": "^22.22.2 || ^24.15.0 || >=26.0.0" }
```

and `.nvmrc` pins `24.15.0`.

That range is not a preference — `@vue/test-utils` depends on `js-beautify`,
which depends on `nopt`/`abbrev`, and those two declare exactly this range. On
Node **24.14.1** `npm ci` therefore prints `EBADENGINE` warnings for
`nopt@10.0.1` and `abbrev@5.0.0`.

They are warnings, not failures: `engine-strict` is off, and `npm ci`,
`npm run lint`, `npm run test`, `npm run build` and `npm audit` all succeed on
24.14.1. Moving to Node 24.15+ (or the 22.22 LTS line) silences them.

**Do not downgrade `vitest`, `eslint`, `jsdom` or `@vue/test-utils` to quiet the
warning.** Those packages carry the security and correctness fixes this client's
tests depend on; `npm audit` is currently clean and must stay clean.

## Commands

```
npm ci          # install from the lockfile
npm run lint    # eslint, includes a rule set that forbids HTML sinks
npm run test    # vitest, jsdom environment, no network
npm run build   # production bundle into dist/
```

## Configuration

`VITE_API_BASE_URL` is the origin of the HTTP/SSE API; copy `.env.example` to
`.env` (git-ignored) to set it. An empty value means "call the same origin the
page is served from". No other build-time setting exists, and no credential
belongs in this tree — the bearer token is a runtime value only.

### Running against the local Go backend

The client is written against the **integrated entry**
(`hybrid/cmd/gopherlearn-integrated`), so start that one and point the dev
proxy at it. The integrated entry reads a configuration file and listens on the
address that file names; the example template listens on `127.0.0.1:18082`.

From the repository root:

```bash
cp config.integrated.example.toml config.integrated.toml.local   # then fill it in
go -C hybrid run ./cmd/gopherlearn-integrated -config ../config.integrated.toml.local
```

In another shell, from the repository root:

```
npm --prefix web ci
npm --prefix web run dev            # this client on http://127.0.0.1:5273
```

`vite.config.js` proxies `/api` to `VITE_DEV_API_ORIGIN` for the dev server
only. Its default is `http://127.0.0.1:8080`, which is where the **baseline**
entry (`cmd/gopherlearn`) listens - not where the integrated entry does. Set
the variable to the integrated entry's address, or the client will talk to a
port nothing is serving:

```
VITE_DEV_API_ORIGIN=http://127.0.0.1:18082
```

On PowerShell set it as `$env:VITE_DEV_API_ORIGIN = 'http://127.0.0.1:18082'`
before running `npm --prefix web run dev`.

Because the browser stays on one origin, the backend needs no CORS
allowance, and a production bundle still calls same-origin `/api`.

## Session modes and the knowledge toggle

Two modes, per the contract: `chat` and `learning_agent`. `POST /api/v1/sessions`
always carries a `mode`; the client never creates a session without one, and
`knowledge_qa` is not a mode — it is a per-turn option inside Chat.

The composer renders the grounding toolbar **only** when the mode the server
reported for the open session is `chat`, and the toggle is inert until a real
collection exists. There is no knowledge endpoint yet, so `collections` stays
empty and the toolbar shows "添加知识库后可启用" linking to the knowledge page.
`useChat().setCollections()` is the single writer that a later endpoint feeds.

When disabled, the stream request body is `{"content": "…"}` with no `knowledge`
key at all; that is the one representation this client standardises on, and a
test pins it.

An unrecognised mode normalises to `''`, which renders as "模式未识别", hides the
toolbar, sends no knowledge block, and records a
`session.mode.unknown` entry readable through `devIssues()` in
`src/lib/devlog.js`. It is never guessed to be an agent.

## Layout of `src/`

| Path | Responsibility |
| --- | --- |
| `api/` | HTTP envelope, auth calls, session CRUD, SSE transport |
| `auth/` | token storage and the signed-in profile |
| `lib/` | pure logic: SSE frame parser, run state machine, validation |
| `composables/` | chat orchestration, media-query subscription |
| `components/`, `views/` | presentation |
| `router/` | routes plus the authentication guard |

## Responsive layout

The one shared breakpoint is **1021px**, declared in two places that must stay
in step: `ChatView.vue` subscribes to `(min-width: 1021px)` through
`composables/useMediaQuery.js`, and the scoped CSS in `ChatView.vue` /
`SessionPanel.vue` / `NavRail.vue` switches on the same value.

Above it the session column is resident; below it the column becomes a drawer
that starts closed. `useMediaQuery` exists because `window.innerWidth` is not
reactive — reading it in a computed evaluates once and never re-runs on resize.
