import { chromium } from 'playwright';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';

const workerDir = path.dirname(fileURLToPath(import.meta.url));
await loadDotEnv(path.resolve(workerDir, '..', '.env'));
await loadDotEnv(path.resolve(workerDir, '..', '..', '.env'));
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
const force = parseBool(process.env.COOKIE_WORKER_FORCE || 'false');
const postRetries = parseInt(process.env.COOKIE_WORKER_POST_RETRIES || '3', 10);
const postRetryDelayMs = parseInt(process.env.COOKIE_WORKER_POST_RETRY_DELAY_MS || '2000', 10);
// Opt-in CDP mode: attach to a Chrome that is already running, read cookies
// from its existing login, then exit. `COOKIE_WORKER_CDP_URL=auto` reuses the
// default Chrome profile's DevToolsActivePort (Chrome 144+ remote debugging
// toggle at chrome://inspect/#remote-debugging); an explicit http:// or ws://
// endpoint keeps working for a Chrome launched with --remote-debugging-port.
// Leaving COOKIE_WORKER_CDP_URL empty keeps the original persistent-profile flow.
const cdpEndpointSetting = (process.env.COOKIE_WORKER_CDP_URL || '').trim();
const cdpProfileDir = (process.env.COOKIE_WORKER_CDP_PROFILE_DIR || '').trim();
const cdpMode = Boolean(cdpEndpointSetting);
const cdpCreateAccount = parseBool(process.env.COOKIE_WORKER_CREATE_ACCOUNT || 'false');

if (!syncToken && !openOnly) {
  throw new Error('COOKIE_SYNC_TOKEN is required');
}
if (cdpMode && !syncToken) {
  throw new Error('COOKIE_SYNC_TOKEN is required in CDP mode');
}
if (accounts.length === 0) {
  throw new Error('no accounts configured; set COOKIE_WORKER_ACCOUNT (CDP mode) or GEMINI_ACCOUNTS and GEMINI_ACCOUNT_<ID>_PROFILE_DIR');
}

if (cdpMode) {
  await runCDPMode(accounts);
} else {
  for (let iteration = 0; ; iteration++) {
    const statusByAccount = openOnly || force ? new Map() : await loadRemoteAccountStatuses();
    for (const account of accounts) {
      if (!openOnly && !force && shouldSkipAccountRefresh(account, statusByAccount)) {
        continue;
      }
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
}

// CDP mode is a one-shot sync: it reads the already-logged-in browser rather
// than driving its own profile. It exits explicitly because the CDP websocket
// keeps the Node event loop alive, and browser.close() must not be used.
async function runCDPMode(targets) {
  let endpoint;
  try {
    endpoint = await resolveCDPEndpoint(cdpEndpointSetting);
  } catch (err) {
    console.error(JSON.stringify({
      level: 'error',
      msg: 'cannot resolve Chrome CDP endpoint',
      error: err.message,
    }));
    await sleep(50);
    process.exit(1);
  }

  console.log(JSON.stringify({
    level: 'info',
    msg: 'cookie worker CDP mode',
    endpoint,
    accounts: targets.map((account) => account.id),
    create_account: cdpCreateAccount,
  }));

  let failed = false;
  for (const account of targets) {
    try {
      await syncAccountViaCDP(account, endpoint);
    } catch (err) {
      failed = true;
      console.error(JSON.stringify({
        level: 'warn',
        msg: 'cdp account sync failed',
        account: account.id,
        stage: err.stage || 'unknown',
        error: err.message,
        cause: err.cause?.message,
      }));
    }
  }

  console.log(JSON.stringify({ level: 'info', msg: 'cdp sync finished', failed }));
  await sleep(50);
  process.exit(failed ? 1 : 0);
}

async function resolveCDPEndpoint(raw) {
  if (raw && raw.toLowerCase() !== 'auto') {
    return raw;
  }
  const userDataDir = cdpProfileDir || defaultChromeUserDataDir();
  const portFile = path.join(userDataDir, 'DevToolsActivePort');
  let content;
  try {
    content = await fs.readFile(portFile, 'utf8');
  } catch (err) {
    throw new Error(`cannot read ${portFile}; open chrome://inspect/#remote-debugging in Chrome 144+ and enable remote debugging, or set COOKIE_WORKER_CDP_URL explicitly`, { cause: err });
  }
  const [rawPort, wsPath] = content
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
  const port = Number.parseInt(rawPort, 10);
  if (!Number.isInteger(port) || port <= 0 || port > 65535 || !wsPath) {
    throw new Error(`invalid DevToolsActivePort content in ${portFile}`);
  }
  return `ws://127.0.0.1:${port}${wsPath}`;
}

function defaultChromeUserDataDir() {
  if (process.platform === 'win32') {
    const localAppData = process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local');
    return path.join(localAppData, 'Google', 'Chrome', 'User Data');
  }
  if (process.platform === 'darwin') {
    return path.join(os.homedir(), 'Library', 'Application Support', 'Google', 'Chrome');
  }
  return path.join(os.homedir(), '.config', 'google-chrome');
}

async function syncAccountViaCDP(account, endpoint) {
  const { psid, psidts } = await readCookiesFromRunningChrome(endpoint);
  if (!psid) {
    throw withStage(new Error('the attached Chrome is not logged in to gemini.google.com, or __Secure-1PSID is missing'), 'read_cookies');
  }

  const statusByAccount = await loadRemoteAccountStatuses();
  if (statusByAccount.has(account.id)) {
    await postCookiesWithRetry(account.id, psid, psidts);
    console.log(JSON.stringify({
      level: 'info',
      msg: 'account cookies synced from attached browser',
      account: account.id,
      has_psidts: Boolean(psidts),
    }));
    return;
  }

  if (!cdpCreateAccount) {
    throw new Error(`account ${account.id} does not exist in the main service; set COOKIE_WORKER_CREATE_ACCOUNT=true to create it from the attached browser`);
  }
  await createAccountWithRetry(account.id, psid, psidts);
  console.log(JSON.stringify({
    level: 'info',
    msg: 'account created from attached browser',
    account: account.id,
    has_psidts: Boolean(psidts),
  }));
}

async function readCookiesFromRunningChrome(endpoint) {
  let browser;
  try {
    browser = await chromium.connectOverCDP(endpoint, { timeout: 15_000 });
  } catch (err) {
    throw withStage(new Error(`cannot connect to Chrome at ${endpoint}; enable chrome://inspect/#remote-debugging (Chrome 144+), or start Chrome with --remote-debugging-port=<port> and a separate --user-data-dir`, { cause: err }), 'connect_cdp');
  }

  // Intentionally no browser.close(): over CDP Playwright sends Browser.close,
  // which would terminate the user's Chrome. runCDPMode exits the process instead.
  const context = browser.contexts()[0];
  if (!context) {
    throw withStage(new Error('attached Chrome has no default browser context'), 'connect_cdp');
  }

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
  console.log(JSON.stringify({
    level: 'info',
    msg: 'cookies read from attached browser',
    has_psid: Boolean(psid),
    has_psidts: Boolean(psidts),
    cookie_count: cookies.length,
  }));
  return { psid, psidts };
}

async function createAccountWithRetry(accountID, secure1PSID, secure1PSIDTS) {
  const attempts = Number.isFinite(postRetries) && postRetries >= 0 ? postRetries + 1 : 4;
  const delayMs = Number.isFinite(postRetryDelayMs) && postRetryDelayMs >= 0 ? postRetryDelayMs : 2000;
  let lastErr;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      await createAccount(accountID, secure1PSID, secure1PSIDTS);
      return;
    } catch (err) {
      lastErr = err;
      if (attempt >= attempts) {
        break;
      }
      await sleep(delayMs);
    }
  }
  throw lastErr;
}

async function createAccount(accountID, secure1PSID, secure1PSIDTS) {
  // No per-account proxy: the browser's own egress is unknown here, so leave it
  // empty and let the main service inherit its global PROXY_URL.
  const url = `${apiBase}/admin/accounts`;
  let res;
  try {
    res = await fetch(url, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'authorization': `Bearer ${syncToken}`,
      },
      body: JSON.stringify({
        account_id: accountID,
        secure_1psid: secure1PSID,
        secure_1psidts: secure1PSIDTS,
      }),
    });
  } catch (err) {
    throw withStage(new Error(`POST ${url} failed`, { cause: err }), 'post_admin');
  }
  if (!res.ok) {
    const text = await res.text();
    throw withStage(new Error(`account create failed with status ${res.status}: ${text.slice(0, 500)}`), 'post_admin');
  }
}

async function loadRemoteAccountStatuses() {
  const url = `${apiBase}/admin/accounts`;
  let res;
  try {
    res = await fetch(url, {
      method: 'GET',
      headers: {
        'authorization': `Bearer ${syncToken}`,
      },
    });
  } catch (err) {
    throw withStage(new Error(`GET ${url} failed`, { cause: err }), 'load_remote_status');
  }
  if (!res.ok) {
    const text = await res.text();
    throw withStage(new Error(`account status failed with status ${res.status}: ${text.slice(0, 500)}`), 'load_remote_status');
  }
  const body = await res.json();
  const statuses = new Map();
  for (const status of body.accounts || []) {
    if (!status || !status.id) {
      continue;
    }
    statuses.set(String(status.id), status);
  }
  return statuses;
}

function shouldSkipAccountRefresh(account, statusByAccount) {
  const status = statusByAccount.get(account.id);
  if (!status) {
    console.log(JSON.stringify({
      level: 'info',
      msg: 'remote account status missing, refreshing',
      account: account.id,
    }));
    return false;
  }
  const state = String(status.state || '').trim().toLowerCase();
  if (state === 'healthy' || state === 'refreshing') {
    console.log(JSON.stringify({
      level: 'info',
      msg: 'remote account healthy, skip browser sync',
      account: account.id,
      state,
      last_validated: status.last_validated || '',
      last_cookie_sync: status.last_cookie_sync || '',
    }));
    return true;
  }
  console.log(JSON.stringify({
    level: 'info',
    msg: 'remote account needs cookie sync',
    account: account.id,
    state: state || 'unknown',
    last_error: status.last_error || '',
  }));
  return false;
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
  console.log(JSON.stringify({
    level: 'info',
    msg: 'cookie worker browser launch',
    account: account.id,
    proxy_enabled: Boolean(account.proxy),
    proxy: redactProxy(account.proxy),
    headless,
  }));

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

    await postCookiesWithRetry(account.id, psid, psidts);
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

async function postCookiesWithRetry(accountID, secure1PSID, secure1PSIDTS) {
  const attempts = Number.isFinite(postRetries) && postRetries >= 0 ? postRetries + 1 : 4;
  const delayMs = Number.isFinite(postRetryDelayMs) && postRetryDelayMs >= 0 ? postRetryDelayMs : 2000;
  let lastErr;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      await postCookies(accountID, secure1PSID, secure1PSIDTS);
      if (attempt > 1) {
        console.log(JSON.stringify({ level: 'info', msg: 'account cookie post retry succeeded', account: accountID, attempt }));
      }
      return;
    } catch (err) {
      lastErr = err;
      if (attempt >= attempts) {
        break;
      }
      console.error(JSON.stringify({
        level: 'warn',
        msg: 'account cookie post failed, retrying',
        account: accountID,
        attempt,
        next_delay_ms: delayMs,
        error: err.message,
        cause: err.cause?.message,
      }));
      await sleep(delayMs);
    }
  }
  throw lastErr;
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
  // COOKIE_WORKER_ACCOUNT is authoritative so CDP mode can onboard an account
  // that is not declared in GEMINI_ACCOUNTS yet.
  const ids = onlyAccount
    ? [onlyAccount]
    : (process.env.GEMINI_ACCOUNTS || '')
      .split(',')
      .map((id) => id.trim())
      .filter(Boolean);
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
  if (event === 'login') {
    process.env.COOKIE_WORKER_OPEN_ONLY = 'true';
    process.env.COOKIE_WORKER_ONCE = 'true';
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

function redactProxy(proxy) {
  const value = normalizeProxy(proxy);
  if (!value) {
    return '';
  }
  try {
    const parsed = new URL(value);
    if (parsed.username || parsed.password) {
      parsed.username = '***';
      parsed.password = '';
    }
    return parsed.toString().replace(/\/$/, '');
  } catch {
    return '[invalid]';
  }
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
