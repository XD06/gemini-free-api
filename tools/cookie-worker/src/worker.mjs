import { chromium } from 'playwright';
import fs from 'node:fs/promises';
import path from 'node:path';
import { spawn } from 'node:child_process';

await loadDotEnv(path.resolve('..', '..', '.env'));
applyLifecycleDefaults();

const apiBase = trimTrailingSlash(process.env.API_BASE || 'http://127.0.0.1:8787');
const syncToken = process.env.COOKIE_SYNC_TOKEN || '';
const accounts = loadAccounts();
const minMinutes = parseInt(process.env.COOKIE_WORKER_MIN_INTERVAL_MINUTES || '90', 10);
const maxMinutes = parseInt(process.env.COOKIE_WORKER_MAX_INTERVAL_MINUTES || '180', 10);
const headless = parseBool(process.env.COOKIE_WORKER_HEADLESS || 'false');
const browserChannel = process.env.COOKIE_WORKER_BROWSER_CHANNEL ?? 'chrome';
const once = parseBool(process.env.COOKIE_WORKER_ONCE || 'false');
const openOnly = parseBool(process.env.COOKIE_WORKER_OPEN_ONLY || 'false');

if (!syncToken && !openOnly) {
  throw new Error('COOKIE_SYNC_TOKEN is required');
}
if (accounts.length === 0) {
  throw new Error('no accounts configured; set GEMINI_ACCOUNTS and GEMINI_ACCOUNT_<ID>_PROFILE_DIR');
}

for (let iteration = 0; ; iteration++) {
  for (const account of accounts) {
    await refreshAccount(account).catch((err) => {
      console.error(JSON.stringify({
        level: 'warn',
        msg: 'account refresh failed',
        account: account.id,
        stage: err.stage || 'unknown',
        error: err.message,
        cause: err.cause?.message,
      }));
    });
  }
  if (once || openOnly) {
    break;
  }
  const delayMs = randomBetween(minMinutes, maxMinutes) * 60_000;
  console.log(JSON.stringify({ level: 'info', msg: 'sleep', delay_ms: delayMs }));
  await sleep(delayMs);
}

async function refreshAccount(account) {
  if (!account.profileDir) {
    throw new Error(`missing profile dir for account ${account.id}`);
  }
  await fs.mkdir(account.profileDir, { recursive: true });

  if (openOnly) {
    await openSystemChromeForLogin(account);
    return;
  }

  const launchOptions = {
    headless,
    channel: browserChannel || undefined,
  };
  if (account.proxy) {
    launchOptions.proxy = { server: normalizeProxy(account.proxy) };
  }

  let context;
  try {
    context = await chromium.launchPersistentContext(account.profileDir, launchOptions);
  } catch (err) {
    throw withStage(err, 'launch_browser');
  }
  try {
    const page = context.pages()[0] || await context.newPage();
    try {
      await page.goto('https://gemini.google.com/app', { waitUntil: 'domcontentloaded', timeout: 60_000 });
    } catch (err) {
      throw withStage(err, 'open_gemini');
    }
    await page.waitForTimeout(5000);

    let cookies;
    try {
      cookies = await context.cookies([
        'https://google.com',
        'https://www.google.com',
        'https://gemini.google.com',
        'https://accounts.google.com',
      ]);
    } catch (err) {
      throw withStage(err, 'read_cookies');
    }
    const psid = cookies.find((cookie) => cookie.name === '__Secure-1PSID')?.value || '';
    const psidts = cookies.find((cookie) => cookie.name === '__Secure-1PSIDTS')?.value || '';
    if (!psid) {
      throw new Error('profile is not logged in or __Secure-1PSID is missing');
    }

    await postCookies(account.id, psid, psidts);
    console.log(JSON.stringify({
      level: 'info',
      msg: 'account cookies synced',
      account: account.id,
      has_psidts: Boolean(psidts),
    }));
  } finally {
    if (context) {
      await context.close();
    }
  }
}

async function postCookies(accountID, secure1PSID, secure1PSIDTS) {
  const url = `${apiBase}/admin/accounts/${encodeURIComponent(accountID)}/cookies`;
  let res;
  try {
    res = await fetch(url, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'authorization': `Bearer ${syncToken}`,
      },
      body: JSON.stringify({
        secure_1psid: secure1PSID,
        secure_1psidts: secure1PSIDTS,
        source: 'playwright-cookie-worker',
        observed_at: Math.floor(Date.now() / 1000),
      }),
    });
  } catch (err) {
    throw withStage(new Error(`POST ${url} failed`, { cause: err }), 'post_admin');
  }
  if (!res.ok) {
    const text = await res.text();
    throw withStage(new Error(`cookie sync failed with status ${res.status}: ${text.slice(0, 500)}`), 'post_admin');
  }
}

function withStage(err, stage) {
  err.stage = stage;
  return err;
}

function loadAccounts() {
  const onlyAccount = (process.env.COOKIE_WORKER_ACCOUNT || '').trim();
  const ids = (process.env.GEMINI_ACCOUNTS || '')
    .split(',')
    .map((id) => id.trim())
    .filter(Boolean)
    .filter((id) => !onlyAccount || id === onlyAccount);
  return ids.map((id) => {
    const key = envAccountKey(id);
    return {
      id,
      profileDir: process.env[`GEMINI_ACCOUNT_${key}_PROFILE_DIR`] || path.join('profiles', id),
      proxy: process.env[`GEMINI_ACCOUNT_${key}_PROXY`] || '',
    };
  });
}

function envAccountKey(id) {
  return id.trim().toUpperCase().replace(/[^A-Z0-9]/g, '_');
}

function randomBetween(min, max) {
  const low = Number.isFinite(min) && min > 0 ? min : 90;
  const high = Number.isFinite(max) && max >= low ? max : low;
  return Math.floor(low + Math.random() * (high - low + 1));
}

function parseBool(value) {
  return ['1', 'true', 'yes', 'on'].includes(String(value).trim().toLowerCase());
}

function trimTrailingSlash(value) {
  return value.replace(/\/+$/, '');
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function loadDotEnv(filePath) {
  let text = '';
  try {
    text = await fs.readFile(filePath, 'utf8');
  } catch {
    return;
  }
  for (const rawLine of text.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith('#')) {
      continue;
    }
    const eq = line.indexOf('=');
    if (eq <= 0) {
      continue;
    }
    const key = line.slice(0, eq).trim();
    let value = line.slice(eq + 1).trim();
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1);
    }
    if (process.env[key] === undefined) {
      process.env[key] = value;
    }
  }
}

function applyLifecycleDefaults() {
  const event = process.env.npm_lifecycle_event || '';
  if (event === 'login' && process.env.COOKIE_WORKER_OPEN_ONLY === undefined) {
    process.env.COOKIE_WORKER_OPEN_ONLY = 'true';
  }
  if ((event === 'sync' || event === 'once') && process.env.COOKIE_WORKER_ONCE === undefined) {
    process.env.COOKIE_WORKER_ONCE = 'true';
  }
}

function chromeProxyArg(proxy) {
  const value = normalizeProxy(proxy);
  if (!value) {
    return '';
  }
  if (value.startsWith('http://') || value.startsWith('https://')) {
    return `http=${value};https=${value}`;
  }
  return value;
}

function normalizeProxy(proxy) {
  return String(proxy || '').trim().replace(/\/+$/, '');
}

async function waitForManualClose(context) {
  while (context.pages().length > 0) {
    await sleep(1000);
  }
}

async function openSystemChromeForLogin(account) {
  const chromePath = await findChromeExecutable();
  const profileDir = path.resolve(account.profileDir);
  const args = [
    `--user-data-dir=${profileDir}`,
    '--no-first-run',
    '--no-default-browser-check',
  ];
  if (account.proxy) {
    args.push(`--proxy-server=${chromeProxyArg(account.proxy)}`);
  }
  args.push('https://gemini.google.com/app');

  console.log(JSON.stringify({
    level: 'info',
    msg: 'opening system Chrome for manual login',
    account: account.id,
    chrome_path: chromePath,
    profile_dir: profileDir,
  }));

  await new Promise((resolve, reject) => {
    const child = spawn(chromePath, args, { stdio: 'inherit' });
    child.on('error', reject);
    child.on('exit', (code) => {
      if (code && code !== 0) {
        reject(new Error(`Chrome exited with code ${code}`));
        return;
      }
      resolve();
    });
  });
}

async function findChromeExecutable() {
  if (process.env.CHROME_PATH) {
    return process.env.CHROME_PATH;
  }
  const candidates = [
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
    path.join(process.env.LOCALAPPDATA || '', 'Google\\Chrome\\Application\\chrome.exe'),
  ].filter(Boolean);
  for (const candidate of candidates) {
    try {
      await fs.access(candidate);
      return candidate;
    } catch {
      // try next
    }
  }
  return 'chrome';
}
