// Delta in a browser: the chats, Files, Tablet, QA and Accounts, laid out
// like the Codex app. It is served by the command center and
// talks to it on the same origin, with the token it asks for at sign-in.
//
// Two kinds of session. Regular ones (●) are cloud sessions
// on the user's accounts, reached through the command center's cloud relay
// (/cloud-accounts/{id}/api/...), which adds the account's access token: the
// provider's session routes can't be called from a browser, and this way the
// page never holds a cloud login. Backup ones (▲) are the command center's
// own (/agent-sessions). Both streams use the same frame format.
//
// White-labelled: nothing here names the agent or the model. What a new
// session is told (SETUP_BLOCK) is word for word what a backup session is
// told (agentSetup in agents.go), and SETUP_SCRIPT is defaultAgentSetup
// there; keep them in step. The environment's name hashes SETUP_SCRIPT, so
// changing the script makes every account a new environment.
'use strict';

(() => {
  // ---------------------------------------------------------------------
  // Constants shared with the command center
  // ---------------------------------------------------------------------

  const SETUP_BLOCK = '<delta-setup>\n' +
    'You were started from the Delta website. In it you are Delta: ' +
    'refer to yourself as Delta, and do not bring up the underlying model, its maker or the tool you run in ' +
    'unless the user asks.\n\n' +
    'Before you start on their message, run `delta help` and read all of its output: it explains Delta, ' +
    'how to get files to the user, and how changes to the apps on their phone reach it. Do this quietly: do not announce, ' +
    'narrate or summarize it, and do not mention it unless it fails in a way that affects their request. ' +
    'Your first words to the user should be about their message.\n' +
    '</delta-setup>';
  const SETUP_SCRIPT = '#!/bin/bash\nset -eo pipefail\ncurl -fsSL https://github.com/thooton/delta/releases/download/latest/install.sh | sh\n';
  const FILES_OPEN = '<delta-files>';
  const FILES_CLOSE = '</delta-files>';
  const CARRY_OPEN = '<delta-continued>';
  const CARRY_CLOSE = '</delta-continued>';
  const CARRY_NOTE = 'Continued from an earlier session on another account.';

  const BETA = 'ccr-byoc-2025-07-29';
  const CLOUD_KIND = 'anthropic_cloud';
  const MODE = 'auto';
  const MARK = { cloud: '●', backup: '▲' };
  const KIND_NAME = { cloud: 'Regular', backup: 'Backup' };
  const MAX_TITLE = 60;
  const MAX_PICTURES = 4;
  const MAX_FILES = 10;
  const PICTURE_SIZE = 1568;

  // ---------------------------------------------------------------------
  // Small helpers
  // ---------------------------------------------------------------------

  const $ = (s) => document.querySelector(s);
  const sleep = (ms, signal) => new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    if (signal) signal.addEventListener('abort', () => { clearTimeout(t); resolve(); }, { once: true });
  });

  function stored(key, value) {
    try {
      if (value === undefined) return localStorage.getItem(key);
      if (value === null) localStorage.removeItem(key);
      else localStorage.setItem(key, value);
    } catch (e) { /* private mode: nothing is remembered */ }
    return null;
  }

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') node.className = v;
      else if (k === 'html') node.innerHTML = v;
      else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v === true ? '' : v);
    }
    for (const c of children.flat()) if (c != null && c !== false) node.append(c.nodeType ? c : String(c));
    return node;
  }

  function icon(name, cls) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    if (cls) svg.setAttribute('class', cls);
    const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', '#i-' + name);
    svg.append(use);
    return svg;
  }

  /** A page's button, its words in a span; `cls` adds to its class. */
  const button = (text, onclick, cls, title) =>
    el('button', { type: 'button', class: 'button' + (cls ? ' ' + cls : ''), title, onclick }, el('span', {}, text));

  /** A row on a page: an icon, a name over a line about it (`opts.title`, `opts.small` its classes, `opts.extra` under it), and buttons. */
  function row(iconName, name, about, actions, opts = {}) {
    return el('div', { class: 'row' },
      el('span', { class: 'icon' }, icon(iconName)),
      el('span', { class: 'label' }, name instanceof Node ? name : el('b', { title: opts.title }, name),
        el('small', { class: opts.small }, about), opts.extra),
      el('span', { class: 'actions' }, actions));
  }

  /** Shows only `text` in a page's box. */
  function empty(box, text) {
    box.replaceChildren(el('div', { class: 'empty' }, text));
    return box;
  }
  const loading = (box) => { if (!box.childNodes.length) empty(box, 'Loading…'); };

  const uuid = () => (crypto.randomUUID ? crypto.randomUUID()
    : '10000000-1000-4000-8000-100000000000'.replace(/[018]/g, (c) => (c ^ (crypto.getRandomValues(new Uint8Array(1))[0] & (15 >> (c / 4)))).toString(16)));

  /** Milliseconds since the epoch from an ISO time, or seconds or milliseconds. */
  function instant(v) {
    if (typeof v === 'number') return v < 1e12 ? v * 1000 : v;
    if (typeof v === 'string' && v) { const t = Date.parse(v); return isNaN(t) ? 0 : t; }
    return 0;
  }

  function ago(ms) {
    if (!ms) return '';
    const s = Math.max(0, (Date.now() - ms) / 1000);
    if (s < 60) return 'now';
    if (s < 3600) return Math.floor(s / 60) + 'm';
    if (s < 86400) return Math.floor(s / 3600) + 'h';
    if (s < 604800) return Math.floor(s / 86400) + 'd';
    if (s < 2629800) return Math.floor(s / 604800) + 'w';
    if (s < 31557600) return Math.floor(s / 2629800) + 'mo';
    return Math.floor(s / 31557600) + 'y';
  }

  function when(ms) {
    const d = new Date(ms);
    const time = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
    if (d.toDateString() === new Date().toDateString()) return time;
    return d.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' }) + ', ' + time;
  }
  /** `when` of an ISO time, or seconds or milliseconds. */
  const dated = (v) => when(instant(v));

  function size(n) {
    if (n < 1024) return n + ' B';
    const units = ['KB', 'MB', 'GB'];
    let v = n;
    let u = -1;
    do { v /= 1024; u++; } while (v >= 1024 && u < units.length - 1);
    return (v < 10 ? v.toFixed(1) : Math.round(v)) + ' ' + units[u];
  }

  /** The app's own lines name the product as Delta; the agent's words are left alone. */
  const rebrand = (s) => String(s).replace(/\bClaude(?: Code| AI)?\b/g, 'Delta');
  const oneLine = (s, max = 160) => {
    const flat = String(s).trim().replace(/\s+/g, ' ');
    return flat.length <= max ? flat : flat.slice(0, max - 1) + '…';
  };
  const titleFor = (text) => oneLine(text.split('\n', 1)[0], MAX_TITLE);
  const cse = (id) => id.startsWith('session_') ? 'cse_' + id.slice('session_'.length) : id;
  const sessionId = (id) => id && id.startsWith('cse_') ? 'session_' + id.slice('cse_'.length) : id;
  const validId = (id) => typeof id === 'string' && /^session_[A-Za-z0-9_-]{1,120}$/.test(id);
  const isBackup = (id) => id.startsWith('session_delta_');
  const kindOf = (c) => c.backup ? 'backup' : 'cloud';
  /** A tool's name as the transcript tells tools apart. */
  const toolKey = (name) => name.replace(/^mcp__/, '').toLowerCase();

  let toastTimer = 0;
  function toast(text) {
    const t = $('#toast');
    t.textContent = text;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 4500);
  }

  // ---------------------------------------------------------------------
  // The command center
  // ---------------------------------------------------------------------

  let token = stored('delta.token');

  // The command center may sit in a subdirectory behind a proxy
  // (https://host/cc/), so every request is made relative to where the page
  // was loaded from, never from the host's root. The page always has a
  // trailing slash (index.html adds one).
  const BASE = location.pathname.replace(/[^/]*$/, '');
  const url = (path) => BASE + path.replace(/^\//, '');
  // The command center's address, as the CLI signs in to it (DELTA_SERVER).
  const SERVER = location.origin + BASE.replace(/\/$/, '');

  class HttpError extends Error {
    constructor(status, message) { super(message); this.status = status; }
  }

  const parse = (text) => { try { return text ? JSON.parse(text) : null; } catch (e) { return null; } };

  /** The error a failed response stands for, from its status and body. */
  function failure(status, text) {
    const data = parse(text);
    return new HttpError(status,
      data && typeof data.error === 'string' ? data.error
        : data && data.error && data.error.message ? rebrand(data.error.message)
          : status === 429 ? 'Too many requests; try again shortly.'
            : text && text.length < 200 && !/^\s*</.test(text) ? text.trim() : 'HTTP ' + status);
  }

  async function api(path, opts = {}) {
    const headers = { Authorization: 'Bearer ' + token, ...(opts.headers || {}) };
    let body = opts.body;
    if (opts.json !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(opts.json);
    }
    const resp = await fetch(url(path), { method: opts.method || 'GET', headers, body, signal: opts.signal });
    if (resp.status === 401 && !path.includes('/api/')) {
      signOut('Your session on the command center ended. Sign in again.');
      throw new HttpError(401, 'Not signed in');
    }
    if (opts.raw) return resp;
    const text = await resp.text();
    if (!resp.ok) throw failure(resp.status, text);
    const data = parse(text);
    return data !== null ? data : text;
  }

  // ---------------------------------------------------------------------
  // The cloud, through the relay
  // ---------------------------------------------------------------------

  const orgs = {};
  const envs = {};

  async function cloud(account, path, opts = {}) {
    const headers = { Accept: 'application/json', ...(opts.headers || {}) };
    if (opts.scoped !== false) headers['x-organization-uuid'] = await organization(account);
    if (opts.beta) headers['anthropic-beta'] = BETA;
    return api('/cloud-accounts/' + encodeURIComponent(account) + '/api' + path, { ...opts, headers });
  }

  /** Every sessions call is scoped to the login's organization. */
  function organization(account) {
    if (!orgs[account]) {
      orgs[account] = cloud(account, '/api/oauth/profile', { scoped: false })
        .then((p) => p.organization.uuid)
        .catch((e) => { delete orgs[account]; throw e; });
    }
    return orgs[account];
  }

  async function environmentName() {
    const all = SETUP_SCRIPT + '\n' + SERVER + '\n' + token;
    const sum = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(all)));
    return 'Delta ' + Array.from(sum.slice(0, 4), (b) => b.toString(16).padStart(2, '0')).join('');
  }

  /**
   * The account's cloud environment for Delta, made if there is none, found
   * by its name (see environmentName). Its setup script installs the CLI and
   * its variables sign it in here.
   */
  function environment(account) {
    if (!envs[account]) {
      envs[account] = (async () => {
        const name = await environmentName();
        const listed = await cloud(account, '/v1/environment_providers');
        for (const e of listed.environments || []) {
          if (e.kind === CLOUD_KIND && e.name === name) return e.environment_id;
        }
        const made = await cloud(account, '/v1/environment_providers/cloud/create', {
          method: 'POST',
          beta: true,
          json: {
            name,
            kind: CLOUD_KIND,
            description: 'Delta - full network access',
            config: {
              environment_type: 'anthropic',
              cwd: '/home/user',
              init_script: SETUP_SCRIPT,
              environment: { DELTA_SERVER: SERVER, DELTA_TOKEN: token },
              languages: [{ name: 'python', version: '3.11' }, { name: 'node', version: '20' }],
              network_config: { allowed_hosts: ['*'], allow_default_hosts: true },
            },
          },
        });
        return made.environment_id;
      })().catch((e) => { delete envs[account]; throw e; });
    }
    return envs[account];
  }

  const controlRequest = (request) => ({ type: 'control_request', request_id: uuid(), request, uuid: uuid() });
  const modeRequest = () => controlRequest({ subtype: 'set_permission_mode', mode: MODE });

  function userEvent(id, text, jpegs, session) {
    let content = text;
    if (jpegs.length) {
      content = jpegs.map((data) => ({ type: 'image', source: { type: 'base64', media_type: 'image/jpeg', data } }));
      if (text.trim()) content.push({ type: 'text', text });
    }
    return { uuid: id, session_id: session, type: 'user', parent_tool_use_id: null, message: { role: 'user', content } };
  }

  const eventsPath = (id) => '/v1/code/sessions/' + cse(id) + '/events';
  const postEvents = (chat, payloads) =>
    cloud(chat.account, eventsPath(chat.id), { method: 'POST', json: { events: payloads.map((payload) => ({ payload })) } });

  // ---------------------------------------------------------------------
  // Accounts and their limits
  // ---------------------------------------------------------------------

  const state = {
    accounts: [],
    chats: [],
    listed: false,
    listing: null,
    warning: '',
    view: 'new',
    kind: stored('delta.kind') === 'backup' ? 'backup' : 'cloud',
    open: null, // the chat on screen
    last: null, // the chat last on screen
    sessions: new Map(), // id -> chat, with its transcript
    attachments: [],
    busy: false, // a message or a new session is on its way
  };

  /** The chat as the sidebar lists it, if it does. */
  const findChat = (id) => state.chats.find((c) => c.id === id);

  function limits() {
    try { return JSON.parse(stored('delta.limits') || '{}'); } catch (e) { return {}; }
  }
  function noteLimit(account, until) {
    const all = limits();
    if (until > Date.now()) all[account] = until; else delete all[account];
    stored('delta.limits', JSON.stringify(all));
  }

  /** When the account has room again, if a window is used up; 0 when it has room. */
  function limitedUntil(report) {
    let until = 0;
    for (const key of ['five_hour', 'seven_day']) {
      const w = report && report[key];
      if (!w || typeof w.utilization !== 'number' || w.utilization < 100) continue;
      until = Math.max(until, instant(w.resets_at) || Date.now() + 3600000);
    }
    return until;
  }

  // The usage report lists the account's cloud credits, a dollar allowance
  // cloud sessions spend before the plan's windows, under an internal name
  // (cloudCreditsKey in cloud.go).
  const CREDITS_KEY = 'iguana_necktie';

  /** The account's cloud credits ({limit, used, left, resets}), or null if it has none. */
  function credits(report) {
    const c = report && report[CREDITS_KEY];
    if (!c || typeof c.limit_dollars !== 'number') return null;
    const used = typeof c.used_dollars === 'number' ? c.used_dollars : 0;
    const left = typeof c.remaining_dollars === 'number' ? c.remaining_dollars : Math.max(0, c.limit_dollars - used);
    return { limit: c.limit_dollars, used, left, resets: instant(c.resets_at) };
  }

  const dollars = (n) => '$' + (n >= 10 || n === 0 ? Math.round(n) : n.toFixed(2));

  async function loadAccounts() {
    state.accounts = await api('/cloud-accounts');
    return state.accounts;
  }

  /** The account's name: its email, or its place in the list. */
  function accountName(id) {
    const k = state.accounts.findIndex((a) => a.id === id);
    if (k < 0) return 'another account';
    return state.accounts[k].email || 'Account ' + (k + 1);
  }

  // What each account's usage report last said, by id: {until, limited,
  // credits, five, week, dead, unread, at}. `limited` is when a used-up
  // window resets; `until` is when the account can be used again, which is
  // 0 while cloud credits are left, as the credits carry on past the
  // windows. Menus and banners read it as it stands;
  // checkAccounts brings it up to date.
  const accountStatus = new Map();

  async function checkAccount(id) {
    let s;
    try {
      const report = await api('/cloud-accounts/' + encodeURIComponent(id) + '/usage');
      const limited = limitedUntil(report);
      const c = credits(report);
      const until = !limited || (c && c.left > 0) ? 0 : c && c.resets ? Math.min(limited, c.resets) : limited;
      noteLimit(id, until);
      const pct = (w) => w && typeof w.utilization === 'number' ? w.utilization : null;
      s = { until, limited, credits: c, five: pct(report.five_hour), week: pct(report.seven_day), report };
    } catch (e) {
      // A dead login can't be used; an unreadable report is tried anyway.
      s = e.status === 403 ? { dead: true } : { unread: true, error: e.message };
    }
    s.at = Date.now();
    accountStatus.set(id, s);
    return s;
  }

  /** Brings every account's status up to date, unless it was read in the last `fresh` ms. */
  async function checkAccounts(fresh = 0) {
    const accounts = await loadAccounts();
    await Promise.all(accounts.map((a) => {
      const s = accountStatus.get(a.id);
      return s && Date.now() - s.at < fresh ? s : checkAccount(a.id);
    }));
    return accounts;
  }

  /** When the account is out until, from its status or what was noted in this browser; 0 if it isn't known to be. */
  function outUntil(id) {
    const s = accountStatus.get(id);
    const until = Math.max((s && s.until) || 0, limits()[id] || 0);
    return until > Date.now() ? until : 0;
  }

  const usable = (id) => !(accountStatus.get(id) || {}).dead && !outUntil(id);
  const hasCredits = (id) => { const c = (accountStatus.get(id) || {}).credits; return !!c && c.left > 0; };

  /**
   * The accounts a new session could go to, best first: those with cloud
   * credits left (whether or not their windows are used up, as the credits
   * carry on past them) come first, so the credits are spent before the
   * plans' windows, then those with room in both windows; otherwise the
   * listed order.
   */
  function ranked(except) {
    const open = state.accounts.filter((a) => a.id !== except && usable(a.id));
    return open.filter((a) => hasCredits(a.id)).concat(open.filter((a) => !hasCredits(a.id)));
  }

  /** A line on how the account stands, for its menu item. */
  function standing(id) {
    const s = accountStatus.get(id);
    if (!s) return 'Checking…';
    if (s.dead) return 'Its login no longer works';
    const out = outUntil(id);
    if (out) return 'Out until ' + when(out);
    if (s.unread) return "Couldn't read its usage";
    if (s.limited > Date.now()) return 'Limits reached, running on credits: ' + dollars(s.credits.left) + ' left';
    const parts = [];
    if (s.five !== null) parts.push('5-hour ' + Math.round(s.five) + '%');
    if (s.week !== null) parts.push('weekly ' + Math.round(s.week) + '%');
    parts.push(s.credits ? dollars(s.credits.left) + ' credits left' : 'no credits');
    return parts.join(' · ');
  }

  /**
   * The account a new session goes to: `chosen` if given (it must have room),
   * otherwise the best one but `except`.
   */
  async function pickAccount(except, chosen) {
    const accounts = await checkAccounts(30000);
    if (!accounts.length) throw new Error('No accounts are set up on the command center yet. Add one with `delta accounts login`.');
    if (chosen) {
      if (!accounts.some((a) => a.id === chosen)) throw new Error('That account is no longer on the command center. Choose another.');
      await checkAccount(chosen);
      if ((accountStatus.get(chosen) || {}).dead) throw new Error(accountName(chosen) + "'s login no longer works. Choose another account.");
      const out = outUntil(chosen);
      if (out) throw new Error(accountName(chosen) + ' is at its usage limit until ' + when(out) + '. Choose another account.');
      return chosen;
    }
    const best = ranked(except)[0];
    if (best) return best.id;
    const soonest = Math.min(...accounts.filter((a) => a.id !== except).map((a) => outUntil(a.id) || Infinity));
    throw new Error((except ? 'Every other account' : 'Every account') + ' is at its usage limit' + (isFinite(soonest) ? ' until ' + when(soonest) : '') + '. A backup session (▲) works meanwhile.');
  }

  /** Whether another account than `account` isn't known to be out, so a chat can move there. */
  function canMove(account) {
    return state.accounts.some((a) => a.id !== account && usable(a.id));
  }

  // ---------------------------------------------------------------------
  // Chats
  // ---------------------------------------------------------------------

  async function listChats() {
    if (state.listing) return state.listing;
    state.listing = (async () => {
      $('#refresh').classList.add('spin');
      try {
        const accounts = await loadAccounts().catch(() => state.accounts);
        const results = await Promise.allSettled([
          api('/agent-sessions'),
          ...accounts.map((a) => cloud(a.id, '/v1/code/sessions', { scoped: false })),
        ]);
        const chats = [];
        const failed = [];
        const [backups, ...clouds] = results;
        if (backups.status === 'fulfilled') {
          for (const s of (backups.value && backups.value.data) || []) {
            if (!validId(s.id)) continue;
            chats.push({ id: s.id, title: s.title, status: s.status === 'running' ? 'running' : 'idle', updated: instant(s.updated) || instant(s.created), backup: true });
          }
        } else {
          failed.push('backup sessions');
          chats.push(...state.chats.filter((c) => c.backup));
        }
        accounts.forEach((a, k) => {
          const r = clouds[k];
          if (r.status !== 'fulfilled') {
            failed.push(a.email || 'an account');
            chats.push(...state.chats.filter((c) => c.account === a.id));
            return;
          }
          for (const s of (r.value && r.value.data) || []) {
            if (s.environment_kind && s.environment_kind !== CLOUD_KIND) continue;
            if (s.status === 'archived') continue;
            const id = sessionId(s.id);
            if (!validId(id)) continue;
            chats.push({
              id, title: s.title, status: s.worker_status || 'idle', backup: false, account: a.id,
              updated: instant(s.last_event_at) || instant(s.updated_at) || instant(s.created_at),
            });
          }
        });
        // A session just started here may not be listed yet.
        for (const c of state.chats) {
          if (c.fresh && Date.now() - c.updated < 60000 && !chats.some((x) => x.id === c.id)) chats.push(c);
        }
        for (const c of chats) {
          const open = state.sessions.get(c.id);
          if (open && open.t) c.status = open.t.pending ? 'requires_action' : open.t.working ? 'running' : c.status === 'running' && open.connected ? 'idle' : c.status;
        }
        chats.sort((a, b) => b.updated - a.updated);
        state.chats = chats;
        state.listed = true;
        const open = state.view === 'chat' && state.open && findChat(state.open.id);
        if (open) setTitle(chatTitle(open), kindOf(open));
        if (state.view === 'chat' && state.open) { if (!state.open.account && open) state.open.account = open.account; topActions(state.open); }
        state.warning = failed.length ? "Couldn't list " + failed.join(', ') + '.' : '';
      } finally {
        $('#refresh').classList.remove('spin');
        state.listing = null;
      }
      renderChats();
    })();
    return state.listing;
  }

  function chatTitle(c) {
    const t = (c.title || '').trim();
    return t || 'Untitled';
  }

  function renderChats() {
    const list = $('#chats');
    const warning = $('#chat-warning');
    warning.hidden = !state.warning;
    warning.textContent = state.warning;
    list.replaceChildren();
    if (state.listed && !state.chats.length) {
      list.append(el('li', { class: 'chats-empty' }, 'No chats yet'));
    }
    for (const c of state.chats) {
      const active = state.view === 'chat' && state.open && state.open.id === c.id;
      const kind = kindOf(c);
      const status = c.status === 'running' ? el('span', { class: 'busy', title: 'Working' })
        : c.status === 'requires_action' ? el('span', { class: 'needs', title: 'Waiting for you' })
        : el('span', { class: 'when' }, ago(c.updated));
      const more = el('button', {
        type: 'button', class: 'icon-btn small more', title: 'More',
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); chatMenu(c, e.currentTarget); },
      }, icon('more'));
      list.append(el('li', {}, el('a', {
        class: 'chat' + (active ? ' active' : ''), href: '#/s/' + c.id, title: chatTitle(c) + (c.account ? ' · on ' + accountName(c.account) : ''),
      }, el('span', { class: 'mark ' + kind }, MARK[kind]), el('span', { class: 'name' }, chatTitle(c)), status, more)));
    }
  }

  function closeMenus() {
    document.querySelectorAll('.menu.floating').forEach((m) => m.remove());
    document.querySelectorAll('.menu-open').forEach((m) => m.classList.remove('menu-open'));
    $('#kind-menu').hidden = true;
  }

  /**
   * A menu by `anchor`: items are {label, sub?, run, danger?, checked?,
   * disabled?} or {heading}. `up` opens it above the anchor, `left` lines it
   * up with the anchor's left edge rather than its right.
   */
  function floatingMenu(anchor, items, opts = {}) {
    closeMenus();
    const menu = el('div', { class: 'menu floating' });
    for (const item of items) {
      if (item.heading) { menu.append(el('div', { class: 'menu-heading' }, item.heading)); continue; }
      menu.append(el('button', {
        type: 'button', class: 'menu-item' + (item.danger ? ' danger' : '') + (item.checked ? ' checked' : ''), disabled: item.disabled,
        onclick: (e) => { e.stopPropagation(); closeMenus(); item.run(); },
      }, item.sub == null ? item.label : el('span', {}, el('b', {}, item.label), el('small', {}, item.sub))));
    }
    document.body.append(menu);
    const r = anchor.getBoundingClientRect();
    menu.style.position = 'fixed';
    menu.style.minWidth = '170px';
    menu.style.maxWidth = 'min(360px, calc(100vw - 16px))';
    const top = opts.up ? r.top - menu.offsetHeight - 6 : r.bottom + 4;
    menu.style.top = Math.max(8, Math.min(top, innerHeight - menu.offsetHeight - 8)) + 'px';
    const left = opts.left ? r.left : r.right - menu.offsetWidth;
    menu.style.left = Math.max(8, Math.min(left, innerWidth - menu.offsetWidth - 8)) + 'px';
    return menu;
  }

  function chatMenu(c, anchor) {
    anchor.closest('.chat') && anchor.closest('.chat').classList.add('menu-open');
    floatingMenu(anchor, [removeItem(c)]);
  }

  /** The menu item that deletes a backup chat or archives a regular one; `suffix` follows the word. */
  function removeItem(c, suffix = '') {
    return c.backup
      ? { label: 'Delete' + suffix, danger: true, run: () => removeChat(c) }
      : { label: 'Archive' + suffix, run: () => removeChat(c) };
  }

  async function removeChat(c) {
    const name = chatTitle(c);
    if (c.backup && !confirm(`Delete "${name}"? Its computer and files go with it.`)) return;
    try {
      if (c.backup) {
        await api('/agent-sessions/' + c.id, { method: 'DELETE' });
      } else {
        try {
          await cloud(c.account, '/v1/code/sessions/' + cse(c.id) + '/archive', { method: 'POST', json: {}, scoped: false });
        } catch (e) {
          if (e.status !== 409) throw e; // already archived
        }
      }
    } catch (e) {
      toast((c.backup ? "Couldn't delete: " : "Couldn't archive: ") + e.message);
      return;
    }
    const s = state.sessions.get(c.id);
    if (s && s.abort) s.abort.abort();
    state.sessions.delete(c.id);
    state.chats = state.chats.filter((x) => x.id !== c.id);
    toast(c.backup ? 'Deleted' : 'Archived');
    if (state.open && state.open.id === c.id) location.hash = '#/';
    else renderChats();
  }

  // ---------------------------------------------------------------------
  // A session's conversation, from its stream
  // ---------------------------------------------------------------------

  /** A message's words; with `result`, a tool result's, a picture in it as [picture]. */
  function textOf(content, result) {
    if (typeof content === 'string') return content;
    if (!Array.isArray(content)) return '';
    if (result) return content.map((b) => b && b.type === 'text' ? b.text : b && b.type === 'image' ? '[picture]' : '').filter(Boolean).join('\n');
    return content.filter((b) => b && b.type === 'text' && typeof b.text === 'string').map((b) => b.text).join('\n\n');
  }

  function stripSetup(text) {
    const t = text.trim();
    if (!t.startsWith('<delta-setup>')) return text;
    const end = t.indexOf('</delta-setup>');
    return end < 0 ? text : t.slice(end + '</delta-setup>'.length).trim();
  }

  /** What was typed, and the names in a closing <delta-files> block. */
  function splitFiles(text) {
    const t = text.trim();
    const open = t.lastIndexOf(FILES_OPEN);
    if (open < 0 || !t.endsWith(FILES_CLOSE)) return { text, files: [] };
    const files = t.slice(open + FILES_OPEN.length, t.length - FILES_CLOSE.length).split('\n')
      .filter((l) => l.startsWith('- ') && l.length > 2).map((l) => l.slice(2));
    return { text: t.slice(0, open).trim(), files };
  }

  /** What was typed, then a block naming the files. */
  function wrapFiles(typed, names) {
    const text = typed.trim();
    if (!names.length) return text;
    const one = names.length === 1;
    return (text ? text + '\n\n' : '') + FILES_OPEN + '\n' +
      (one ? 'The user sent you this file' : 'The user sent you these files') + '. ' +
      (one ? 'It is' : 'They are') + ' on the command center; fetch ' + (one ? 'it' : 'each one') +
      ' with `delta pull <name>`:\n' + names.map((n) => '- ' + n + '\n').join('') + FILES_CLOSE;
  }

  class Transcript {
    constructor() {
      this.entries = [];
      this.sequence = 0;
      this.working = false;
      this.status = null;
      this.pending = null;
      this.limitedUntil = 0;
      this.tools = new Map();
      this.uuids = new Set();
    }

    add(entry) { this.entries.push(entry); return entry; }
    note(text, bad) { this.add({ kind: 'note', text, bad }); }
    idle() { this.working = false; this.status = null; }

    sent(id, text, images, files) {
      this.uuids.add(id);
      this.add({ kind: 'you', text, images, files, uuid: id });
      this.working = true;
      this.status = null;
    }

    /** Applies one stream frame, and says whether anything on screen changed. */
    apply(event, id, data) {
      if (event === 'catch_up_truncated') { this.note('Some earlier activity was too long to replay.'); return true; }
      if (event !== 'client_event') return false;
      let frame;
      try { frame = JSON.parse(data); } catch (e) { return false; }
      const at = parseInt(id || frame.sequence_num, 10) || 0;
      if (at > 0 && at <= this.sequence) return false;
      if (at > this.sequence) this.sequence = at;
      try {
        return this.payload(frame.payload || {}, frame.source);
      } catch (e) {
        console.warn('skipped an event', e);
        return false;
      }
    }

    payload(p, source) {
      switch (p.type) {
        case 'user': return this.user(p, source);
        case 'assistant': return this.assistant(p);
        case 'result': return this.result(p);
        case 'control_request': return this.ask(p, source);
        case 'control_response': return this.settled(p.response && p.response.request_id);
        case 'control_cancel_request': return this.settled(p.request_id);
        case 'rate_limit_event': return this.rateLimit(p.rate_limit_info || {});
        case 'env_manager_log': {
          const line = p.data && p.data.content;
          if (!line || !line.trim()) return false;
          this.status = oneLine(rebrand(line));
          return this.working;
        }
      }
      return false;
    }

    /**
     * A message to the agent. The worker's own user frames are tool results,
     * which go with their calls.
     */
    user(p, source) {
      const content = p.message && p.message.content;
      if (source !== 'client') {
        if (!Array.isArray(content) || p.parent_tool_use_id) return false;
        let changed = false;
        for (const b of content) {
          if (!b || b.type !== 'tool_result') continue;
          const call = this.tools.get(b.tool_use_id);
          if (!call) continue;
          call.result = textOf(b.content, true);
          call.error = !!b.is_error;
          call.dirty = true;
          changed = true;
        }
        return changed;
      }
      if (p.isMeta) return false;
      let typed = stripSetup(textOf(content));
      const carried = typed.trim().startsWith(CARRY_OPEN) && typed.includes(CARRY_CLOSE);
      if (carried) typed = typed.trim().slice(typed.trim().indexOf(CARRY_CLOSE) + CARRY_CLOSE.length).trim();
      const { text, files } = splitFiles(typed);
      const images = carried || !Array.isArray(content) ? [] : content.filter((b) => b && b.type === 'image')
        .map((b) => b.source && b.source.data ? 'data:' + (b.source.media_type || 'image/jpeg') + ';base64,' + b.source.data : null);
      if (!text.trim() && !images.length && !files.length && !carried) return false;
      if (p.uuid && this.uuids.has(p.uuid)) return false;
      if (p.uuid) this.uuids.add(p.uuid);
      if (carried) this.note(CARRY_NOTE);
      if (text.trim() || images.length || files.length) this.add({ kind: 'you', text: text.trim(), images, files, uuid: p.uuid });
      this.working = true;
      this.status = null;
      return true;
    }

    assistant(p) {
      // A subagent's inner turns would bury the main thread.
      if (p.parent_tool_use_id) return false;
      let changed = !this.working || this.status !== null;
      this.working = true;
      this.status = null;
      for (const b of (p.message && p.message.content) || []) {
        if (!b) continue;
        if (b.type === 'text' && b.text && b.text.trim()) {
          this.add({ kind: 'agent', text: b.text.trim() });
          changed = true;
        } else if (b.type === 'thinking' && b.thinking && b.thinking.trim()) {
          this.add({ kind: 'thinking', text: b.thinking.trim() });
          changed = true;
        } else if (b.type === 'tool_use') {
          const call = this.add({ kind: 'tool', name: b.name || '', input: b.input || {}, id: b.id });
          if (b.id) this.tools.set(b.id, call);
          changed = true;
        }
      }
      return changed;
    }

    result(p) {
      this.working = false;
      this.status = null;
      this.pending = null;
      const sub = p.subtype;
      if (sub === 'error_during_execution') this.note('Stopped.');
      else if (sub === 'error_max_turns') this.note('Stopped at the turn limit.');
      else if (p.is_error || (sub && sub !== 'success')) {
        const said = typeof p.result === 'string' ? p.result.trim() : '';
        const last = this.entries[this.entries.length - 1];
        // A usage limit arrives as the agent's own words and then as the result.
        if (said && last && last.kind === 'agent' && last.text === said) return true;
        this.note(said ? oneLine(rebrand(said), 400) : 'The turn ended with an error.', true);
      }
      return true;
    }

    /** The worker asking permission for a tool call. */
    ask(p, source) {
      if (source !== 'worker') return false;
      const r = p.request || {};
      if (r.subtype !== 'can_use_tool' || !p.request_id || !r.tool_name) return false;
      this.pending = { requestId: p.request_id, tool: r.tool_name, input: r.input || {} };
      return true;
    }

    settled(requestId) {
      if (!this.pending || !requestId || requestId !== this.pending.requestId) return false;
      this.pending = null;
      return true;
    }

    rateLimit(info) {
      // A used-up window is no limit while cloud credits carry the session on.
      const onCredits = info.isUsingOverage === true || info.overageStatus === 'allowed' || info.overageStatus === 'allowed_warning';
      if (info.status === 'rejected' && !onCredits) {
        const resets = Number(info.resetsAt);
        if (resets > 0) { this.limitedUntil = resets * 1000; return true; }
      } else if (info.status) {
        const had = this.limitedUntil;
        this.limitedUntil = 0;
        return had > 0;
      }
      return false;
    }
  }

  // ---------------------------------------------------------------------
  // Carrying a conversation to another account
  // ---------------------------------------------------------------------
  //
  // When a chat's account reaches its usage limit, the chat can move to a new
  // session on another account. The old session's whole event history is
  // written out as text (every message, tool call and tool result in full,
  // nothing shortened), stored on the command center (POST /conversations),
  // and the new session's first message opens with a <delta-continued> block
  // naming it for `delta resume`. Pictures the user sent ride along on that
  // message. This is what the retired phone app did (Carry.java); keep the
  // block in step with `delta help`.

  /** Every event payload of a cloud session, oldest first. */
  async function history(chat) {
    const payloads = [];
    let cursor = null;
    for (let page = 0; page < 10000; page++) {
      const listed = await cloud(chat.account, eventsPath(chat.id) + '?sort_order=asc' +
        (cursor !== null ? '&cursor=' + encodeURIComponent(cursor) : ''));
      const data = (listed && listed.data) || [];
      for (const e of data) {
        if (!e) continue;
        // The next page starts after the last sequence number seen.
        if (e.sequence_num !== undefined && e.sequence_num !== null) cursor = String(e.sequence_num);
        if (e.payload && typeof e.payload === 'object') payloads.push(e.payload);
      }
      if (!data.length || !listed.next_cursor) return payloads;
    }
    throw new Error("The chat's history is too long to read");
  }

  /** Nothing quoted may close the block early. */
  const carryClean = (text) => text.split(CARRY_CLOSE).join('</delta-continued >');

  /** The whole conversation as text, for `delta resume`, and the pictures the user sent (base64 JPEGs). */
  function carryText(title, events) {
    const out = [];
    const pictures = [];
    const tools = new Map();
    const seen = new Set();
    const said = (who, text) => {
      if (typeof text !== 'string' || !text.trim()) return;
      out.push('[' + who + ']\n' + carryClean(text.trim()) + '\n\n');
    };
    const picture = (source) => {
      if (!source || source.type !== 'base64' || !source.data || source.media_type !== 'image/jpeg') return "[a picture, which can't be carried over]";
      pictures.push(source.data);
      return '[picture ' + pictures.length + ', attached to the first message]';
    };
    const result = (content) => {
      if (content === null || content === undefined) return '(no output)';
      if (typeof content === 'string') return content || '(no output)';
      if (!Array.isArray(content)) return JSON.stringify(content);
      const text = content.filter((b) => b && typeof b === 'object').map((b) =>
        b.type === 'text' ? b.text || ''
          : b.type === 'image' ? "[an image, which can't be carried over; produce it again if you need it]"
            : JSON.stringify(b)).join('\n');
      return text || '(no output)';
    };
    for (const p of events) {
      // A subagent's own turns are its working; what it found comes back as
      // the result of the call that started it.
      if (p.parent_tool_use_id) continue;
      if (p.uuid) { if (seen.has(p.uuid)) continue; seen.add(p.uuid); }
      const content = p.message && p.message.content;
      if (p.type === 'user') {
        if (p.isMeta) continue;
        if (typeof content === 'string') { said('User', stripSetup(content)); continue; }
        const words = [];
        for (const b of Array.isArray(content) ? content : []) {
          if (!b || typeof b !== 'object') continue;
          if (b.type === 'tool_result') {
            const name = tools.get(b.tool_use_id);
            said('Result of ' + (name || 'a tool call') + (b.is_error ? ' (error)' : ''), result(b.content));
          } else if (b.type === 'text' && typeof b.text === 'string') {
            words.push(stripSetup(b.text));
          } else if (b.type === 'image') {
            words.push(picture(b.source));
          }
        }
        if (words.length) said('User', words.join('\n\n'));
      } else if (p.type === 'assistant') {
        for (const b of Array.isArray(content) ? content : []) {
          if (!b || typeof b !== 'object') continue;
          if (b.type === 'text') said('You', b.text);
          else if (b.type === 'thinking') said('You (thinking)', b.thinking);
          else if (b.type === 'tool_use') {
            if (b.id && b.name) tools.set(b.id, b.name);
            said('You called ' + (b.name || 'a tool'), JSON.stringify(b.input || {}));
          }
        }
      }
    }
    const name = (title || '').trim().replace(/\n/g, ' ') || 'Untitled';
    const text = '# ' + name + '\n\n' +
      'The complete conversation of an earlier Delta session, which was moved to another account ' +
      '(usually because the one it ran on reached its usage limit): every message, every tool call with its full input, and every ' +
      'tool result in full, oldest first. "You" is the agent. Pictures the user sent are attached to ' +
      'the first message of the session that continues it, in the order they are mentioned.\n\n' +
      out.join('') + '[End of the conversation. Carry on from here.]\n';
    return { text, pictures };
  }

  /** The block that opens the new session's first message. */
  function carryBlock(title, id) {
    const t = (title || '').trim();
    return CARRY_OPEN + '\n' +
      'This conversation continues an earlier session' + (t ? ' ("' + carryClean(t) + '")' : '') +
      ' that was moved to another account, usually because the one it ran on reached its usage ' +
      'limit; this session runs on the new account. The whole conversation so far, every message, tool call and tool result in ' +
      'full, is on the command center. Once the Delta CLI is set up, and before anything else, run\n\n' +
      '    delta resume ' + id + ' conversation.md\n\n' +
      'which saves it to conversation.md, and read ALL of that file, from the first line to the last, ' +
      'in parts if need be. ' +
      'Pictures the user sent earlier are attached to this message, in the order the conversation ' +
      'mentions them. The old session\'s sandbox did not come along: clone repositories again and redo ' +
      'any work that was not pushed or saved elsewhere. Then pick up exactly where it left off. ' +
      'If the user wrote anything new, it follows this block.\n' +
      CARRY_CLOSE;
  }

  // ---------------------------------------------------------------------
  // Streams
  // ---------------------------------------------------------------------

  async function readEvents(resp, onEvent) {
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    let ev = { event: '', id: '', data: [] };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      buf += decoder.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        let line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        if (line.endsWith('\r')) line = line.slice(0, -1);
        if (line === '') {
          if (ev.data.length || ev.event) onEvent(ev.event || 'message', ev.id, ev.data.join('\n'));
          ev = { event: '', id: '', data: [] };
          continue;
        }
        if (line[0] === ':') continue;
        const colon = line.indexOf(':');
        const field = colon < 0 ? line : line.slice(0, colon);
        let v = colon < 0 ? '' : line.slice(colon + 1);
        if (v[0] === ' ') v = v.slice(1);
        if (field === 'event') ev.event = v;
        else if (field === 'data') ev.data.push(v);
        else if (field === 'id') ev.id = v;
      }
    }
  }

  /** Keeps the chat's stream open, resuming after the last event seen, until it is closed. */
  function connect(chat) {
    if (chat.abort) chat.abort.abort();
    const abort = new AbortController();
    chat.abort = abort;
    (async () => {
      let delay = 1000;
      while (!abort.signal.aborted) {
        try {
          const after = chat.t.sequence;
          const query = after > 0 ? '?from_sequence_num=' + after : '';
          const headers = { Accept: 'text/event-stream' };
          if (after > 0) headers['Last-Event-ID'] = String(after);
          const resp = chat.backup
            ? await api('/agent-sessions/' + chat.id + '/events' + query, { headers, raw: true, signal: abort.signal })
            : await cloud(chat.account, '/v1/code/sessions/' + cse(chat.id) + '/events/stream' + query, { headers, raw: true, signal: abort.signal });
          if (!resp.ok) throw failure(resp.status, await resp.text());
          chat.connected = true;
          chat.trouble = '';
          delay = 1000;
          if (chat === state.open) renderTail();
          await readEvents(resp, (event, id, data) => {
            if (chat.t.apply(event, id, data)) changed(chat);
          });
        } catch (e) {
          if (abort.signal.aborted) return;
          if (e.status === 404) {
            chat.connected = false;
            chat.trouble = 'This session no longer exists.';
            if (chat === state.open) renderTail();
            return;
          }
          chat.trouble = e.status === 403 ? e.message : 'Reconnecting…';
        }
        if (abort.signal.aborted) return;
        chat.connected = false;
        if (chat === state.open) renderTail();
        await sleep(delay, abort.signal);
        delay = Math.min(delay * 2, 15000);
      }
    })();
  }

  let frame = 0;
  function changed(chat) {
    const listed = findChat(chat.id);
    if (listed) {
      const status = chat.t.pending ? 'requires_action' : chat.t.working ? 'running' : 'idle';
      if (listed.status !== status) { listed.status = status; scheduleChats(); }
    }
    if (chat !== state.open) return;
    if (!frame) frame = requestAnimationFrame(() => { frame = 0; renderChat(); });
  }

  let chatsTimer = 0;
  function scheduleChats() {
    if (!chatsTimer) chatsTimer = setTimeout(() => { chatsTimer = 0; renderChats(); }, 100);
  }

  // ---------------------------------------------------------------------
  // Rendering a conversation
  // ---------------------------------------------------------------------

  const shortPath = (p) => {
    if (!p) return '';
    const parts = String(p).split('/').filter(Boolean);
    return parts.length > 3 ? '…/' + parts.slice(-3).join('/') : String(p);
  };

  function summary(input) {
    for (const k of ['command', 'file_path', 'notebook_path', 'path', 'pattern', 'url', 'query', 'description', 'prompt']) {
      if (typeof input[k] === 'string' && input[k].trim()) return oneLine(input[k]);
    }
    for (const v of Object.values(input)) if (typeof v === 'string' && v.trim()) return oneLine(v);
    return '';
  }

  const lineCount = (s) => typeof s === 'string' && s ? s.replace(/\n$/, '').split('\n').length : 0;

  /** How a tool call reads in its one line, as Codex words them. */
  function describe(name, input) {
    const n = name.replace(/^mcp__/, '');
    const path = input.file_path || input.notebook_path || input.path;
    switch (toolKey(name)) {
      case 'bash': return { verb: 'Ran', target: input.command, code: true };
      case 'read': return { verb: 'Read', target: shortPath(path) };
      case 'write': return { verb: 'Wrote', target: shortPath(path), add: lineCount(input.content) };
      case 'edit': return { verb: 'Edited', target: shortPath(path), add: lineCount(input.new_string), del: lineCount(input.old_string) };
      case 'multiedit': {
        const edits = Array.isArray(input.edits) ? input.edits : [];
        return {
          verb: 'Edited', target: shortPath(path),
          add: edits.reduce((t, e) => t + lineCount(e.new_string), 0), del: edits.reduce((t, e) => t + lineCount(e.old_string), 0),
        };
      }
      case 'notebookedit': return { verb: 'Edited', target: shortPath(path) };
      case 'grep': return { verb: 'Searched for', target: input.pattern, code: true };
      case 'glob': return { verb: 'Listed', target: input.pattern, code: true };
      case 'webfetch': return { verb: 'Fetched', target: input.url };
      case 'websearch': return { verb: 'Searched the web for', target: input.query };
      case 'todowrite': return { verb: 'Updated the plan', target: '' };
      case 'task': case 'agent': return { verb: 'Delegated', target: input.description || summary(input) };
      case 'exitplanmode': return { verb: 'Proposed a plan', target: '' };
      case 'askuserquestion': return { verb: 'Asked', target: (Array.isArray(input.questions) ? input.questions : []).map((q) => q && q.question).filter(Boolean).join(' · ') };
      default: return { verb: n.replace(/__/g, ' · ') || 'Used a tool', target: summary(input) };
    }
  }

  function clip(text, lines = 400) {
    const all = String(text).split('\n');
    return all.length <= lines ? String(text) : all.slice(0, lines).join('\n') + '\n… ' + (all.length - lines) + ' more lines';
  }

  function diffView(oldText, newText) {
    const box = el('div', { class: 'diff' });
    for (const l of String(oldText || '').split('\n')) box.append(el('div', { class: 'd' }, '- ' + l));
    for (const l of String(newText || '').split('\n')) box.append(el('div', { class: 'a' }, '+ ' + l));
    return box;
  }

  function toolBody(call) {
    const n = toolKey(call.name);
    const input = call.input;
    const body = el('div', { class: 'body' });
    if (n === 'bash') {
      body.append(el('pre', { class: 'cmd' }, '$ ' + (input.command || '')));
    } else if (n === 'edit') {
      body.append(diffView(input.old_string, input.new_string));
    } else if (n === 'multiedit' && Array.isArray(input.edits)) {
      for (const e of input.edits) body.append(diffView(e.old_string, e.new_string));
    } else if (n === 'write' && typeof input.content === 'string') {
      body.append(el('pre', {}, clip(input.content)));
    } else if (n !== 'read' || !call.result) {
      const keys = Object.keys(input);
      if (keys.length) body.append(el('pre', {}, clip(JSON.stringify(input, null, 2))));
    }
    if (call.result !== undefined && !(n === 'write' || ((n === 'edit' || n === 'multiedit') && !call.error))) {
      body.append(el('pre', { class: call.error ? 'err' : '' }, call.result.trim() ? clip(call.result) : '(no output)'));
    } else if (call.error) {
      body.append(el('pre', { class: 'err' }, clip(call.result || 'Failed')));
    }
    return body.childNodes.length ? body : null;
  }

  function buildEntry(e) {
    switch (e.kind) {
      case 'you': {
        const node = el('div', { class: 'you' });
        if (e.images && e.images.length) {
          node.append(el('div', { class: 'pics' }, e.images.map((src) => src
            ? el('img', { src, alt: 'Picture', onclick: () => window.open().document.write('<img src="' + src + '" style="max-width:100%">') })
            : el('div', { class: 'pic-missing' }, 'Picture'))));
        }
        if (e.files && e.files.length) {
          node.append(el('div', { class: 'chips' }, e.files.map((f) => el('span', { class: 'chip', title: f }, icon('file'), el('span', {}, f)))));
        }
        if (e.text) node.append(el('div', { class: 'bubble' }, e.text));
        return node;
      }
      case 'agent':
        return el('div', { class: 'agent md', html: Markdown.render(e.text) });
      case 'thinking': {
        const d = el('details', { class: 'thinking', open: e.open },
          el('summary', {}, el('span', {}, 'Thought'), icon('chevron', 'chev')),
          el('div', { class: 'thought' }, e.text));
        d.addEventListener('toggle', () => { e.open = d.open; });
        return d;
      }
      case 'tool': {
        if (toolKey(e.name) === 'todowrite' && Array.isArray(e.input.todos)) {
          return el('div', { class: 'tool' }, el('div', {}, 'Updated the plan'),
            el('ul', { class: 'todos' }, e.input.todos.map((t) => el('li', { class: t.status || '' }, t.content || t.activeForm || ''))));
        }
        const d = describe(e.name, e.input);
        const head = el('summary', {}, el('span', { class: 'verb' }, d.verb));
        if (d.target) head.append(el(d.code ? 'code' : 'span', { class: 'target', title: d.target }, oneLine(d.target, 200)));
        if (d.add || d.del) head.append(el('span', { class: 'stat' }, el('span', { class: 'a' }, '+' + (d.add || 0)), el('span', { class: 'd' }, '-' + (d.del || 0))));
        if (e.error) head.append(el('span', { class: 'failed' }, 'failed'));
        const body = toolBody(e);
        if (!body) return el('div', { class: 'tool' }, el('div', { class: 'line' }, ...Array.from(head.childNodes)));
        head.append(icon('chevron', 'chev'));
        const node = el('details', { class: 'tool', open: e.open }, head, body);
        node.addEventListener('toggle', () => { e.open = node.open; });
        return node;
      }
      default:
        return el('div', { class: 'note' + (e.bad ? ' bad' : '') }, e.text);
    }
  }

  function nearBottom() {
    const s = $('#scroller');
    return s.scrollHeight - s.scrollTop - s.clientHeight < 120;
  }
  function toBottom() {
    const s = $('#scroller');
    s.scrollTop = s.scrollHeight;
  }

  function renderChat() {
    const chat = state.open;
    const box = $('#transcript');
    if (!chat) { box.replaceChildren(); $('#tail').replaceChildren(); return; }
    const stick = nearBottom();
    let prev = null;
    for (const e of chat.t.entries) {
      if (!e.node || e.dirty) {
        const node = buildEntry(e);
        if (e.node && e.node.parentNode === box) box.replaceChild(node, e.node);
        e.node = node;
        e.dirty = false;
      }
      if (e.node.parentNode !== box || e.node.previousSibling !== prev) {
        box.insertBefore(e.node, prev ? prev.nextSibling : box.firstChild);
      }
      prev = e.node;
    }
    while (prev && prev.nextSibling) prev.nextSibling.remove();
    renderTail();
    renderComposer();
    if (stick) toBottom();
  }

  function renderTail() {
    const chat = state.open;
    const tail = $('#tail');
    tail.replaceChildren();
    if (!chat || state.view !== 'chat') return;
    const t = chat.t;
    if (t.pending && t.pending.tool === 'AskUserQuestion' && Array.isArray(t.pending.input.questions)) {
      // Kept across renders, so picks and typed answers survive new events.
      t.pending.node = t.pending.node || questionForm(chat, t.pending);
      tail.append(t.pending.node);
    } else if (t.pending) {
      const p = t.pending;
      const d = describe(p.tool, p.input);
      tail.append(el('div', { class: 'ask' },
        el('div', { class: 'q' }, 'Delta wants to ' + (d.verb === 'Ran' ? 'run a command' : 'use ' + p.tool.replace(/^mcp__/, ''))),
        el('pre', {}, p.input.command || summary(p.input) || JSON.stringify(p.input, null, 2)),
        el('div', { class: 'actions' },
          el('button', { type: 'button', class: 'button', onclick: () => answer(chat, false) }, 'Deny'),
          el('button', { type: 'button', class: 'button primary', onclick: () => answer(chat, true) }, 'Allow'))));
    } else if (t.working) {
      tail.append(el('div', { class: 'working' }, el('span', { class: 'shimmer' }, 'Working'),
        t.status ? el('span', { class: 'status' }, t.status) : null));
    }
    if (chat.trouble) tail.append(el('div', { class: 'note' }, chat.trouble));
    else if (!chat.connected && !t.entries.length) tail.append(el('div', { class: 'note' }, 'Loading…'));
  }

  /**
   * The agent's AskUserQuestion: each question's options to pick from, plus
   * an answer of the user's own. The answers go back as the tool's input.
   */
  function questionForm(chat, p) {
    const questions = p.input.questions.filter((q) => q && typeof q.question === 'string');
    const picks = questions.map(() => new Set());
    const own = questions.map(() => '');
    const send = el('button', { type: 'button', class: 'button primary', disabled: true }, 'Send');
    const ready = () => questions.every((q, i) => picks[i].size || own[i].trim());
    const update = () => { send.disabled = !ready(); };
    const blocks = questions.map((q, i) => {
      const multi = !!q.multiSelect;
      const options = (Array.isArray(q.options) ? q.options : []).filter((o) => o && o.label);
      const buttons = options.map((o) => {
        const b = el('button', { type: 'button', class: 'option', 'aria-pressed': 'false' },
          el('span', { class: 'label' }, o.label),
          o.description ? el('span', { class: 'desc' }, o.description) : null);
        b.addEventListener('click', () => {
          if (picks[i].has(o.label)) picks[i].delete(o.label);
          else {
            if (!multi) { picks[i].clear(); other.value = ''; own[i] = ''; }
            picks[i].add(o.label);
          }
          buttons.forEach((ob, k) => ob.setAttribute('aria-pressed', String(picks[i].has(options[k].label))));
          update();
        });
        return b;
      });
      const other = el('input', { type: 'text', class: 'other', placeholder: 'Other…' });
      other.addEventListener('input', () => {
        own[i] = other.value;
        if (!multi && own[i].trim()) {
          picks[i].clear();
          for (const b of buttons) b.setAttribute('aria-pressed', 'false');
        }
        update();
      });
      other.addEventListener('keydown', (e) => { if (e.key === 'Enter' && ready()) send.click(); });
      return el('div', { class: 'question' },
        q.header ? el('div', { class: 'header' }, q.header) : null,
        el('div', { class: 'q' }, q.question),
        multi ? el('div', { class: 'hint' }, 'Pick any') : null,
        el('div', { class: 'options' }, buttons),
        other);
    });
    send.addEventListener('click', () => {
      if (!ready()) return;
      const answers = {};
      questions.forEach((q, i) => {
        const chosen = [...picks[i]];
        if (own[i].trim()) chosen.push(own[i].trim());
        answers[q.question] = chosen.join(', ');
      });
      answer(chat, true, { ...p.input, answers });
    });
    return el('div', { class: 'ask questions' }, blocks,
      el('div', { class: 'actions' },
        el('button', { type: 'button', class: 'button', onclick: () => answer(chat, false) }, 'Skip'),
        send));
  }

  // ---------------------------------------------------------------------
  // Talking to a session
  // ---------------------------------------------------------------------

  async function answer(chat, allow, input) {
    const p = chat.t.pending;
    if (!p) return;
    try {
      await postEvents(chat, [{
        type: 'control_response',
        response: {
          subtype: 'success', request_id: p.requestId,
          response: allow ? { behavior: 'allow', updatedInput: input || p.input } : { behavior: 'deny', message: 'Denied from the website.' },
        },
        uuid: uuid(),
      }]);
      chat.t.pending = null;
      changed(chat);
    } catch (e) {
      toast("Couldn't answer: " + e.message);
    }
  }

  async function interrupt() {
    const chat = state.open;
    if (!chat) return;
    try {
      if (chat.backup) await api('/agent-sessions/' + chat.id + '/interrupt', { method: 'POST' });
      else await postEvents(chat, [controlRequest({ subtype: 'interrupt' })]);
      toast('Stopping…');
    } catch (e) {
      toast("Couldn't stop it: " + e.message);
    }
  }

  /** The name a file goes up under: its own, or the first numbered one free. */
  function numbered(name, n) {
    if (n === 1) return name;
    const dot = name.lastIndexOf('.');
    return dot > 0 ? name.slice(0, dot) + ' (' + n + ')' + name.slice(dot) : name + ' (' + n + ')';
  }

  async function upload(file, progress) {
    let base = file.name.replace(/[\/\\\u0000]/g, '_').replace(/^\.+/, '') || 'file';
    if (base.length > 200) base = base.slice(-200);
    const taken = new Set((await api('/files')).map((f) => f.name));
    for (let n = 1; n < 100; n++) {
      const name = numbered(base, n);
      if (taken.has(name)) continue;
      const status = await put(name, file, progress);
      if (status === 201) return name;
      if (status !== 409) throw new Error(status === 413 ? file.name + ' is too big for the command center' : "Couldn't upload " + file.name);
    }
    throw new Error("Couldn't find a free name for " + file.name);
  }

  /** A PUT with upload progress, which fetch doesn't report. */
  function put(name, file, progress) {
    return new Promise((resolve, reject) => {
      const x = new XMLHttpRequest();
      x.open('PUT', url('/files/' + encodeURIComponent(name)));
      x.setRequestHeader('Authorization', 'Bearer ' + token);
      if (progress) x.upload.onprogress = (e) => e.lengthComputable && progress(e.loaded / e.total);
      x.onload = () => resolve(x.status);
      x.onerror = () => reject(new Error("Couldn't reach the command center"));
      x.send(file);
    });
  }

  async function toJpeg(file) {
    const url = URL.createObjectURL(file);
    try {
      const img = new Image();
      img.src = url;
      await img.decode();
      const scale = Math.min(1, PICTURE_SIZE / Math.max(img.naturalWidth, img.naturalHeight));
      const c = document.createElement('canvas');
      c.width = Math.max(1, Math.round(img.naturalWidth * scale));
      c.height = Math.max(1, Math.round(img.naturalHeight * scale));
      const g = c.getContext('2d');
      g.fillStyle = '#fff';
      g.fillRect(0, 0, c.width, c.height);
      g.drawImage(img, 0, 0, c.width, c.height);
      const dataUrl = c.toDataURL('image/jpeg', 0.85);
      return { dataUrl, base64: dataUrl.slice(dataUrl.indexOf(',') + 1) };
    } finally {
      URL.revokeObjectURL(url);
    }
  }

  async function attach(files) {
    for (const file of files) {
      const pictures = state.attachments.filter((a) => a.kind === 'picture').length;
      const others = state.attachments.length - pictures;
      if (/^image\/(jpeg|png|gif|webp|bmp)$/.test(file.type) && pictures < MAX_PICTURES) {
        try {
          const pic = await toJpeg(file);
          state.attachments.push({ kind: 'picture', name: file.name, ...pic });
        } catch (e) {
          toast("Couldn't read " + file.name);
        }
      } else if (others < MAX_FILES) {
        state.attachments.push({ kind: 'file', name: file.name, file, size: file.size });
      } else {
        toast('At most ' + MAX_FILES + ' files per message');
        break;
      }
    }
    renderAttachments();
    $('#input').focus();
  }

  function renderAttachments() {
    const box = $('#attachments');
    box.hidden = !state.attachments.length;
    box.replaceChildren(...state.attachments.map((a, k) => {
      const rm = el('button', {
        type: 'button', class: 'rm', title: 'Remove',
        onclick: () => { state.attachments.splice(k, 1); renderAttachments(); },
      }, icon('x'));
      if (a.kind === 'picture') return el('div', { class: 'att pic', title: a.name }, el('img', { src: a.dataUrl, alt: a.name }), rm);
      return el('div', { class: 'att', title: a.name }, el('span', { class: 'icon' }, icon('file')),
        el('span', { class: 'label' }, el('b', {}, a.name), el('small', {}, a.kind === 'remote' ? 'On the command center' : size(a.size))), rm);
    }));
    renderComposer();
  }

  /**
   * Sends what is in the composer: to the open chat, or to a new one. With
   * `move`, the open chat goes on in a new session on another account
   * instead (`target`, or the best one), and what was typed follows the
   * conversation it carries.
   */
  async function send(move, target) {
    const input = $('#input');
    const typed = input.value.trim();
    const attachments = state.attachments.slice();
    const chat = state.view === 'chat' ? state.open : null;
    if (move && (!chat || chat.backup)) return;
    if (state.busy || (!move && !typed && !attachments.length)) return;
    if (state.view === 'chat' && !chat) return;
    state.busy = true;
    renderComposer();
    try {
      const pictures = attachments.filter((a) => a.kind === 'picture');
      const names = [];
      for (const a of attachments) {
        if (a.kind === 'remote') names.push(a.name);
        else if (a.kind === 'file') names.push(await upload(a.file));
      }
      const message = wrapFiles(typed, names);
      const jpegs = pictures.map((p) => p.base64);
      const images = pictures.map((p) => p.dataUrl);
      const id = uuid();
      if (move) {
        await moveChat(chat, typed, message, id, jpegs, images, names, target);
        clearComposer();
      } else if (chat) {
        chat.t.sent(id, typed, images, names);
        // Cleared before it goes, so a failure can put the words back.
        clearComposer();
        renderChat();
        toBottom();
        try {
          if (chat.backup) await api('/agent-sessions/' + chat.id + '/messages', { method: 'POST', json: { uuid: id, text: message, images: jpegs } });
          else await postEvents(chat, [modeRequest(), userEvent(id, message, jpegs, cse(chat.id))]);
          const listed = findChat(chat.id);
          if (listed) { listed.updated = Date.now(); listed.status = 'running'; state.chats.sort((a, b) => b.updated - a.updated); renderChats(); }
        } catch (e) {
          chat.t.idle();
          chat.t.note('Not sent: ' + e.message, true);
          renderChat();
          if (!input.value) input.value = typed;
          autosize();
        }
      } else {
        await create(typed, message, id, jpegs, images, names);
        clearComposer();
      }
    } catch (e) {
      toast(e.message);
    } finally {
      state.busy = false;
      renderComposer();
    }
  }

  /**
   * Starts a new chat with the message, on the account chosen in the
   * composer, or the best one. `carry` ({title, block, pictures, from,
   * target}) makes it the continuation of a moved one: a regular session on
   * `target`, or the best account other than `from`, whose first message
   * opens with the block.
   */
  async function create(typed, message, id, jpegs, images, names, carry) {
    const kind = carry ? 'cloud' : state.kind;
    const title = carry ? carry.title : titleFor(typed || names[0] || 'Picture');
    $('#hero').querySelector('h1').textContent = 'Starting…';
    try {
      let chat;
      if (kind === 'backup') {
        const made = await api('/agent-sessions', { method: 'POST', json: { title, uuid: id, text: message, images: jpegs } });
        chat = { id: made.id, backup: true, title };
      } else {
        const account = carry ? await pickAccount(carry.from, carry.target) : await pickAccount(null, chosenAccount());
        const env = await environment(account);
        if (carry) {
          message = message ? carry.block + '\n\n' + message : carry.block;
          jpegs = carry.pictures.concat(jpegs);
        }
        const first = message ? SETUP_BLOCK + '\n\n' + message : SETUP_BLOCK;
        const made = await cloud(account, '/v1/sessions', {
          method: 'POST',
          beta: true,
          json: {
            title,
            events: [{ type: 'event', data: modeRequest() }, { type: 'event', data: userEvent(id, first, jpegs, '') }],
            session_context: { sources: [], outcomes: [] },
            environment_id: env,
          },
        });
        if (!validId(made.id)) throw new Error('The cloud returned an unexpected session id');
        chat = { id: made.id, backup: false, account, title };
      }
      chat.t = new Transcript();
      if (carry) {
        chat.t.uuids.add(id);
        chat.t.note('Continued from an earlier session on ' + accountName(carry.from) + '. This one runs on ' + accountName(chat.account) + '.');
      }
      if (!carry || typed || images.length || names.length) chat.t.sent(id, typed, images, names);
      else chat.t.working = true;
      state.sessions.set(chat.id, chat);
      state.chats.unshift({ id: chat.id, title, status: 'running', updated: Date.now(), backup: chat.backup, account: chat.account, fresh: true });
      location.hash = '#/s/' + chat.id;
      if (!chat.backup) toast((carry ? 'Moved to ' : 'Started on ') + accountName(chat.account));
    } finally {
      $('#hero').querySelector('h1').textContent = "New chat";
    }
  }

  /** Carries `chat` to a new session on `target`, or the best other account (see carryText). */
  async function moveChat(chat, typed, message, id, jpegs, images, names, target) {
    if (chat.t.limitedUntil > Date.now()) noteLimit(chat.account, chat.t.limitedUntil);
    const title = chatTitle(findChat(chat.id) || chat);
    chat.moving = true;
    renderComposer();
    try {
      const carried = carryText(title, await history(chat));
      const stored = await api('/conversations', { method: 'POST', body: carried.text, headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
      if (!stored || !stored.id) throw new Error("The command center didn't store the conversation");
      await create(typed, message, id, jpegs, images, names,
        { title, block: carryBlock(title, stored.id), pictures: carried.pictures, from: chat.account, target });
    } catch (e) {
      throw new Error("Couldn't move the chat: " + e.message);
    } finally {
      chat.moving = false;
      renderComposer();
    }
  }

  // ---------------------------------------------------------------------
  // The composer
  // ---------------------------------------------------------------------

  function autosize() {
    const t = $('#input');
    t.style.height = 'auto';
    t.style.height = Math.min(t.scrollHeight, 240) + 'px';
  }

  function clearComposer() {
    $('#input').value = '';
    state.attachments = [];
    renderAttachments();
    autosize();
  }

  function renderComposer() {
    const chat = state.view === 'chat' ? state.open : null;
    const kind = chat ? kindOf(chat) : state.kind;
    const btn = $('#kind-btn');
    btn.replaceChildren(el('span', { class: 'mark' }, MARK[kind]), el('span', {}, KIND_NAME[kind]));
    btn.disabled = !!chat;
    if (!chat) btn.append(icon('down'));
    btn.title = chat ? (chat.backup ? 'A backup session, run by the command center' : 'A regular session, on your accounts') : 'Choose the kind of session';
    const hero = $('#hero-kind');
    hero.replaceChildren(el('span', {}, MARK[state.kind] + ' ' + KIND_NAME[state.kind]), icon('down'));

    const typed = $('#input').value.trim() || state.attachments.length;
    const working = chat && chat.t.working;
    const sendBtn = $('#send');
    const stop = working && !typed;
    sendBtn.replaceChildren(icon(stop ? 'stop' : 'up'));
    sendBtn.title = stop ? 'Stop' : 'Send';
    sendBtn.dataset.stop = stop ? '1' : '';
    sendBtn.disabled = state.busy || (!stop && !typed);
    $('#input').placeholder = chat ? (working ? 'Send a message while Delta works' : 'Reply to Delta') : 'Ask Delta anything';

    const picker = $('#account-picker');
    picker.hidden = !!chat || state.kind !== 'cloud' || !state.accounts.length;
    if (!picker.hidden) {
      const chosen = chosenAccount();
      const best = ranked()[0];
      $('#account-btn').replaceChildren(el('span', {}, chosen ? accountName(chosen) : 'Auto' + (best ? ' · ' + accountName(best.id) : '')), icon('down'));
      $('#account-btn').title = chosen ? 'New chats start on ' + accountName(chosen) : 'New chats start on the best account' + (best ? ', now ' + accountName(best.id) : '');
    }

    const banner = $('#banner');
    // The session's own word, unless the account's report says credits carry it on.
    const status = chat && accountStatus.get(chat.account);
    const onCredits = !!(status && status.credits && status.credits.left > 0);
    const limited = chat && !chat.backup && chat.t.limitedUntil > Date.now() && !onCredits;
    banner.hidden = !limited;
    banner.replaceChildren();
    if (limited) {
      if (Date.now() - refreshed > 60000) refreshAccounts();
      const movable = canMove(chat.account);
      const best = ranked(chat.account)[0];
      banner.append(el('span', {}, 'This chat\'s account, ' + accountName(chat.account) + ', has reached its usage limit until ' + when(chat.t.limitedUntil) + '.' +
        (movable ? ' Move the chat to another account to carry on now; anything typed below goes with it.' : ' Every other account is at its limit too.')));
      if (movable) {
        const busy = state.busy || !!chat.moving;
        banner.append(el('button', { type: 'button', class: 'button primary', disabled: busy, onclick: () => send(true, best && best.id) },
          chat.moving ? 'Moving…' : best ? 'Move to ' + accountName(best.id) : 'Move to another account'));
        banner.append(el('button', { type: 'button', class: 'button', disabled: busy, onclick: (e) => { e.stopPropagation(); moveMenu(chat, e.currentTarget, true); } }, 'Other account…'));
      }
    }
  }

  // The account new chats start on: one the user chose in the composer, or
  // (when none is chosen, or it was removed) the best one at the time.
  function chosenAccount() {
    const id = stored('delta.account');
    return id && state.accounts.some((a) => a.id === id) ? id : null;
  }

  let refreshing = null;
  let refreshed = 0;
  /** Brings the accounts' statuses up to date in the background, then redraws what shows them. */
  function refreshAccounts(fresh = 30000) {
    if (!refreshing) {
      refreshed = Date.now();
      refreshing = checkAccounts(fresh).catch(() => {}).finally(() => { refreshing = null; renderComposer(); });
    }
    return refreshing;
  }

  async function accountMenu(anchor) {
    await refreshAccounts();
    const chosen = chosenAccount();
    const best = ranked()[0];
    const pick = (id) => { stored('delta.account', id); renderComposer(); $('#input').focus(); };
    floatingMenu(anchor, [
      { heading: 'Start new chats on' },
      { label: 'Auto', sub: (best ? 'Now ' + accountName(best.id) : 'No account has room now') + ': the first with cloud credits left, or else with room',
        checked: !chosen, run: () => pick(null) },
      ...state.accounts.map((a) => ({
        label: accountLabel(a), sub: standing(a.id),
        checked: chosen === a.id, disabled: !usable(a.id), run: () => pick(a.id),
      })),
    ], { up: true, left: true });
  }

  /** Offers to move `chat` to another account, each shown with how it stands. */
  async function moveMenu(chat, anchor, up) {
    if (chat.moving || state.busy) return;
    await refreshAccounts();
    if (state.open !== chat) return;
    const best = ranked(chat.account)[0];
    floatingMenu(anchor, [
      { heading: 'Move this chat to' },
      ...state.accounts.map((a) => a.id === chat.account
        ? { label: accountName(a.id), sub: 'This chat runs here now', checked: true, disabled: true, run: () => {} }
        : {
          label: accountLabel(a),
          sub: standing(a.id) + (best && best.id === a.id ? ' · best' : ''),
          disabled: !usable(a.id), run: () => confirmMove(chat, a.id),
        }),
    ], { up });
  }

  function confirmMove(chat, target) {
    const limited = chat.t.limitedUntil > Date.now();
    if (!limited && !confirm('Move this chat to ' + accountName(target) + '?\n\nIt carries on in a new chat there, which is given the whole conversation so far' +
      (chat.t.working ? ' (Delta is still working here: what it does from now on stays behind)' : '') +
      '. This chat stays in the list. Anything typed in the box goes along.')) return;
    send(true, target);
  }

  // ---------------------------------------------------------------------
  // Views
  // ---------------------------------------------------------------------

  // The pages, by their address (#/files), with what fills each.
  const PAGES = new Map([['files', loadFiles], ['tablet', loadTablet], ['qa', loadQA], ['accounts', loadUsage]]);

  function show(view) {
    state.view = view;
    $('#view-chat').hidden = !(view === 'chat' || view === 'new');
    for (const page of PAGES.keys()) $('#view-' + page).hidden = view !== page;
    document.querySelectorAll('.nav-item[data-view]').forEach((a) => a.classList.toggle('active', a.dataset.view === view));
    $('#app').classList.remove('drawer');
    closeMenus();
    renderChats();
  }

  function setTitle(text, kind) {
    const box = $('#title');
    box.replaceChildren();
    if (text) box.append(el('span', { class: 't' }, text));
    if (kind) box.append(el('span', { class: 'kind' }, MARK[kind] + ' ' + KIND_NAME[kind]));
    document.title = text ? text + ' · Delta' : 'Delta';
  }

  function topActions(chat) {
    const box = $('#top-actions');
    box.replaceChildren();
    if (!chat) return;
    if (!chat.backup) {
      const where = el('button', {
        type: 'button', class: 'pill account-chip',
        title: chat.account ? 'This chat runs on ' + accountName(chat.account) + ' (' + chat.account + '). Choose another account to move it there.' : 'The account this chat runs on',
        onclick: (e) => { e.stopPropagation(); moveMenu(chat, e.currentTarget); },
      }, el('span', {}, chat.account ? accountName(chat.account) : 'Account'), icon('down'));
      box.append(where);
    }
    const more = el('button', { type: 'button', class: 'icon-btn ghost', title: 'More' }, icon('more'));
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      floatingMenu(more, [removeItem(findChat(chat.id) || chat, ' chat')]);
    });
    box.append(more);
  }

  function leaveChat() {
    if (state.open && state.open.abort) state.open.abort.abort();
    if (state.open) { state.open.abort = null; state.open.connected = false; }
    state.open = null;
  }

  async function route() {
    const hash = location.hash.replace(/^#\/?/, '');
    if (hash.startsWith('s/')) {
      await openChat(hash.slice(2));
      return;
    }
    leaveChat();
    const page = PAGES.has(hash) ? hash : 'new';
    show(page);
    setTitle('');
    topActions(null);
    if (page !== 'new') { PAGES.get(page)(); return; }
    $('#hero').hidden = false;
    renderChat();
    renderComposer();
    refreshAccounts();
    $('#input').focus();
  }

  async function openChat(id) {
    if (!validId(id)) { location.hash = '#/'; return; }
    if (state.open && state.open.id === id && state.open.abort) return;
    leaveChat();
    let chat = state.sessions.get(id);
    if (!chat) {
      let listed = findChat(id);
      if (!listed && !isBackup(id)) {
        if (!state.listed || state.listing) await listChats().catch(() => {});
        listed = findChat(id);
      }
      if (!listed && !isBackup(id)) {
        toast("That chat isn't on any of your accounts.");
        location.hash = '#/';
        return;
      }
      chat = { id, backup: isBackup(id), account: listed && listed.account, title: listed && listed.title, t: new Transcript() };
      state.sessions.set(id, chat);
    }
    if (location.hash.replace(/^#\/?/, '') !== 's/' + id) return; // moved on meanwhile
    state.open = chat;
    state.last = id;
    show('chat');
    $('#hero').hidden = true;
    setTitle(chatTitle(findChat(id) || chat), kindOf(chat));
    topActions(chat);
    $('#transcript').replaceChildren();
    renderChat();
    toBottom();
    connect(chat);
    $('#input').focus();
  }

  // ---------------------------------------------------------------------
  // Pages
  // ---------------------------------------------------------------------

  // Each page's timer, which loads it again while it is on screen.
  const polls = {};
  /** Stops the page's timer, and with `ms` loads it again in that long if it is still on screen. */
  function poll(view, load, ms) {
    clearTimeout(polls[view]);
    if (ms) polls[view] = setTimeout(() => { if (state.view === view) load(); }, ms);
  }

  // act makes a request for a button, toasts what came of it (done when it
  // worked, if anything; the error, after failed, when it didn't) and then
  // shows the page again with after.
  async function act(path, opts, after, done = '', failed = '') {
    try {
      await api(path, opts);
      if (done) toast(done);
    } catch (e) {
      toast(failed + e.message);
    }
    after();
  }

  const chatButton = (session) => button('Open chat', () => { location.hash = '#/s/' + session; }, '', 'See what the agent is doing');
  const jobBar = (percent) => el('span', { class: 'job-bar' }, el('span', { style: 'width:' + percent + '%' }));

  // ---------------------------------------------------------------------
  // Files
  // ---------------------------------------------------------------------

  async function loadFiles() {
    const box = $('#files');
    loading(box);
    let files;
    try {
      files = await api('/files');
    } catch (e) {
      empty(box, "Couldn't list the files: " + e.message);
      return;
    }
    if (state.view !== 'files') return;
    if (!files.length) { empty(box, 'No files yet. What agents push shows up here.'); return; }
    box.replaceChildren(...files.map((f) => row('file', f.name, size(f.size) + ' · ' + dated(f.modified), [
      button('Send to agent', () => sendToAgent(f.name), '', 'Attach to your next message'),
      button('Download', () => download(f.name)),
      button('Delete', () => deleteFile(f.name), 'danger'),
    ], { title: f.name })));
  }

  function sendToAgent(name) {
    if (!state.attachments.some((a) => a.kind === 'remote' && a.name === name)) state.attachments.push({ kind: 'remote', name });
    renderAttachments();
    location.hash = state.last && state.sessions.has(state.last) ? '#/s/' + state.last : '#/';
    toast('Attached ' + name + ' to your next message');
  }

  async function download(name) {
    try {
      const resp = await api('/files/' + encodeURIComponent(name), { raw: true });
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      const blob = await resp.blob();
      const url = URL.createObjectURL(blob);
      const a = el('a', { href: url, download: name });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60000);
    } catch (e) {
      toast("Couldn't download " + name + ': ' + e.message);
    }
  }

  async function deleteFile(name) {
    if (!confirm('Delete ' + name + ' from the command center?')) return;
    try {
      await api('/files/' + encodeURIComponent(name), { method: 'DELETE' });
      loadFiles();
    } catch (e) {
      toast("Couldn't delete " + name + ': ' + e.message);
    }
  }

  async function uploadFiles(files) {
    const bar = $('#upload-progress');
    bar.hidden = false;
    try {
      for (let k = 0; k < files.length; k++) {
        await upload(files[k], (f) => { bar.firstChild.style.width = ((k + f) / files.length * 100) + '%'; });
      }
      toast(files.length === 1 ? 'Uploaded ' + files[0].name : 'Uploaded ' + files.length + ' files');
    } catch (e) {
      toast(e.message);
    } finally {
      bar.hidden = true;
      bar.firstChild.style.width = '0';
      loadFiles();
    }
  }

  // ---------------------------------------------------------------------
  // Tablet: the reMarkable's documents, their transform jobs, and pairing
  // Flashcards
  // ---------------------------------------------------------------------

  const DOC_KIND = { pdf: 'PDF', epub: 'EPUB', notebook: 'Notebook' };
  const JOB_STATUS = { running: 'Working on it…', done: 'Done', failed: 'Failed' };

  /** The names of the folders an item is in, innermost first, and whether they are in the trash. */
  function folders(it, byId) {
    const path = [];
    for (let p = it.parent, n = 0; p && n < 50; n++) {
      if (p === 'trash') return { path, trash: true };
      const f = byId.get(p);
      if (!f) break;
      path.push(f.name);
      p = f.parent;
    }
    return { path, trash: false };
  }

  async function loadTablet() {
    poll('tablet');
    loadDevices();
    const box = $('#tablet');
    loading(box);
    let items, jobs;
    try {
      [items, jobs] = await Promise.all([api('/remarkable'), api('/transforms')]);
    } catch (e) {
      empty(box, "Couldn't read the tablet: " + e.message);
      return;
    }
    if (state.view !== 'tablet') return;
    renderJobs(jobs);
    const busy = new Set(jobs.filter((j) => j.status === 'running').map((j) => j.document));
    if (busy.size) poll('tablet', loadTablet, 5000);

    const byId = new Map(items.map((it) => [it.id, it]));
    const docs = items.filter((it) => it.type !== 'folder' && !folders(it, byId).trash)
      .sort((a, b) => instant(b.modified) - instant(a.modified));
    if (!docs.length) { empty(box, 'Nothing on the tablet yet.'); return; }
    box.replaceChildren(...docs.map((it) => {
      const facts = [DOC_KIND[it.type] || it.type];
      if (it.pages) facts.push(it.pages + (it.pages === 1 ? ' page' : ' pages'));
      const folder = folders(it, byId).path.reverse().join(' / ');
      if (folder) facts.push(folder);
      if (it.modified) facts.push(dated(it.modified));
      const start = button(busy.has(it.id) ? 'Transforming…' : 'Transform', () => transform(it, start));
      start.disabled = busy.has(it.id);
      return row('file', it.name, facts.join(' · '), start, { title: it.name });
    }));
  }

  function renderJobs(jobs) {
    const box = $('#jobs');
    box.hidden = $('#jobs-head').hidden = !jobs.length;
    box.replaceChildren(...jobs.map((j) => {
      const running = j.status === 'running';
      const percent = Math.max(0, Math.min(100, j.percent || 0));
      let detail = JOB_STATUS[j.status] || j.status;
      if (running) detail = (j.progress || 'Starting…') + ' · ' + percent + '%';
      if (j.status === 'done') detail = '“' + j.result + '” is on the tablet';
      if (j.status === 'failed' && j.error) detail = j.error;
      const actions = (j.files || []).map((name) =>
        button(name.toLowerCase().endsWith('.docx') ? 'Word' : 'PDF', () => download(name), '', 'Download ' + name));
      if (j.session && j.status !== 'done') actions.push(chatButton(j.session));
      actions.push(button(running ? 'Cancel' : 'Remove', () => forgetJob(j), 'danger', running ? 'Stop waiting for it' : 'Remove from the list'));
      // The agent's title for the job, once it has given one, over the document it came from.
      return row(running ? 'refresh' : 'file', j.title || j.name,
        (j.title ? 'From ' + j.name + ' · ' : '') + detail + ' · ' + dated(j.created), actions,
        { title: j.title || j.name, small: j.status === 'failed' ? 'failed' : '', extra: running ? jobBar(percent) : null });
    }));
  }

  // The formats a job can make, as on the phone: the PDF, which goes on the
  // tablet, ticked to begin with, the rest unticked each time.
  const FORMATS = [['pdf', 'PDF'], ['docx', 'Word (.docx)'], ['md', 'Markdown (.md)'], ['txt', 'Text (.txt)'], ['eml', 'Email (.eml)'], ['fkv', 'Flashcards (.fkv)'], ['jpg', 'Images (.jpg)']];

  // pickFormats asks which formats to make; null when cancelled.
  function pickFormats(name) {
    return new Promise(resolve => {
      const boxes = FORMATS.map(([value, label]) => {
        const box = el('input', { type: 'checkbox', value, checked: value === 'pdf' });
        const note = value === 'pdf' ? el('span', { class: 'note' }, 'to tablet') : '';
        return { box, row: el('label', { class: 'format' }, box, ' ', label, note) };
      });
      const start = el('button', { class: 'button primary', value: 'start' }, el('span', {}, 'Start'));
      // Nothing ticked, nothing to make.
      const ready = () => { start.disabled = !boxes.some(b => b.box.checked); };
      boxes.forEach(b => b.box.addEventListener('change', ready));
      const dialog = el('dialog', { class: 'formats' },
        el('form', { method: 'dialog' },
          el('p', {}, 'Transform “' + name + '”'),
          boxes.map(b => b.row),
          el('div', { class: 'actions' },
            el('button', { class: 'button', value: 'cancel' }, el('span', {}, 'Cancel')),
            start)));
      dialog.addEventListener('close', () => {
        dialog.remove();
        resolve(dialog.returnValue === 'start' ? boxes.filter(b => b.box.checked).map(b => b.box.value) : null);
      });
      document.body.append(dialog);
      dialog.showModal();
    });
  }

  async function transform(it, btn) {
    const formats = await pickFormats(it.name);
    if (!formats) return;
    btn.disabled = true;
    try {
      await api('/transforms', { method: 'POST', json: { document: it.id, formats, server: SERVER } });
      toast('Started transforming ' + it.name);
      listChats().catch(() => {});
    } catch (e) {
      toast("Couldn't start it: " + e.message);
    }
    loadTablet();
  }

  function forgetJob(j) {
    if (j.status === 'running' && !confirm('Cancel transforming “' + j.name + '”? Its agent can no longer submit anything.')) return;
    act('/transforms/' + encodeURIComponent(j.id), { method: 'DELETE' }, loadTablet);
  }

  // Flashcards on the tablet and the phone pairs with a code it shows, entered here.
  async function loadDevices() {
    let devices;
    try {
      devices = await api('/flashcards/devices');
    } catch (e) {
      return;
    }
    const box = $('#devices');
    box.hidden = !devices.length;
    box.replaceChildren(...devices.map((d) => row('tablet', d.name,
      'Paired ' + dated(d.added) + (d.seen && instant(d.seen) > 0 ? ' · last synced ' + dated(d.seen) : ''),
      button('Unpair', () => unpair(d), 'danger'))));
  }

  async function pair(e) {
    e.preventDefault();
    const input = $('#pair-code');
    const code = input.value.trim();
    if (!code) return;
    try {
      await api('/flashcards/pair/approve', { method: 'POST', json: { code } });
      input.value = '';
      toast('Paired. Its Flashcards is syncing.');
    } catch (err) {
      toast("Couldn't pair: " + err.message);
    }
    loadDevices();
  }

  function unpair(d) {
    if (!confirm('Unpair “' + d.name + '”? Its Flashcards stops syncing until it is paired again.')) return;
    act('/flashcards/devices/' + encodeURIComponent(d.id), { method: 'DELETE' }, loadDevices);
  }

  // ---------------------------------------------------------------------
  // QA: the nightly runs, the nightly report, findings and controls (see qa.go)
  // ---------------------------------------------------------------------

  const QA_STATUS = { open: 'Open', fixed: 'Fixed', passed: 'For you', declined: 'Declined', grown: 'Grown', reverted: 'Taken out' };
  const QA_APP_STATUS = { waiting: 'Waiting', running: 'Working', done: 'Done', skipped: 'Skipped' };
  const QA_ROLE = { spec: 'writing its spec', explore: 'exploring', fix: 'fixing', refactor: 'refactoring' };
  const qaOpen = new Set(); // apps whose findings are shown
  let qaRunning = null;

  const appLabel = (id) => { const d = id.split('/')[1] || id; return d.charAt(0).toUpperCase() + d.slice(1); };

  async function loadQA() {
    poll('qa');
    let o;
    try {
      o = await api('/qa');
    } catch (e) {
      empty($('#qa-apps'), "Couldn't read QA: " + e.message);
      return;
    }
    if (state.view !== 'qa') return;
    qaRunning = o.running;
    renderQASchedule(o);
    renderQALatest(o);
    renderQAApps(o);
    if (o.running) poll('qa', loadQA, 10000);
  }

  function renderQASchedule(o) {
    const s = o.settings;
    const on = el('input', { type: 'checkbox', checked: s.enabled, onchange: () => qaSettings({ enabled: on.checked }) });
    const hour = el('select', { onchange: () => qaSettings({ hour: Number(hour.value) }) },
      Array.from({ length: 24 }, (_, h) => el('option', { value: h, selected: h === s.hour },
        new Date(2000, 0, 1, h).toLocaleTimeString([], { hour: 'numeric' }))));
    const parts = [el('label', {}, on, 'Every day at'), hour, el('span', { class: 'note' }, s.time_zone)];
    if (!o.devices) parts.push(el('span', { class: 'note' }, 'Phone apps have no emulators yet: have an agent run delta qa setup.'));
    $('#qa-schedule').replaceChildren(...parts);
    const run = $('#qa-run');
    run.replaceChildren(el('span', {}, o.running ? 'Cancel run' : 'Run all now'));
    run.classList.toggle('primary', !o.running);
    run.classList.toggle('danger', !!o.running);
  }

  function renderQALatest(o) {
    const box = $('#qa-latest');
    const r = o.running || (o.runs || [])[0];
    $('#qa-run-head').hidden = !r;
    $('#qa-run-head').textContent = o.running ? 'Run in progress' : 'Nightly report';
    if (!r) { box.replaceChildren(); return; }
    const run = r.trigger === 'schedule' ? 'Scheduled run' : 'Run';
    if (!o.running) {
      const meta = run + ' · ' + dated(r.created) + (r.status === 'cancelled' ? ' · cancelled' : '');
      box.replaceChildren(el('div', { class: 'qa-summary' }, el('span', { class: 'meta' }, meta), r.summary || r.error || ''));
      return;
    }
    const apps = r.apps || [];
    const done = apps.filter((a) => a.status === 'done' || a.status === 'skipped').length;
    const stage = r.stage === 'refactor' ? 'Refactoring the repositories' : done + ' of ' + apps.length + ' apps through';
    const pct = r.stage === 'refactor' ? 100 : Math.round(100 * done / Math.max(1, apps.length));
    const rows = (r.jobs || []).filter((j) => j.status === 'running').map((j) =>
      row('refresh', (j.app ? appLabel(j.app) : j.repo) + ': ' + (QA_ROLE[j.role] || j.role), 'Since ' + dated(j.created),
        j.session ? chatButton(j.session) : null));
    box.replaceChildren(
      el('div', { class: 'qa-summary' }, el('span', { class: 'meta' }, run + ' going since ' + dated(r.created)), stage, jobBar(pct)),
      rows.length ? el('div', { class: 'rows qa-progress' }, rows) : '');
  }

  function renderQAApps(o) {
    const running = new Map(((o.running && o.running.apps) || []).map((a) => [a.app, a]));
    const rows = [];
    for (const a of o.apps) {
      const facts = [];
      if (!a.spec) facts.push('No spec yet');
      if (a.open) facts.push(a.open + ' open');
      if (a.passed) facts.push(a.passed + ' for you');
      if (a.grown) facts.push(a.grown + ' grown, awaiting your verdict');
      const now = running.get(a.id);
      if (now) facts.push(QA_APP_STATUS[now.status] + (now.why ? ': ' + now.why : ''));
      else if (a.explored) facts.push('Checked ' + dated(a.explored));
      const grow = button(a.grow ? 'Growing' : 'Grow', () => qaGrow(a, !a.grow), '',
        a.grow ? 'QA may grow features in this app: stop it' : 'Let QA propose and build new features in this app');
      const run = button('Run', () => qaStart([a.id]), '', 'Run QA on this app now');
      run.disabled = !!o.running;
      const open = qaOpen.has(a.id);
      const more = button(open ? 'Hide' : 'Findings', () => { open ? qaOpen.delete(a.id) : qaOpen.add(a.id); loadQA(); });
      const name = el('b', {}, appLabel(a.id), el('span', { class: 'tag' }, a.phone ? 'phone' : 'tablet'),
        a.grow ? el('span', { class: 'tag on' }, 'grows') : null);
      rows.push(row(a.phone ? 'file' : 'tablet', name, facts.join(' · ') || 'Nothing found yet', [grow, run, more]));
      if (open) {
        const box = empty(el('div', { class: 'qa-findings' }), 'Loading…');
        rows.push(box);
        qaFindings(a.id, box);
      }
    }
    $('#qa-apps').replaceChildren(...rows);
  }

  async function qaFindings(app, box) {
    let list;
    try {
      list = await api('/qa/findings?limit=100&app=' + encodeURIComponent(app));
    } catch (e) {
      empty(box, e.message);
      return;
    }
    if (!list.length) { empty(box, 'No findings yet.'); return; }
    box.replaceChildren(...list.map((f) => {
      const d = el('details', { class: 'qa-finding' },
        el('summary', {}, el('span', { class: 'st ' + f.status }, QA_STATUS[f.status] || f.status),
          el('span', {}, f.title, el('span', { class: 'tag' }, f.kind))));
      if (f.reason) d.append(el('div', { class: 'why' }, f.reason + (f.commit ? ' (' + f.commit.slice(0, 7) + ')' : '')));
      if (f.status === 'grown') {
        const pick = (v) => qaVerdict(f, f.verdict === v ? '' : v);
        d.append(el('div', { class: 'verdict' },
          button('👍 Keep', () => pick('up'), f.verdict === 'up' ? 'chosen' : ''),
          button('👎 Take out', () => pick('down'), f.verdict === 'down' ? 'chosen' : '')));
      }
      d.addEventListener('toggle', () => {
        if (!d.open || d.dataset.filled) return;
        d.dataset.filled = '1';
        d.append(el('pre', {}, f.detail));
        if (f.shots && f.shots.length) {
          const shots = el('div', { class: 'shots' });
          d.append(shots);
          for (const name of f.shots) {
            api('/qa/shots/' + encodeURIComponent(name), { raw: true }).then((r) => r.blob())
              .then((b) => shots.append(el('img', { src: URL.createObjectURL(b), alt: 'Screenshot' }))).catch(() => {});
          }
        }
      });
      return d;
    }));
  }

  function qaSettings(change) {
    act('/qa/settings', { method: 'PUT', json: { ...change, server: SERVER } }, loadQA);
  }

  function qaGrow(a, grow) {
    act('/qa/apps/' + a.id.split('/').map(encodeURIComponent).join('/'), { method: 'PUT', json: { grow } }, loadQA,
      grow ? appLabel(a.id) + ' may grow now' : appLabel(a.id) + ' no longer grows');
  }

  function qaStart(apps) {
    act('/qa/runs', { method: 'POST', json: { apps, server: SERVER } }, loadQA,
      apps.length ? 'QA started on ' + apps.map(appLabel).join(', ') : 'QA started on every app', "Couldn't start: ");
  }

  function qaVerdict(f, verdict) {
    act('/qa/findings/' + encodeURIComponent(f.id) + '/verdict', { method: 'PUT', json: { verdict } }, loadQA,
      verdict === 'down' ? 'The next run takes it out' : '');
  }

  async function qaRunButton() {
    if (!qaRunning) return qaStart([]);
    if (!confirm('Cancel the QA run? Its agents stop where they are.')) return;
    act('/qa/runs/' + encodeURIComponent(qaRunning.id), { method: 'DELETE' }, loadQA);
  }

  // ---------------------------------------------------------------------
  // Accounts
  // ---------------------------------------------------------------------

  const planName = (p) => p ? p.replace(/^claude_/, '').replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase()) : '';
  const accountLabel = (a) => accountName(a.id) + (a.plan ? ' · ' + planName(a.plan) : '');

  async function loadUsage() {
    const box = $('#accounts');
    loading(box);
    let accounts;
    try {
      accounts = await loadAccounts();
    } catch (e) {
      empty(box, "Couldn't list the accounts: " + e.message);
      return;
    }
    if (!accounts.length) { empty(box, 'No accounts yet. An agent can add one with `delta accounts login`.'); return; }
    const heads = new Map();
    const cards = accounts.map((a) => {
      const usage = el('div', {}, el('div', { class: 'meter-label' }, 'Loading usage…'));
      const right = el('span', {}, planName(a.plan));
      const head = el('div', { class: 'card-head' }, el('b', {}, accountName(a.id)), right);
      heads.set(a.id, head);
      const chats = state.chats.filter((c) => c.account === a.id).length;
      const card = el('div', { class: 'card' }, head, el('div', { class: 'card-id', title: 'Account UUID' }, a.id), usage,
        chats ? el('div', { class: 'card-foot' }, chats + (chats === 1 ? ' chat' : ' chats') + ' here') : null);
      return { card, done: checkAccount(a.id).then((s) => {
        if (s.dead) throw Object.assign(new Error(''), { status: 403 });
        if (s.unread) throw new Error(s.error);
        if (s.until > Date.now()) { right.className = 'out'; right.textContent = 'Out until ' + when(s.until); }
        else if (s.limited > Date.now()) { right.className = 'credits'; right.textContent = 'Limits reached · on credits'; }
        usage.replaceChildren(meter('5-hour', s.report.five_hour), meter('Weekly', s.report.seven_day), creditMeter(s.credits));
      }).catch((e) => {
        usage.replaceChildren(el('div', { class: 'meter-label error' }, e.status === 403 ? 'Its login no longer works. Sign it in again with `delta accounts login`.' : "Couldn't get its usage: " + e.message));
      }) };
    });
    if (state.view === 'accounts') box.replaceChildren(...cards.map((c) => c.card));
    await Promise.all(cards.map((c) => c.done));
    const chosen = chosenAccount();
    const next = chosen || (ranked()[0] || {}).id;
    const head = next && heads.get(next);
    if (head) head.firstChild.after(el('span', { class: 'next', title: chosen ? 'You chose it in the composer' : 'The best account now' }, chosen ? 'New chats (chosen)' : 'Next new chat'));
    renderComposer();
  }

  /** A line on an account's card: what it measures, how it stands, and a bar `used`% full, unless there is none. */
  function gauge(label, text, used, full) {
    return el('div', { class: 'meter' },
      el('div', { class: 'meter-label' }, el('b', {}, label), el('span', {}, text)),
      used === undefined ? null : el('div', { class: 'bar' }, el('div', { class: full ? 'full' : '', style: 'width:' + used + '%' })));
  }

  function creditMeter(c) {
    if (!c) return gauge('Cloud credits', 'None');
    const used = c.limit > 0 ? Math.max(0, Math.min(100, c.used / c.limit * 100)) : 100;
    return gauge('Cloud credits', dollars(c.left) + ' of ' + dollars(c.limit) + ' left' + (c.resets ? ' · resets ' + when(c.resets) : ''), used, c.left <= 0);
  }

  function meter(label, w) {
    const used = w && typeof w.utilization === 'number' ? Math.max(0, Math.min(100, w.utilization)) : null;
    const resets = w && instant(w.resets_at);
    return gauge(label, used === null ? 'No data' : Math.round(used) + '% used' + (resets ? ' · resets ' + when(resets) : ''), used || 0, used >= 100);
  }

  // ---------------------------------------------------------------------
  // Signing in
  // ---------------------------------------------------------------------

  function signOut(why) {
    token = null;
    stored('delta.token', null);
    for (const s of state.sessions.values()) if (s.abort) s.abort.abort();
    state.sessions.clear();
    state.open = null;
    state.chats = [];
    $('#app').hidden = true;
    $('#signin').hidden = false;
    const err = $('#signin-error');
    err.hidden = !why;
    err.textContent = why || '';
    $('#signin-token').focus();
  }

  const checkToken = (t) => fetch(url('/auth'), { headers: { Authorization: 'Bearer ' + t } });

  async function signIn(candidate) {
    const resp = await checkToken(candidate);
    if (resp.status === 401) throw new Error("That token isn't right.");
    if (!resp.ok) throw new Error("Couldn't reach the command center (HTTP " + resp.status + ').');
    const info = await resp.json();
    if (info.scope && info.scope !== 'full') throw new Error("That token can't run the website.");
    token = candidate;
    stored('delta.token', candidate);
  }

  function start() {
    $('#signin').hidden = true;
    $('#app').hidden = false;
    if (stored('delta.collapsed') === '1' && innerWidth > 760) $('#app').classList.add('collapsed');
    listChats().catch((e) => { state.warning = "Couldn't list the chats: " + e.message; renderChats(); });
    route();
  }

  // ---------------------------------------------------------------------
  // Wiring
  // ---------------------------------------------------------------------

  $('#signin-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const err = $('#signin-error');
    err.hidden = true;
    try {
      await signIn($('#signin-token').value.trim());
      $('#signin-token').value = '';
      start();
    } catch (x) {
      err.hidden = false;
      err.textContent = x.message;
    }
  });
  $('#signout').addEventListener('click', () => { if (confirm('Sign out of Delta on this browser?')) signOut(); });
  $('#pair').addEventListener('submit', pair);
  $('#qa-run').addEventListener('click', qaRunButton);
  $('#refresh').addEventListener('click', () => listChats().catch((e) => toast(e.message)));
  $('#collapse').addEventListener('click', () => { $('#app').classList.add('collapsed'); stored('delta.collapsed', '1'); });
  $('#expand').addEventListener('click', () => {
    if (innerWidth <= 760) $('#app').classList.add('drawer');
    else { $('#app').classList.remove('collapsed'); stored('delta.collapsed', null); }
  });
  $('#scrim').addEventListener('click', () => $('#app').classList.remove('drawer'));
  document.querySelectorAll('.nav-item[data-view]').forEach((a) => a.addEventListener('click', () => {
    // A second click on New chat while there clears nothing but closes the drawer.
    $('#app').classList.remove('drawer');
  }));

  const input = $('#input');
  input.addEventListener('input', () => { autosize(); renderComposer(); });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    } else if (e.key === 'Escape' && state.open && state.open.t.working) {
      e.preventDefault();
      interrupt();
    }
  });
  input.addEventListener('paste', (e) => {
    const files = Array.from(e.clipboardData ? e.clipboardData.files : []);
    if (files.length) { e.preventDefault(); attach(files); }
  });
  $('#composer').addEventListener('submit', (e) => {
    e.preventDefault();
    if ($('#send').dataset.stop) interrupt(); else send();
  });
  $('#attach').addEventListener('click', () => $('#file-input').click());
  $('#file-input').addEventListener('change', (e) => { attach(Array.from(e.target.files)); e.target.value = ''; });

  // The kind of a new chat, from the composer's menu or the hero's copy of it.
  const checkKind = (menu) => menu.querySelectorAll('[data-kind]').forEach((b) => b.classList.toggle('checked', b.dataset.kind === state.kind));
  function pickKind(e) {
    const b = e.target.closest('[data-kind]');
    if (!b) return;
    state.kind = b.dataset.kind;
    stored('delta.kind', state.kind);
    closeMenus();
    renderComposer();
    input.focus();
  }
  $('#kind-btn').addEventListener('click', (e) => {
    e.stopPropagation();
    if (state.view !== 'new') return;
    const menu = $('#kind-menu');
    const wasHidden = menu.hidden;
    closeMenus();
    menu.hidden = !wasHidden;
    checkKind(menu);
  });
  $('#kind-menu').addEventListener('click', pickKind);
  $('#account-btn').addEventListener('click', (e) => { e.stopPropagation(); accountMenu(e.currentTarget); });
  // From the hero, the same choice opens under it, as Codex's does.
  $('#hero-kind').addEventListener('click', (e) => {
    e.stopPropagation();
    const anchor = e.currentTarget;
    const menu = $('#kind-menu').cloneNode(true);
    closeMenus();
    menu.removeAttribute('id');
    menu.hidden = false;
    menu.classList.remove('up');
    menu.classList.add('floating');
    checkKind(menu);
    menu.addEventListener('click', pickKind);
    document.body.append(menu);
    const r = anchor.getBoundingClientRect();
    menu.style.position = 'fixed';
    menu.style.top = r.bottom + 6 + 'px';
    menu.style.left = Math.max(8, r.left + r.width / 2 - menu.offsetWidth / 2) + 'px';
  });
  document.addEventListener('click', (e) => {
    if (!e.target.closest('.menu')) closeMenus();
    const copy = e.target.closest('[data-copy]');
    if (copy) {
      const code = copy.closest('.code').querySelector('code').textContent;
      navigator.clipboard.writeText(code).then(() => {
        copy.textContent = 'Copied';
        setTimeout(() => { copy.textContent = 'Copy'; }, 1400);
      });
    }
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closeMenus();
  });

  $('#upload').addEventListener('click', () => $('#upload-input').click());
  $('#upload-input').addEventListener('change', (e) => { const f = Array.from(e.target.files); e.target.value = ''; if (f.length) uploadFiles(f); });

  // Dropping files: onto Files uploads them, anywhere else attaches them.
  let dragDepth = 0;
  const dragging = (e) => e.dataTransfer && Array.from(e.dataTransfer.types).includes('Files');
  document.addEventListener('dragenter', (e) => { if (!dragging(e) || !token) return; dragDepth++; $('#dropzone').hidden = false; $('#dropzone div').textContent = state.view === 'files' ? 'Drop files to upload' : 'Drop files to attach'; });
  document.addEventListener('dragleave', (e) => { if (!dragging(e)) return; if (--dragDepth <= 0) { dragDepth = 0; $('#dropzone').hidden = true; } });
  document.addEventListener('dragover', (e) => { if (dragging(e)) e.preventDefault(); });
  document.addEventListener('drop', (e) => {
    if (!dragging(e)) return;
    e.preventDefault();
    dragDepth = 0;
    $('#dropzone').hidden = true;
    const files = Array.from(e.dataTransfer.files);
    if (!files.length || !token) return;
    if (state.view === 'files') uploadFiles(files);
    else if (state.view === 'accounts') toast('Open a chat to attach files');
    else attach(files);
  });

  window.addEventListener('hashchange', () => { if (token) route(); });

  // The sidebar's times and statuses, kept current.
  setInterval(() => { if (token && !document.hidden) listChats().catch(() => {}); }, 20000);
  document.addEventListener('visibilitychange', () => { if (token && !document.hidden) listChats().catch(() => {}); });
  setInterval(() => { if (token && state.open) renderComposer(); }, 30000);

  if (token) {
    checkToken(token).then((r) => {
      if (r.status === 401) signOut('Your saved token no longer works. Sign in again.');
      else start();
    }).catch(() => start());
  } else {
    signOut();
  }
})();
