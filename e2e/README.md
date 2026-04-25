# AIFStudio — End-to-End Tests

Playwright test suite targeting [https://demo.aifstudio.org](https://demo.aifstudio.org).

## Setup

```bash
cd e2e
npm install
npx playwright install chromium --with-deps
```

## Running tests

```bash
# All tests (setup → e2e + auth)
npx playwright test

# Specific spec file
npx playwright test tests/play.spec.ts

# Headed (visible browser)
npx playwright test --headed

# Debug a single test
npx playwright test --debug tests/catalogue.spec.ts
```

## Project structure

| File | Purpose |
|------|---------|
| `tests/auth.setup.ts` | Logs in via API, writes `auth.json` |
| `tests/auth.spec.ts` | Register / login / logout / redirect (no stored auth) |
| `tests/catalogue.spec.ts` | IFDB search, results, empty state, click-to-detail |
| `tests/game-detail.spec.ts` | Game title/cover/play button, click play → /play/* |
| `tests/play.spec.ts` | Terminal output, send commands (look/inventory/go north) |
| `tests/history.spec.ts` | Run appears in history, continue button works |
| `tests/pages.spec.ts` | Projects + community pages load; /health and /api/config |

## Auth

Tests that require authentication use `auth.json` (written by `auth.setup.ts`).
`auth.json` is **gitignored** — it is created at runtime from the test account
credentials (`claude@aifstudio.org`).

## Key selectors

| Element | Selector |
|---------|---------|
| Search input (index) | `#searchInput` |
| Search button (index) | `#searchBtn` |
| Terminal input (play page) | `#termInput` |
| Terminal output (play page) | `#termOutput` |
| History list | `#historyList` |
| History card (by run ID) | `#card-{runId}` |
| Session cookie | `aifstudio_session` |

## Notes

- UI text is uppercase — all text assertions use `/regex/i` or `ignoreCase`.
- Pages wait for `networkidle` because `auth.js` performs an async
  `/api/auth/me` check on every page load.
- Play tests create runs via `POST /api/runs` (API) rather than clicking
  through the UI — faster and more reliable.
- The command input on the play page is `#termInput` (not `#commandInput`).
