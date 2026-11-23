// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

import { test, expect, login, ROUTER_USER, ROUTER_PASSWORD } from '../fixtures';

test('add a device, test the connection (port detection), save a channel', async ({ page, stack }) => {
  await login(page, stack);
  await page.goto(stack.baseURL + '/devices/new');

  await page.getByLabel('Name', { exact: true }).fill('lab-router');
  await page.locator('#dev-url').fill(stack.routerURL);
  await page.locator('#dev-user').fill(ROUTER_USER);
  await page.locator('#dev-pass').fill(ROUTER_PASSWORD);
  await page.locator('#dev-tls').selectOption('insecure');
  await page.getByRole('button', { name: 'Add device' }).click();

  await expect(page.getByRole('heading', { name: 'lab-router' })).toBeVisible();
  await expect(page.getByText('Device added.')).toBeVisible();
  await expect(page.locator('#insecure-banner')).toBeVisible();

  await page.getByRole('button', { name: 'Test connection' }).click();
  await expect(page.locator('#test-result')).toContainText('The connection works.');
  await expect(page.locator('#test-board')).toContainText('FakeBoard');
  const ports = page.locator('#test-ports');
  await expect(ports).toContainText('lte1');
  await expect(ports).toContainText('lte2');

  await ports.locator('tr', { hasText: 'lte1' }).getByRole('button', { name: 'Add channel' }).click();
  await expect(page.locator('#channels')).toContainText('lte1');

  await page.goto(stack.baseURL + '/devices');
  await expect(page.locator('#devices')).toContainText('lab-router');
  await expect(page.locator('#devices')).toContainText('lte1');
});

test('wrong router password makes the connection test fail', async ({ page, stack }) => {
  await login(page, stack);
  await page.goto(stack.baseURL + '/devices/new');
  await page.getByLabel('Name', { exact: true }).fill('bad-router');
  await page.locator('#dev-url').fill(stack.routerURL);
  await page.locator('#dev-user').fill(ROUTER_USER);
  await page.locator('#dev-pass').fill('wrong-router-password');
  await page.locator('#dev-tls').selectOption('insecure');
  await page.getByRole('button', { name: 'Add device' }).click();
  await page.getByRole('button', { name: 'Test connection' }).click();
  await expect(page.locator('#test-result.test-failed')).toContainText('rejected the user or the password');
});
