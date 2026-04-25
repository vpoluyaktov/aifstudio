import { defineConfig, devices } from '@playwright/test';

/**
 * AIFStudio Playwright configuration.
 *
 * Projects:
 *  - setup:  runs auth.setup.ts once; writes auth.json
 *  - auth:   runs auth.spec.ts without stored auth (tests the auth flow itself)
 *  - e2e:    all other specs with storageState=auth.json; depends on setup
 */
export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: 'list',

  use: {
    baseURL: 'https://demo.aifstudio.org',
    trace: 'on-first-retry',
    actionTimeout: 20_000,
    navigationTimeout: 30_000,
  },

  projects: [
    // 1. Auth setup — runs first, saves cookies to auth.json
    {
      name: 'setup',
      testMatch: /auth\.setup\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },

    // 2. Authenticated tests — depend on setup; skip auth.spec.ts and setup
    {
      name: 'e2e',
      use: {
        ...devices['Desktop Chrome'],
        storageState: 'auth.json',
      },
      dependencies: ['setup'],
      testIgnore: [/auth\.setup\.ts/, /auth\.spec\.ts/],
    },

    // 3. Auth flow tests — no stored state; runs independently
    {
      name: 'auth',
      testMatch: /auth\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
