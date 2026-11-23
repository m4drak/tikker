// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

import { test as base, expect, type Page } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtempSync, rmSync, existsSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import path from 'node:path';

export const ADMIN_USER = 'admin';
export const ADMIN_PASSWORD = 'change-me-please-123';
export const ROUTER_USER = 'tikker';
export const ROUTER_PASSWORD = 'tikker-test';

const repoRoot = path.resolve(__dirname, '../..');

export interface Stack {
  baseURL: string;
  routerURL: string; // REST URL of fake-routeros as seen by tikker
  controlURL: string; // /_fake/ control API
  dataDir: string;
  fakeSent(): Promise<Array<{ seq: number; port: string; phone_number: string; message: string }>>;
  inject(from: string, text: string, port?: string): Promise<void>;
}

async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address() as { port: number };
      srv.close(() => resolve(port));
    });
  });
}

function binary(name: string): string {
  const p = path.join(repoRoot, 'bin', name);
  if (!existsSync(p)) throw new Error(`${p} not found; run "make build" first`);
  return p;
}

function start(cmd: string, args: string[], env: NodeJS.ProcessEnv, log: string[]): ChildProcess {
  const child = spawn(cmd, args, { env, stdio: ['ignore', 'pipe', 'pipe'] });
  const keep = (b: Buffer) => { log.push(b.toString()); if (log.length > 200) log.shift(); };
  child.stdout?.on('data', keep);
  child.stderr?.on('data', keep);
  return child;
}

async function stop(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise<void>((r) => child.once('exit', () => r()));
  child.kill('SIGTERM');
  await exited;
}

async function waitReady(url: string, auth: string | undefined, log: string[], procs: ChildProcess[]): Promise<void> {
  await expect
    .poll(
      async () => {
        const dead = procs.find((p) => p.exitCode !== null);
        if (dead) throw new Error(`process exited with ${dead.exitCode}:\n${log.join('')}`);
        try {
          const res = await fetch(url, { headers: auth ? { Authorization: auth } : {} });
          return res.status;
        } catch {
          return 0;
        }
      },
      { message: `waiting for ${url}`, timeout: 30_000, intervals: [50, 100, 250] },
    )
    .toBe(200);
}

async function startStack(withAdmin: boolean): Promise<{ stack: Stack; close: () => Promise<void> }> {
  const [tikkerPort, fakePort] = [await freePort(), await freePort()];
  const dataDir = mkdtempSync(path.join(tmpdir(), 'tikker-e2e-'));
  const log: string[] = [];

  const fake = start(
    binary('fake-routeros'),
    ['--listen', `127.0.0.1:${fakePort}`, '--control', '--ports', 'lte1,lte2', '--user', ROUTER_USER, '--password', ROUTER_PASSWORD],
    process.env,
    log,
  );
  const env: NodeJS.ProcessEnv = {
    PATH: process.env.PATH,
    TIKKER_DATA_DIR: path.join(dataDir, 'data'),
    TIKKER_LISTEN: `127.0.0.1:${tikkerPort}`,
    TIKKER_LOG_FORMAT: 'text',
    TIKKER_ALLOWED_HTTP_HOSTS: '127.0.0.1',
    TIKKER_HEALTH_INTERVAL: '1s',
    TIKKER_INBOX_POLL_INTERVAL: '1s',
  };
  if (withAdmin) env.TIKKER_ADMIN_PASSWORD = ADMIN_PASSWORD;
  const tikker = start(binary('tikker'), ['serve'], env, log);

  const close = async () => {
    await stop(tikker);
    await stop(fake);
    rmSync(dataDir, { recursive: true, force: true });
  };
  try {
    const routerURL = `http://127.0.0.1:${fakePort}`;
    const basic = 'Basic ' + Buffer.from(`${ROUTER_USER}:${ROUTER_PASSWORD}`).toString('base64');
    await waitReady(`${routerURL}/rest/system/resource`, basic, log, [fake, tikker]);
    // /healthz proves liveness; /readyz stays 503 until a channel exists.
    await waitReady(`http://127.0.0.1:${tikkerPort}/healthz`, undefined, log, [fake, tikker]);
    const controlURL = `${routerURL}/_fake`;
    const stack: Stack = {
      baseURL: `http://127.0.0.1:${tikkerPort}`,
      routerURL,
      controlURL,
      dataDir,
      async fakeSent() {
        const res = await fetch(`${controlURL}/sent`);
        const body = await res.json();
        return Array.isArray(body) ? body : (body.sent ?? []);
      },
      async inject(from, text, port = 'lte1') {
        const res = await fetch(`${controlURL}/inject`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ port, from, text }),
        });
        if (!res.ok) throw new Error(`inject failed: ${res.status} ${await res.text()}`);
      },
    };
    return { stack, close };
  } catch (e) {
    await close();
    throw e;
  }
}

type Fixtures = { stack: Stack; freshStack: Stack };

export const test = base.extend<Fixtures>({
  // Stack with the bootstrap admin (TIKKER_ADMIN_PASSWORD).
  stack: async ({}, use) => {
    const s = await startStack(true);
    await use(s.stack);
    await s.close();
  },
  // Stack with an empty database and no bootstrap admin: first-run wizard.
  freshStack: async ({}, use) => {
    const s = await startStack(false);
    await use(s.stack);
    await s.close();
  },
});

export { expect };

export async function login(page: Page, stack: Stack, username = ADMIN_USER, password = ADMIN_PASSWORD): Promise<void> {
  await page.goto(`${stack.baseURL}/login`);
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible();
}

// Adds the fake router as a device and its lte1 port as a channel through the UI.
export async function addDevice(page: Page, stack: Stack, name = 'fake-router'): Promise<void> {
  await page.goto(`${stack.baseURL}/devices/new`);
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.locator('#dev-url').fill(stack.routerURL);
  await page.locator('#dev-user').fill(ROUTER_USER);
  await page.locator('#dev-pass').fill(ROUTER_PASSWORD);
  await page.locator('#dev-tls').selectOption('insecure');
  await page.getByRole('button', { name: 'Add device' }).click();
  await expect(page.getByRole('heading', { name })).toBeVisible();
  await page.getByRole('button', { name: 'Test connection' }).click();
  await expect(page.locator('#test-ports')).toContainText('lte1');
  await page.locator('#test-ports tr', { hasText: 'lte1' }).getByRole('button', { name: /add/i }).click();
  await expect(page.locator('#channels')).toContainText('lte1');
}
