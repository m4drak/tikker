// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

import { defineConfig } from '@playwright/test';

// The stack (tikker + fake-routeros, own data dir and ports) is started per
// test by the `stack` fixture in fixtures.ts. A single static `webServer`
// cannot give parallel workers isolated SQLite databases.
export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : undefined,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  outputDir: '../../test-results/e2e-artifacts',
  reporter: [['list'], ['html', { outputFolder: '../../test-results/e2e-report', open: 'never' }]],
  use: {
    locale: 'en-US', // the panel picks the language from Accept-Language
    trace: 'retain-on-failure',
    actionTimeout: 10_000,
    navigationTimeout: 15_000,
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium', headless: true } }],
});
