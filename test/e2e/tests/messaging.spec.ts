// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

import { test, expect, login, addDevice } from '../fixtures';

test('send an SMS: live segment preview, queued -> sent, seen by the fake router', async ({ page, stack }) => {
  await login(page, stack);
  await addDevice(page, stack);

  await page.goto(stack.baseURL + '/send');
  await page.getByLabel('Recipient number').fill('+48123456789');

  // Live preview (htmx, debounced): GSM-7, 1 segment, then 2 segments.
  await page.getByLabel('Text', { exact: true }).fill('Hello from e2e');
  await expect(page.locator('#pv-chars')).toHaveText('14');
  await expect(page.locator('#pv-encoding')).toContainText('GSM');
  await expect(page.locator('#pv-segments')).toHaveText('1');
  await page.getByLabel('Text', { exact: true }).fill('x'.repeat(170));
  await expect(page.locator('#pv-chars')).toHaveText('170');
  await expect(page.locator('#pv-segments')).toHaveText('2');

  const text = 'Hello from e2e';
  await page.getByLabel('Text', { exact: true }).fill(text);
  await expect(page.locator('#pv-chars')).toHaveText(String(text.length));
  await page.getByRole('button', { name: 'Send' }).click();

  await expect(page).toHaveURL(/\/messages/);
  await expect(page.getByText('The message is queued for sending.')).toBeVisible();

  // The dispatcher picks it up: the row reaches "sent" (reload until then).
  await expect
    .poll(
      async () => {
        await page.goto(stack.baseURL + '/messages?direction=out');
        return page.locator('#messages tbody tr').first().innerText();
      },
      { timeout: 30_000 },
    )
    .toMatch(/\bsent\b/i);

  // The fake router really got the SMS.
  await expect
    .poll(async () => (await stack.fakeSent()).length, { timeout: 15_000 })
    .toBe(1);
  const [sent] = await stack.fakeSent();
  expect(sent.phone_number).toContain('48123456789');
  expect(sent.port).toBe('lte1');
  expect(sent.message).toBe(text);
});

test('an SMS injected into the router appears as incoming', async ({ page, stack }) => {
  await login(page, stack);
  await addDevice(page, stack);

  await stack.inject('+48600700800', 'Incoming e2e message');

  await expect
    .poll(
      async () => {
        await page.goto(stack.baseURL + '/messages?direction=in');
        return page.locator('#messages').innerText().catch(() => '');
      },
      { timeout: 30_000 },
    )
    .toContain('Incoming e2e message');
  await expect(page.locator('#messages')).toContainText('600700800');
  await expect(page.locator('#messages')).toContainText('received');
});
