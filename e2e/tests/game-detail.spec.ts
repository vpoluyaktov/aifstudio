/**
 * game-detail.spec.ts — Game detail page tests.
 *
 * Uses Zork I (IFDB ID: 0dbnusxunq7fw5ro) as the canonical test fixture.
 * All UI text is uppercase — assertions use case-insensitive matchers.
 */
import { test, expect } from '@playwright/test';

const ZORK_IFDB_ID = '0dbnusxunq7fw5ro';
const ZORK_URL = `/games/${ZORK_IFDB_ID}`;

test.describe('Game detail page', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto(ZORK_URL, { waitUntil: 'networkidle' });
    // Wait for the game content to load (JS fetches /api/ifdb/games/{id}).
    await expect(page.locator('#gameContent')).toBeVisible({ timeout: 15_000 });
  });

  test('game title is visible', async ({ page }) => {
    // Title is rendered inside #gameHeader by client-side JS.
    const header = page.locator('#gameHeader');
    await expect(header).toContainText(/zork/i, { timeout: 10_000 });
  });

  test('game cover image or placeholder is visible', async ({ page }) => {
    const header = page.locator('#gameHeader');
    // Either a cover <img> or the placeholder div should be present.
    const coverImg = header.locator('img.game-cover');
    const placeholder = header.locator('.game-cover-placeholder');
    const hasCover = await coverImg.isVisible().catch(() => false);
    const hasPlaceholder = await placeholder.isVisible().catch(() => false);
    expect(hasCover || hasPlaceholder).toBe(true);
  });

  test('game authors are displayed', async ({ page }) => {
    await expect(page.locator('#gameHeader')).toContainText(/marc blank|dave lebling/i);
  });

  test('play / start button is visible', async ({ page }) => {
    // The play button may say "PLAY", "START", "[ PLAY ]", "> PLAY", etc.
    const actions = page.locator('#gameActions');
    await expect(actions).toBeVisible({ timeout: 10_000 });
    const playBtn = actions.locator('button, a').filter({ hasText: /play|start/i }).first();
    await expect(playBtn).toBeVisible({ timeout: 10_000 });
  });

  test('clicking play navigates to /play/{runId}', async ({ page }) => {
    const actions = page.locator('#gameActions');
    await expect(actions).toBeVisible({ timeout: 10_000 });

    const playBtn = actions.locator('button, a').filter({ hasText: /play|start/i }).first();
    await expect(playBtn).toBeVisible({ timeout: 10_000 });
    await playBtn.click();

    // Should navigate to /play/<runId> (r- followed by 26 uppercase alphanumeric chars).
    await expect(page).toHaveURL(/\/play\/r-[0-9A-Z]{26}/, { timeout: 20_000 });
  });
});
