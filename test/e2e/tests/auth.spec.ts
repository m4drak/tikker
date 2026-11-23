// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

import { test, expect, login, ADMIN_USER, ADMIN_PASSWORD } from '../fixtures';

test.describe('first run', () => {
  test('wizard creates the first admin, then login works', async ({ page, freshStack }) => {
    await page.goto(freshStack.baseURL + '/');
    await expect(page).toHaveURL(/\/setup$/);
    await expect(page.getByRole('heading', { name: 'Create the administrator' })).toBeVisible();

    await page.getByLabel('Username').fill('owner');
    await page.getByLabel('Password', { exact: true }).fill(ADMIN_PASSWORD);
    await page.getByLabel('Repeat the password').fill(ADMIN_PASSWORD);
    await page.getByRole('button', { name: 'Create administrator' }).click();

    // The wizard signs the new admin in; after signing out, login works.
    await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible();
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page).toHaveURL(/\/login/);
    await login(page, freshStack, 'owner');
    // The wizard is gone once a user exists.
    const res = await page.goto(freshStack.baseURL + '/setup');
    expect(res?.status()).toBe(404);
  });

  test('wizard rejects mismatching passwords', async ({ page, freshStack }) => {
    await page.goto(freshStack.baseURL + '/setup');
    await page.getByLabel('Username').fill('owner');
    await page.getByLabel('Password', { exact: true }).fill(ADMIN_PASSWORD);
    await page.getByLabel('Repeat the password').fill(ADMIN_PASSWORD + 'x');
    await page.getByRole('button', { name: 'Create administrator' }).click();
    await expect(page.locator('#confirm-error')).toContainText('do not match');
    await expect(page).toHaveURL(/\/setup$/);
  });
});

test.describe('login', () => {
  test('anonymous user is redirected to the login page', async ({ page, stack }) => {
    await page.goto(stack.baseURL + '/messages');
    await expect(page).toHaveURL(/\/login/);
  });

  test('valid credentials sign in, sign out ends the session', async ({ page, stack }) => {
    await login(page, stack);
    await expect(page.locator('li.who')).toContainText(ADMIN_USER);
    await page.getByRole('button', { name: 'Sign out' }).click();
    await expect(page).toHaveURL(/\/login/);
    await page.goto(stack.baseURL + '/messages');
    await expect(page).toHaveURL(/\/login/);
  });

  test('wrong password shows an error', async ({ page, stack }) => {
    await page.goto(stack.baseURL + '/login');
    await page.getByLabel('Username').fill(ADMIN_USER);
    await page.getByLabel('Password').fill('definitely-wrong-password');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.locator('#login-error')).toContainText('Invalid username or password');
    await expect(page.getByRole('button', { name: 'Sign out' })).toHaveCount(0);
  });
});
