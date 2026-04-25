/**
 * catalogue.spec.ts — IFDB catalogue search tests.
 *
 * Runs with stored auth (auth.json). UI text is uppercase — all text
 * assertions use case-insensitive matchers.
 *
 * Key selectors:
 *   #searchInput  — search text field
 *   #searchBtn    — submit button
 *   #resultsArea  — container where results are rendered
 */
import { test, expect } from '@playwright/test';

test.describe('Catalogue search', () => {
  test.beforeEach(async ({ page }) => {
    // Wait for networkidle because auth.js performs an async /api/auth/me on
    // every page load and may redirect to /login if the check hasn't resolved.
    await page.goto('/', { waitUntil: 'networkidle' });
  });

  test('search form elements are visible', async ({ page }) => {
    await expect(page.locator('#searchInput')).toBeVisible();
    await expect(page.locator('#searchBtn')).toBeVisible();
  });

  test('searching for "zork" returns results', async ({ page }) => {
    await page.fill('#searchInput', 'zork');
    await page.click('#searchBtn');

    // Results area should eventually contain at least one game card.
    const resultsArea = page.locator('#resultsArea');
    await expect(resultsArea).not.toBeEmpty({ timeout: 15_000 });

    // The results list contains "Zork" (case-insensitive — UI renders uppercase).
    await expect(resultsArea).toContainText(/zork/i);
  });

  test('searching returns result count header', async ({ page }) => {
    await page.fill('#searchInput', 'adventure');
    await page.click('#searchBtn');

    // Results area should show a result count or game titles.
    await expect(page.locator('#resultsArea')).not.toBeEmpty({ timeout: 15_000 });
  });

  test('empty search query does not navigate away', async ({ page }) => {
    await page.fill('#searchInput', '');
    await page.click('#searchBtn');

    // Should stay on the homepage.
    await expect(page).toHaveURL('/');
  });

  test('search for unlikely string shows empty state or zero results', async ({ page }) => {
    const unlikely = `zzznoresults${Date.now()}`;
    await page.fill('#searchInput', unlikely);
    await page.click('#searchBtn');

    // After a reasonable wait, either an empty-state message or a 0-count header.
    await page.waitForTimeout(6_000);
    const resultsText = await page.locator('#resultsArea').textContent();
    // Either empty, or contains "0" or "no results" or "no games" (case-insensitive).
    const looksEmpty =
      !resultsText ||
      resultsText.trim() === '' ||
      /0\s*(result|game)/i.test(resultsText) ||
      /no\s*(result|game)/i.test(resultsText);
    expect(looksEmpty).toBe(true);
  });

  test('clicking a search result navigates to game detail page', async ({ page }) => {
    await page.fill('#searchInput', 'zork');
    await page.click('#searchBtn');

    // Wait for results to appear.
    const resultsArea = page.locator('#resultsArea');
    await expect(resultsArea).not.toBeEmpty({ timeout: 15_000 });

    // Click the first result link (game card links point to /games/{id}).
    const firstLink = resultsArea.locator('a[href^="/games/"]').first();
    await expect(firstLink).toBeVisible({ timeout: 10_000 });
    await firstLink.click();

    await expect(page).toHaveURL(/\/games\/[a-z0-9]+/, { timeout: 15_000 });
  });
});
