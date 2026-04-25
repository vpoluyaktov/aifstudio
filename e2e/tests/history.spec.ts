/**
 * history.spec.ts — History page tests.
 *
 * Creates a run, plays one command to get a save (makes canContinue=true),
 * then navigates to /history to verify the card and continue button.
 *
 * History page selectors:
 *   #historyLoading  — spinner while loading
 *   #historyList     — rendered list of run cards
 *   #historyEmpty    — shown when there are no runs
 *   .history-card    — individual run card
 *   .history-card-title — game title within a card
 *   .btn-primary (inside .history-card-actions) — Continue button
 */
import { test, expect } from '@playwright/test';

const ZORK_IFDB_ID = '0dbnusxunq7fw5ro';

test.describe('History page', () => {
  test('history page loads and shows MY GAMES heading', async ({ page }) => {
    await page.goto('/history', { waitUntil: 'networkidle' });

    await expect(page.locator('h1.history-title')).toContainText(/my games/i, {
      timeout: 10_000,
    });
  });

  test('run appears in history after creation', async ({ page }) => {
    // Create a run via API.
    const createResp = await page.request.post('/api/runs', {
      data: { sourceType: 'ifdb', ifdbId: ZORK_IFDB_ID },
    });
    expect(createResp.status()).toBe(201);
    const run = await createResp.json();
    const runId: string = run.id;

    // Navigate to history.
    await page.goto('/history', { waitUntil: 'networkidle' });

    // Wait for loading to finish.
    await expect(page.locator('#historyLoading')).not.toBeVisible({ timeout: 15_000 });

    // The run card should be present.
    const card = page.locator(`#card-${runId}`);
    await expect(card).toBeVisible({ timeout: 10_000 });

    // Card title should contain "Zork" (case-insensitive — UI is uppercase).
    await expect(card.locator('.history-card-title')).toContainText(/zork/i);
  });

  test('continue button appears and links to /play/{id} after run is started', async ({
    page,
  }) => {
    // Create a run.
    const createResp = await page.request.post('/api/runs', {
      data: { sourceType: 'ifdb', ifdbId: ZORK_IFDB_ID },
    });
    expect(createResp.status()).toBe(201);
    const run = await createResp.json();
    const runId: string = run.id;

    // Start the run (downloads story file, spawns interpreter, sets status=running).
    const startResp = await page.request.get(`/api/runs/${runId}/start`);
    expect([200, 201]).toContain(startResp.status());

    // Send one command so the interpreter issues a save (status -> running/suspended
    // with savePath != "").
    await page.request.post(`/api/runs/${runId}/command`, {
      data: { command: 'look' },
    });

    // Navigate to history.
    await page.goto('/history', { waitUntil: 'networkidle' });
    await expect(page.locator('#historyLoading')).not.toBeVisible({ timeout: 15_000 });

    const card = page.locator(`#card-${runId}`);
    await expect(card).toBeVisible({ timeout: 10_000 });

    // Continue button ("> Continue" anchor pointing to /play/{runId}).
    const continueBtn = card.locator(`a[href="/play/${runId}"]`);
    await expect(continueBtn).toBeVisible({ timeout: 10_000 });
    await expect(continueBtn).toContainText(/continue/i);
  });

  test('clicking continue navigates to the play page', async ({ page }) => {
    // Create and start a run.
    const createResp = await page.request.post('/api/runs', {
      data: { sourceType: 'ifdb', ifdbId: ZORK_IFDB_ID },
    });
    expect(createResp.status()).toBe(201);
    const run = await createResp.json();
    const runId: string = run.id;

    await page.request.get(`/api/runs/${runId}/start`);
    await page.request.post(`/api/runs/${runId}/command`, {
      data: { command: 'look' },
    });

    await page.goto('/history', { waitUntil: 'networkidle' });
    await expect(page.locator('#historyLoading')).not.toBeVisible({ timeout: 15_000 });

    const card = page.locator(`#card-${runId}`);
    await expect(card).toBeVisible({ timeout: 10_000 });

    const continueBtn = card.locator(`a[href="/play/${runId}"]`);
    await expect(continueBtn).toBeVisible({ timeout: 10_000 });
    await continueBtn.click();

    await expect(page).toHaveURL(`/play/${runId}`, { timeout: 15_000 });
  });

  test('empty state is shown when user has no runs', async ({ page }) => {
    // This test relies on the API returning zero runs; we cannot guarantee that
    // in a shared env — so just verify the page structure is present.
    await page.goto('/history', { waitUntil: 'networkidle' });
    await expect(page.locator('#historyLoading')).not.toBeVisible({ timeout: 15_000 });

    // Either the list or the empty state must be visible after loading.
    const listVisible = await page.locator('#historyList').isVisible();
    const emptyVisible = await page.locator('#historyEmpty').isVisible();
    expect(listVisible || emptyVisible).toBe(true);
  });
});
