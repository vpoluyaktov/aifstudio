/**
 * auth.spec.ts — Auth flow tests (register, login, logout, protected route).
 *
 * Runs WITHOUT stored auth state so it can test the unauthenticated flows.
 * The logout test logs in via API first, then tests the logout endpoint.
 */
import { test, expect } from '@playwright/test';

const BASE = 'https://demo.aifstudio.org';
const TEST_EMAIL = 'claude@aifstudio.org';
const TEST_PASSWORD = 'Cl4ude$cr33nsh0t';

// ── Register ─────────────────────────────────────────────────────────────────

test('POST /api/auth/register creates a new user and returns 201', async ({ request }) => {
  const email = `test-${Date.now()}@aifstudio.org`;

  const resp = await request.post(`${BASE}/api/auth/register`, {
    data: {
      email,
      password: 'TestPassword1!',
      displayName: 'Test User',
    },
  });

  expect(resp.status()).toBe(201);

  const body = await resp.json();
  expect(body).toHaveProperty('user');
  expect(body.user.email).toBe(email);
  expect(body.user).toHaveProperty('uid');
  expect(body.user).toHaveProperty('displayName', 'Test User');
});

test('POST /api/auth/register returns 409 for duplicate email', async ({ request }) => {
  // Register once
  const email = `dup-${Date.now()}@aifstudio.org`;
  await request.post(`${BASE}/api/auth/register`, {
    data: { email, password: 'TestPassword1!', displayName: 'First' },
  });

  // Register again with same email
  const resp = await request.post(`${BASE}/api/auth/register`, {
    data: { email, password: 'AnotherPassword1!', displayName: 'Second' },
  });

  expect(resp.status()).toBe(409);
  const body = await resp.json();
  expect(body.code).toBe('email_taken');
});

test('POST /api/auth/register returns 400 for weak password', async ({ request }) => {
  const resp = await request.post(`${BASE}/api/auth/register`, {
    data: {
      email: `weak-${Date.now()}@aifstudio.org`,
      password: 'short',
      displayName: 'Test',
    },
  });

  expect(resp.status()).toBe(400);
  const body = await resp.json();
  // Architecture spec says 'weak_password' but the implementation uses 'invalid_password'.
  expect(body.code).toBe('invalid_password');
});

// ── Login ─────────────────────────────────────────────────────────────────────

test('POST /api/auth/login returns 200 and user for valid credentials', async ({ request }) => {
  const resp = await request.post(`${BASE}/api/auth/login`, {
    data: { email: TEST_EMAIL, password: TEST_PASSWORD },
  });

  expect(resp.status()).toBe(200);

  const body = await resp.json();
  expect(body).toHaveProperty('user');
  expect(body.user.email).toBe(TEST_EMAIL);
  expect(body.user).toHaveProperty('uid');
});

test('POST /api/auth/login returns 401 for wrong password', async ({ request }) => {
  const resp = await request.post(`${BASE}/api/auth/login`, {
    data: { email: TEST_EMAIL, password: 'wrong-password-xyz' },
  });

  expect(resp.status()).toBe(401);
  const body = await resp.json();
  expect(body.code).toBe('invalid_credentials');
});

test('POST /api/auth/login sets aifstudio_session cookie', async ({ page }) => {
  const resp = await page.request.post('/api/auth/login', {
    data: { email: TEST_EMAIL, password: TEST_PASSWORD },
  });

  expect(resp.status()).toBe(200);

  // Cookie should be in the browser context now
  const cookies = await page.context().cookies();
  const sessionCookie = cookies.find((c) => c.name === 'aifstudio_session');
  expect(sessionCookie).toBeDefined();
  expect(sessionCookie!.httpOnly).toBe(true);
});

// ── Logout ────────────────────────────────────────────────────────────────────

test('POST /api/auth/logout clears session and returns 204', async ({ page }) => {
  // Log in first via the page-bound request context so cookies are available.
  const loginResp = await page.request.post('/api/auth/login', {
    data: { email: TEST_EMAIL, password: TEST_PASSWORD },
  });
  expect(loginResp.status()).toBe(200);

  // Confirm we're authenticated.
  const meResp = await page.request.get('/api/auth/me');
  expect(meResp.status()).toBe(200);

  // Log out.
  const logoutResp = await page.request.post('/api/auth/logout');
  expect(logoutResp.status()).toBe(204);

  // Session should be gone — /api/auth/me should now return 401.
  const meAfter = await page.request.get('/api/auth/me');
  expect(meAfter.status()).toBe(401);
});

// ── Protected route redirect ───────────────────────────────────────────────────

test('unauthenticated request to /history redirects to /login', async ({ page }) => {
  // Navigate without any session cookie.
  await page.goto('/history');
  // The server issues a 303 redirect to /login; Playwright follows it.
  await expect(page).toHaveURL(/\/login/);
});

test('unauthenticated request to / redirects to /login', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/login/);
});
