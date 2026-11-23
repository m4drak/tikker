// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

import { test, expect, login, ADMIN_PASSWORD } from '../fixtures';

test('viewer cannot reach admin pages; admin can', async ({ page, stack, browser }) => {
  // Admin creates a viewer.
  await login(page, stack);
  await page.goto(stack.baseURL + '/users/new');
  await page.getByLabel('Username').fill('viewer1');
  await page.locator('#u-role').selectOption('viewer');
  await page.locator('#u-pass').fill(ADMIN_PASSWORD);
  await page.locator('#u-confirm').fill(ADMIN_PASSWORD);
  await page.getByRole('button', { name: /Create user|Add user|Create/ }).click();
  await page.goto(stack.baseURL + '/users');
  await expect(page.locator('#users')).toContainText('viewer1');

  // Admin sees the admin navigation.
  const adminNav = page.getByRole('navigation', { name: 'Main navigation' });
  for (const item of ['Devices', 'API tokens', 'Users', 'Audit log']) {
    await expect(adminNav.getByRole('link', { name: item })).toBeVisible();
  }

  // Viewer, in a separate browser context.
  const ctx = await browser.newContext({ locale: 'en-US' });
  const vp = await ctx.newPage();
  await login(vp, stack, 'viewer1');
  const nav = vp.getByRole('navigation', { name: 'Main navigation' });
  await expect(nav.getByRole('link', { name: 'Messages' })).toBeVisible();
  for (const item of ['Devices', 'API tokens', 'Users', 'Audit log']) {
    await expect(nav.getByRole('link', { name: item })).toHaveCount(0);
  }
  for (const p of ['/users', '/users/new', '/tokens', '/devices', '/devices/new', '/audit']) {
    const res = await vp.goto(stack.baseURL + p);
    expect(res?.status(), p).toBe(403);
  }
  // Viewers cannot send either.
  const send = await vp.goto(stack.baseURL + '/send');
  expect(send?.status()).toBe(403);
  await ctx.close();
});
