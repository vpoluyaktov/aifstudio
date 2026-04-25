/**
 * Auth setup — runs once before authenticated test projects.
 *
 * POSTs credentials directly to /api/auth/login (faster than UI login),
 * then captures the resulting aifstudio_session cookie via storageState
 * and writes it to auth.json for all dependent test files.
 */
import { test as setup, expect } from '@playwright/test';

const AUTH_FILE = 'auth.json';
const TEST_EMAIL = 'claude@aifstudio.org';
const TEST_PASSWORD = 'Cl4ude$cr33nsh0t';

setup('authenticate via API and save session', async ({ page }) => {
  // POST to the login endpoint using the page-bound request context so that
  // response cookies are stored in the browser context's cookie jar.
  const resp = await page.request.post('/api/auth/login', {
    data: { email: TEST_EMAIL, password: TEST_PASSWORD },
  });

  expect(resp.status(), `Login failed: ${await resp.text()}`).toBe(200);

  const body = await resp.json();
  expect(body).toHaveProperty('user');
  expect(body.user.email).toBe(TEST_EMAIL);

  // Capture cookies (including aifstudio_session) to auth.json.
  await page.context().storageState({ path: AUTH_FILE });
});
