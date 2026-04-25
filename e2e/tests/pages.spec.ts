/**
 * pages.spec.ts — Smoke tests for static UI pages.
 *
 * Verifies that the Projects and Community pages load without errors and
 * display their expected headings. All UI text is uppercase — assertions
 * use case-insensitive matchers.
 */
import { test, expect } from '@playwright/test';

test.describe('Projects page', () => {
  test('loads and shows projects heading', async ({ page }) => {
    await page.goto('/projects', { waitUntil: 'networkidle' });

    // Page should not redirect to /login (we're authenticated).
    await expect(page).toHaveURL(/\/projects/, { timeout: 10_000 });

    // Should have a recognisable heading — the template title is "MY PROJECTS" or similar.
    await expect(page.locator('body')).toContainText(/project/i, { timeout: 10_000 });
  });

  test('projects page contains a create link or button', async ({ page }) => {
    await page.goto('/projects', { waitUntil: 'networkidle' });

    // The projects page should have a way to create a new project.
    const createEl = page
      .locator('a, button')
      .filter({ hasText: /create|new project/i })
      .first();
    await expect(createEl).toBeVisible({ timeout: 10_000 });
  });

  test('loading state resolves (no infinite spinner)', async ({ page }) => {
    await page.goto('/projects', { waitUntil: 'networkidle' });

    // Any loading indicator should disappear.
    const spinner = page.locator('.loading-area, .spinner').first();
    if (await spinner.isVisible()) {
      await expect(spinner).not.toBeVisible({ timeout: 15_000 });
    }
    // If no spinner was visible, that's fine too.
  });
});

test.describe('Community page', () => {
  test('loads and shows community heading', async ({ page }) => {
    await page.goto('/community', { waitUntil: 'networkidle' });

    await expect(page).toHaveURL(/\/community/, { timeout: 10_000 });
    await expect(page.locator('body')).toContainText(/community/i, { timeout: 10_000 });
  });

  test('community page shows game grid or empty state', async ({ page }) => {
    await page.goto('/community', { waitUntil: 'networkidle' });

    // Loading should finish.
    await expect(page.locator('#commLoading')).not.toBeVisible({ timeout: 15_000 });

    // Either a game grid or the empty state should be visible.
    const gridVisible = await page.locator('#commGrid').isVisible();
    const emptyVisible = await page.locator('#commEmpty').isVisible();
    expect(gridVisible || emptyVisible).toBe(true);
  });

  test('community page has a "create your own" link', async ({ page }) => {
    await page.goto('/community', { waitUntil: 'networkidle' });

    const createLink = page.locator('a[href="/create"]').first();
    await expect(createLink).toBeVisible({ timeout: 10_000 });
  });
});

test.describe('Health endpoint', () => {
  test('GET /health returns ok', async ({ request }) => {
    const resp = await request.get('https://demo.aifstudio.org/health');
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.status).toBe('ok');
    expect(body).toHaveProperty('version');
  });
});

// Note: /api/config was removed from server.go — no test for it.
