// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

import { test, expect, login } from '../fixtures';

test('create a token, see the secret once, revoke it', async ({ page, stack }) => {
  await login(page, stack);
  await page.goto(stack.baseURL + '/tokens');

  await page.getByLabel('Name', { exact: true }).fill('ci-token');
  await page.getByRole('button', { name: 'Create token' }).click();

  await expect(page.getByRole('heading', { name: 'Token created' })).toBeVisible();
  const secret = (await page.locator('#token-secret').innerText()).trim();
  expect(secret.length).toBeGreaterThan(20);

  // Without the token the API rejects.
  const noAuth = await page.request.get(stack.baseURL + '/api/v1/messages');
  expect(noAuth.status()).toBe(401);

  // The secret works against the REST API...
  const ok = await page.request.get(stack.baseURL + '/api/v1/messages', { headers: { Authorization: `Bearer ${secret}` } });
  expect(ok.status()).toBe(200);

  // ...and is not shown again.
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#tokens')).toContainText('ci-token');
  await expect(page.locator('body')).not.toContainText(secret);
  await page.reload();
  await expect(page.locator('body')).not.toContainText(secret);

  await page.locator('#tokens tr', { hasText: 'ci-token' }).getByRole('button', { name: 'Revoke' }).click();
  await page.getByRole('button', { name: 'Revoke' }).click();
  await expect(page.getByText('Token revoked.')).toBeVisible();
  await expect(page.locator('#tokens')).toHaveCount(0);

  const denied = await page.request.get(stack.baseURL + '/api/v1/messages', { headers: { Authorization: `Bearer ${secret}` } });
  expect(denied.status()).toBe(401);
});
