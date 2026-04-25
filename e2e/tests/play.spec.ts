/**
 * play.spec.ts — Interactive play tests.
 *
 * Creates a run via the API (faster and more reliable than the UI flow),
 * then navigates to /play/{id} to test the terminal interface.
 *
 * The terminal input is #termInput (not #commandInput — confirmed from play.html).
 * Output is rendered into #termOutput.
 *
 * All terminal output is uppercase (the CSS applies text-transform: uppercase).
 */
import { test, expect } from '@playwright/test';

const ZORK_IFDB_ID = '0dbnusxunq7fw5ro';

test.describe('Play page', () => {
  let runId: string;
  let playUrl: string;

  test.beforeEach(async ({ page }) => {
    // Create a new run via the API using the page-bound request context
    // (shares cookies from auth.json with the browser).
    const createResp = await page.request.post('/api/runs', {
      data: { sourceType: 'ifdb', ifdbId: ZORK_IFDB_ID },
    });

    expect(createResp.status(), `Create run failed: ${await createResp.text()}`).toBe(201);

    const run = await createResp.json();
    runId = run.id;
    playUrl = run.playUrl ?? `/play/${runId}`;

    // Navigate to the play page.
    await page.goto(playUrl, { waitUntil: 'networkidle' });
  });

  test('terminal input (#termInput) becomes enabled after game starts', async ({ page }) => {
    // The input starts disabled="disabled" and is enabled once the game is running.
    await expect(page.locator('#termInput')).toBeEnabled({ timeout: 30_000 });
  });

  test('terminal output (#termOutput) contains game intro text after start', async ({ page }) => {
    // Wait for the input to be enabled (game has started and sent initial output).
    await expect(page.locator('#termInput')).toBeEnabled({ timeout: 30_000 });

    const output = page.locator('#termOutput');
    await expect(output).not.toBeEmpty({ timeout: 10_000 });
    // Zork I always opens with "ZORK" in the intro.
    await expect(output).toContainText(/zork/i, { timeout: 10_000 });
  });

  test('sending "look" returns room description', async ({ page }) => {
    await expect(page.locator('#termInput')).toBeEnabled({ timeout: 30_000 });

    const outputBefore = await page.locator('#termOutput').textContent();

    await page.fill('#termInput', 'look');
    await page.keyboard.press('Enter');

    // Wait for new content to appear in the terminal.
    await expect(page.locator('#termOutput')).not.toHaveText(outputBefore ?? '', { timeout: 15_000 });

    // "look" in Zork I shows the current location — expect any descriptive text.
    await expect(page.locator('#termOutput')).not.toBeEmpty();
  });

  test('sending "inventory" returns inventory response', async ({ page }) => {
    await expect(page.locator('#termInput')).toBeEnabled({ timeout: 30_000 });

    const outputBefore = await page.locator('#termOutput').textContent();

    await page.fill('#termInput', 'inventory');
    await page.keyboard.press('Enter');

    await expect(page.locator('#termOutput')).not.toHaveText(outputBefore ?? '', { timeout: 15_000 });
    // Zork I responds with inventory contents or "empty-handed".
    await expect(page.locator('#termOutput')).toContainText(/inventory|carrying|empty/i, {
      timeout: 10_000,
    });
  });

  test('sending "go north" changes the output', async ({ page }) => {
    await expect(page.locator('#termInput')).toBeEnabled({ timeout: 30_000 });

    const outputBefore = await page.locator('#termOutput').textContent();

    await page.fill('#termInput', 'go north');
    await page.keyboard.press('Enter');

    // Output must change after the command.
    await expect(page.locator('#termOutput')).not.toHaveText(outputBefore ?? '', { timeout: 15_000 });
  });

  test('status bar shows RUNNING or CONNECTED after game starts', async ({ page }) => {
    await expect(page.locator('#termInput')).toBeEnabled({ timeout: 30_000 });

    // Status bar should show a running/connected state, not "LOADING".
    await expect(page.locator('#statusText')).not.toHaveText(/loading/i, { timeout: 10_000 });
  });

  test('exit link is visible on play page', async ({ page }) => {
    await expect(page.locator('a.term-exit-link')).toBeVisible({ timeout: 10_000 });
  });
});
