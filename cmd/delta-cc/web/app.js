// Delta in a browser: the chats, Files, Tablet and Accounts, laid out like
// the Codex app. It is served by the command center and
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
  const CARRY_NOTE = 'Continued from an earlier session whose account reached its usage limit.';

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
  const esc = Markdown.esc;
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

  function problem(data, text, status) {
    if (data && typeof data.error === 'string') return data.error;
    if (data && data.error && data.error.message) return rebrand(data.error.message);
    if (status === 429) return 'Too many requests; try again shortly.';
    return text && text.length < 200 && !/^\s*</.test(text) ? text.trim() : 'HTTP ' + status;
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
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
    if (!resp.ok) throw new HttpError(resp.status, problem(data, text, resp.status));
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

  async function loadAccounts() {
    state.accounts = await api('/cloud-accounts');
    return state.accounts;
  }

  /** The first account, in the listed order, not known to be out (and not `except`). */
  async function pickAccount(except) {
    const accounts = await loadAccounts();
    if (!accounts.length) throw new Error('No accounts are set up on the command center yet. Add one with `delta accounts login`.');
    const known = limits();
    let soonest = 0;
    for (const a of accounts) {
      if (a.id === except) continue;
      if (known[a.id] > Date.now()) { soonest = soonest ? Math.min(soonest, known[a.id]) : known[a.id]; continue; }
      try {
        const until = limitedUntil(await api('/cloud-accounts/' + encodeURIComponent(a.id) + '/usage'));
        if (until > Date.now()) {
          noteLimit(a.id, until);
          soonest = soonest ? Math.min(soonest, until) : until;
          continue;
        }
      } catch (e) {
        if (e.status === 403) continue; // its login no longer works
      }
      return a.id;
    }
    throw new Error((except ? 'Every other account' : 'Every account') + ' is at its usage limit' + (soonest ? ' until ' + when(soonest) : '') + '. A backup session (▲) works meanwhile.');
  }

  /** Whether another account than `account` isn't known to be out, so a chat can move there. */
  function canMove(account) {
    const known = limits();
    return state.accounts.some((a) => a.id !== account && !(known[a.id] > Date.now()));
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
        const open = state.view === 'chat' && state.open && chats.find((c) => c.id === state.open.id);
        if (open) setTitle(chatTitle(open), open.backup ? 'backup' : 'cloud');
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
      const kind = c.backup ? 'backup' : 'cloud';
      const status = c.status === 'running' ? el('span', { class: 'busy', title: 'Working' })
        : c.status === 'requires_action' ? el('span', { class: 'needs', title: 'Waiting for you' })
        : el('span', { class: 'when' }, ago(c.updated));
      const more = el('button', {
        type: 'button', class: 'icon-btn small more', title: 'More',
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); chatMenu(c, e.currentTarget); },
      }, icon('more'));
      list.append(el('li', {}, el('a', {
        class: 'chat' + (active ? ' active' : ''), href: '#/s/' + c.id, title: chatTitle(c),
      }, el('span', { class: 'mark ' + kind }, MARK[kind]), el('span', { class: 'name' }, chatTitle(c)), status, more)));
    }
  }

  function closeMenus() {
    document.querySelectorAll('.menu.floating').forEach((m) => m.remove());
    document.querySelectorAll('.menu-open').forEach((m) => m.classList.remove('menu-open'));
    $('#kind-menu').hidden = true;
  }

  function floatingMenu(anchor, items) {
    closeMenus();
    const menu = el('div', { class: 'menu floating' });
    for (const item of items) {
      menu.append(el('button', {
        type: 'button', class: 'menu-item' + (item.danger ? ' danger' : ''),
        onclick: (e) => { e.stopPropagation(); closeMenus(); item.run(); },
      }, item.label));
    }
    document.body.append(menu);
    const r = anchor.getBoundingClientRect();
    menu.style.position = 'fixed';
    menu.style.minWidth = '170px';
    menu.style.top = Math.min(r.bottom + 4, innerHeight - menu.offsetHeight - 8) + 'px';
    menu.style.left = Math.max(8, Math.min(r.right - menu.offsetWidth, innerWidth - menu.offsetWidth - 8)) + 'px';
    return menu;
  }

  function chatMenu(c, anchor) {
    anchor.closest('.chat') && anchor.closest('.chat').classList.add('menu-open');
    floatingMenu(anchor, [c.backup
      ? { label: 'Delete', danger: true, run: () => removeChat(c) }
      : { label: 'Archive', run: () => removeChat(c) }]);
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

  function textOf(content) {
    if (typeof content === 'string') return content;
    if (!Array.isArray(content)) return '';
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

  function resultText(content) {
    if (typeof content === 'string') return content;
    if (!Array.isArray(content)) return '';
    return content.map((b) => b && b.type === 'text' ? b.text : b && b.type === 'image' ? '[picture]' : '').filter(Boolean).join('\n');
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
          call.result = resultText(b.content);
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
      if (info.status === 'rejected') {
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
      'The complete conversation of an earlier Delta session, which stopped because the account it ' +
      'ran on reached its usage limit: every message, every tool call with its full input, and every ' +
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
      ' that stopped because the account it ran on reached its usage limit; this session runs on ' +
      'another account. The whole conversation so far, every message, tool call and tool result in ' +
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
          if (!resp.ok) {
            const text = await resp.text();
            let data = null;
            try { data = JSON.parse(text); } catch (e) { /* not JSON */ }
            throw new HttpError(resp.status, problem(data, text, resp.status));
          }
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
    const listed = state.chats.find((c) => c.id === chat.id);
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
    switch (n.toLowerCase()) {
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
    const n = call.name.replace(/^mcp__/, '').toLowerCase();
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
        const lower = e.name.replace(/^mcp__/, '').toLowerCase();
        if (lower === 'todowrite' && Array.isArray(e.input.todos)) {
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
   * instead, and what was typed follows the conversation it carries.
   */
  async function send(move) {
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
        await moveChat(chat, typed, message, id, jpegs, images, names);
        input.value = '';
        state.attachments = [];
        renderAttachments();
        autosize();
      } else if (chat) {
        chat.t.sent(id, typed, images, names);
        input.value = '';
        state.attachments = [];
        renderAttachments();
        autosize();
        renderChat();
        toBottom();
        try {
          if (chat.backup) await api('/agent-sessions/' + chat.id + '/messages', { method: 'POST', json: { uuid: id, text: message, images: jpegs } });
          else await postEvents(chat, [modeRequest(), userEvent(id, message, jpegs, cse(chat.id))]);
          const listed = state.chats.find((c) => c.id === chat.id);
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
        input.value = '';
        state.attachments = [];
        renderAttachments();
        autosize();
      }
    } catch (e) {
      toast(e.message);
    } finally {
      state.busy = false;
      renderComposer();
    }
  }

  /**
   * Starts a new chat with the message. `carry` ({title, block, pictures,
   * except}) makes it the continuation of a moved one: a regular session on
   * an account other than `except`, whose first message opens with the block.
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
        const account = await pickAccount(carry && carry.except);
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
        chat.t.note(CARRY_NOTE);
      }
      if (!carry || typed || images.length || names.length) chat.t.sent(id, typed, images, names);
      else chat.t.working = true;
      state.sessions.set(chat.id, chat);
      state.chats.unshift({ id: chat.id, title, status: 'running', updated: Date.now(), backup: chat.backup, account: chat.account, fresh: true });
      location.hash = '#/s/' + chat.id;
    } finally {
      $('#hero').querySelector('h1').textContent = "New chat";
    }
  }

  /** Carries `chat`, whose account is out, to a new session on another account (see carryText). */
  async function moveChat(chat, typed, message, id, jpegs, images, names) {
    if (chat.t.limitedUntil > Date.now()) noteLimit(chat.account, chat.t.limitedUntil);
    const listed = state.chats.find((c) => c.id === chat.id);
    const title = chatTitle(listed || chat);
    chat.moving = true;
    renderComposer();
    try {
      const carried = carryText(title, await history(chat));
      const stored = await api('/conversations', { method: 'POST', body: carried.text, headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
      if (!stored || !stored.id) throw new Error("The command center didn't store the conversation");
      await create(typed, message, id, jpegs, images, names,
        { title, block: carryBlock(title, stored.id), pictures: carried.pictures, except: chat.account });
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

  function renderComposer() {
    const chat = state.view === 'chat' ? state.open : null;
    const kind = chat ? (chat.backup ? 'backup' : 'cloud') : state.kind;
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

    const banner = $('#banner');
    const limited = chat && !chat.backup && chat.t.limitedUntil > Date.now();
    banner.hidden = !limited;
    banner.replaceChildren();
    if (limited) {
      const movable = canMove(chat.account);
      banner.append(el('span', {}, "This chat's account has reached its usage limit until " + when(chat.t.limitedUntil) + '.' +
        (movable ? ' Move the chat to another account to carry on now; anything typed below goes with it.' : ' Every other account is at its limit too.')));
      if (movable) {
        banner.append(el('button', { type: 'button', class: 'button primary', disabled: state.busy || !!chat.moving, onclick: () => send(true) },
          chat.moving ? 'Moving…' : 'Move to another account'));
      }
    }
  }

  // ---------------------------------------------------------------------
  // Views
  // ---------------------------------------------------------------------

  function show(view) {
    state.view = view;
    $('#view-chat').hidden = !(view === 'chat' || view === 'new');
    $('#view-files').hidden = view !== 'files';
    $('#view-tablet').hidden = view !== 'tablet';
    $('#view-accounts').hidden = view !== 'accounts';
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
    const more = el('button', { type: 'button', class: 'icon-btn ghost', title: 'More' }, icon('more'));
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      const listed = state.chats.find((c) => c.id === chat.id) || chat;
      floatingMenu(more, [chat.backup
        ? { label: 'Delete chat', danger: true, run: () => removeChat(listed) }
        : { label: 'Archive chat', run: () => removeChat(listed) }]);
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
    if (hash === 'files') {
      show('files');
      setTitle('');
      topActions(null);
      loadFiles();
    } else if (hash === 'tablet') {
      show('tablet');
      setTitle('');
      topActions(null);
      loadTablet();
    } else if (hash === 'accounts') {
      show('accounts');
      setTitle('');
      topActions(null);
      loadUsage();
    } else {
      show('new');
      setTitle('');
      topActions(null);
      $('#hero').hidden = false;
      renderChat();
      renderComposer();
      $('#input').focus();
    }
  }

  async function openChat(id) {
    if (!validId(id)) { location.hash = '#/'; return; }
    if (state.open && state.open.id === id && state.open.abort) return;
    leaveChat();
    let chat = state.sessions.get(id);
    if (!chat) {
      let listed = state.chats.find((c) => c.id === id);
      if (!listed && !isBackup(id)) {
        if (!state.listed || state.listing) await listChats().catch(() => {});
        listed = state.chats.find((c) => c.id === id);
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
    const listed = state.chats.find((c) => c.id === id);
    setTitle(chatTitle(listed || chat), chat.backup ? 'backup' : 'cloud');
    topActions(chat);
    $('#transcript').replaceChildren();
    renderChat();
    toBottom();
    connect(chat);
    $('#input').focus();
  }

  // ---------------------------------------------------------------------
  // Files
  // ---------------------------------------------------------------------

  async function loadFiles() {
    const box = $('#files');
    if (!box.childNodes.length) box.append(el('div', { class: 'empty' }, 'Loading…'));
    let files;
    try {
      files = await api('/files');
    } catch (e) {
      box.replaceChildren(el('div', { class: 'empty' }, "Couldn't list the files: " + e.message));
      return;
    }
    if (state.view !== 'files') return;
    if (!files.length) {
      box.replaceChildren(el('div', { class: 'empty' }, 'No files yet. What agents push shows up here.'));
      return;
    }
    box.replaceChildren(...files.map((f) => el('div', { class: 'row' },
      el('span', { class: 'icon' }, icon('file')),
      el('span', { class: 'label' }, el('b', { title: f.name }, f.name), el('small', {}, size(f.size) + ' · ' + when(instant(f.modified)))),
      el('span', { class: 'actions' },
        el('button', { type: 'button', class: 'button', title: 'Attach to your next message', onclick: () => sendToAgent(f.name) }, el('span', {}, 'Send to agent')),
        el('button', { type: 'button', class: 'button', onclick: () => download(f.name) }, el('span', {}, 'Download')),
        el('button', { type: 'button', class: 'button danger', onclick: () => deleteFile(f.name) }, el('span', {}, 'Delete'))))));
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
  // Tablet: transform jobs on the reMarkable's documents
  // ---------------------------------------------------------------------

  const DOC_KIND = { pdf: 'PDF', epub: 'EPUB', notebook: 'Notebook' };
  const JOB_STATUS = { running: 'Working on it…', done: 'Done', failed: 'Failed' };
  let jobsTimer = 0;

  async function loadTablet() {
    clearTimeout(jobsTimer);
    loadDevices();
    const box = $('#tablet');
    if (!box.childNodes.length) box.append(el('div', { class: 'empty' }, 'Loading…'));
    let items, jobs;
    try {
      [items, jobs] = await Promise.all([api('/remarkable'), api('/transforms')]);
    } catch (e) {
      box.replaceChildren(el('div', { class: 'empty' }, "Couldn't read the tablet: " + e.message));
      return;
    }
    if (state.view !== 'tablet') return;
    renderJobs(jobs);
    if (jobs.some((j) => j.status === 'running')) jobsTimer = setTimeout(() => { if (state.view === 'tablet') loadTablet(); }, 5000);

    const byId = new Map(items.map((it) => [it.id, it]));
    const where = (it) => {
      const path = [];
      for (let p = it.parent, n = 0; p && n < 50; n++) {
        const f = byId.get(p);
        if (!f) break;
        path.unshift(f.name);
        p = f.parent;
      }
      return path.join(' / ');
    };
    const docs = items.filter((it) => it.type !== 'folder' && it.parent !== 'trash' && !inTrash(it, byId))
      .sort((a, b) => instant(b.modified) - instant(a.modified));
    if (!docs.length) {
      box.replaceChildren(el('div', { class: 'empty' }, 'Nothing on the tablet yet.'));
      return;
    }
    const busy = new Set(jobs.filter((j) => j.status === 'running').map((j) => j.document));
    box.replaceChildren(...docs.map((it) => {
      const folder = where(it);
      const facts = [DOC_KIND[it.type] || it.type];
      if (it.pages) facts.push(it.pages + (it.pages === 1 ? ' page' : ' pages'));
      if (folder) facts.push(folder);
      if (it.modified) facts.push(when(instant(it.modified)));
      const button = el('button', { type: 'button', class: 'button', onclick: () => transform(it, button) }, el('span', {}, busy.has(it.id) ? 'Transforming…' : 'Transform'));
      button.disabled = busy.has(it.id);
      return el('div', { class: 'row' },
        el('span', { class: 'icon' }, icon('file')),
        el('span', { class: 'label' }, el('b', { title: it.name }, it.name), el('small', {}, facts.join(' · '))),
        el('span', { class: 'actions' }, button));
    }));
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
    box.replaceChildren(...devices.map((d) => el('div', { class: 'row' },
      el('span', { class: 'icon' }, icon('tablet')),
      el('span', { class: 'label' }, el('b', {}, d.name),
        el('small', {}, 'Paired ' + when(instant(d.added)) + (d.seen && instant(d.seen) > 0 ? ' · last synced ' + when(instant(d.seen)) : ''))),
      el('span', { class: 'actions' }, el('button', { type: 'button', class: 'button danger', onclick: () => unpair(d) }, el('span', {}, 'Unpair'))))));
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

  async function unpair(d) {
    if (!confirm('Unpair “' + d.name + '”? Its Flashcards stops syncing until it is paired again.')) return;
    try {
      await api('/flashcards/devices/' + encodeURIComponent(d.id), { method: 'DELETE' });
    } catch (e) {
      toast(e.message);
    }
    loadDevices();
  }

  function inTrash(it, byId) {
    for (let p = it.parent, n = 0; p && n < 50; n++) {
      if (p === 'trash') return true;
      const f = byId.get(p);
      if (!f) return false;
      p = f.parent;
    }
    return false;
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
      // The agent's title for the job, once it has given one, over the document it came from.
      const label = el('span', { class: 'label' }, el('b', { title: j.title || j.name }, j.title || j.name),
        el('small', { class: j.status === 'failed' ? 'failed' : '' }, (j.title ? 'From ' + j.name + ' · ' : '') + detail + ' · ' + when(instant(j.created))));
      if (running) label.append(el('span', { class: 'job-bar' }, el('span', { style: 'width:' + percent + '%' })));
      const actions = el('span', { class: 'actions' });
      for (const name of j.files || []) {
        actions.append(el('button', { type: 'button', class: 'button', title: 'Download ' + name, onclick: () => download(name) },
          el('span', {}, name.toLowerCase().endsWith('.docx') ? 'Word' : 'PDF')));
      }
      if (j.session && j.status !== 'done') {
        actions.append(el('button', { type: 'button', class: 'button', title: 'See what the agent is doing', onclick: () => { location.hash = '#/s/' + j.session; } }, el('span', {}, 'Open chat')));
      }
      actions.append(el('button', { type: 'button', class: 'button danger', title: j.status === 'running' ? 'Stop waiting for it' : 'Remove from the list', onclick: () => forgetJob(j) },
        el('span', {}, j.status === 'running' ? 'Cancel' : 'Remove')));
      return el('div', { class: 'row' },
        el('span', { class: 'icon' }, icon(running ? 'refresh' : 'file')),
        label,
        actions);
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

  async function transform(it, button) {
    const formats = await pickFormats(it.name);
    if (!formats) return;
    button.disabled = true;
    try {
      await api('/transforms', { method: 'POST', json: { document: it.id, formats, server: SERVER } });
      toast('Started transforming ' + it.name);
      listChats().catch(() => {});
    } catch (e) {
      toast("Couldn't start it: " + e.message);
    }
    loadTablet();
  }

  async function forgetJob(j) {
    if (j.status === 'running' && !confirm('Cancel transforming “' + j.name + '”? Its agent can no longer submit anything.')) return;
    try {
      await api('/transforms/' + encodeURIComponent(j.id), { method: 'DELETE' });
    } catch (e) {
      toast(e.message);
    }
    loadTablet();
  }

  // ---------------------------------------------------------------------
  // Accounts
  // ---------------------------------------------------------------------

  const planName = (p) => p ? p.replace(/^claude_/, '').replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase()) : '';

  async function loadUsage() {
    const box = $('#accounts');
    if (!box.childNodes.length) box.append(el('div', { class: 'empty' }, 'Loading…'));
    let accounts;
    try {
      accounts = await loadAccounts();
    } catch (e) {
      box.replaceChildren(el('div', { class: 'empty' }, "Couldn't list the accounts: " + e.message));
      return;
    }
    if (!accounts.length) {
      box.replaceChildren(el('div', { class: 'empty' }, 'No accounts yet. An agent can add one with `delta accounts login`.'));
      return;
    }
    const cards = accounts.map((a) => {
      const usage = el('div', {}, el('div', { class: 'meter-label' }, 'Loading usage…'));
      const right = el('span', {}, planName(a.plan));
      const card = el('div', { class: 'card' }, el('div', { class: 'card-head' }, el('b', {}, a.email || a.id), right), usage);
      api('/cloud-accounts/' + encodeURIComponent(a.id) + '/usage').then((report) => {
        const until = limitedUntil(report);
        noteLimit(a.id, until);
        if (until > Date.now()) { right.className = 'out'; right.textContent = 'Out until ' + when(until); }
        usage.replaceChildren(meter('5-hour', report.five_hour), meter('Weekly', report.seven_day));
      }).catch((e) => {
        usage.replaceChildren(el('div', { class: 'meter-label error' }, e.status === 403 ? 'Its login no longer works. Sign it in again with `delta accounts login`.' : "Couldn't get its usage: " + e.message));
      });
      return card;
    });
    if (state.view === 'accounts') box.replaceChildren(...cards);
  }

  function meter(label, w) {
    const used = w && typeof w.utilization === 'number' ? Math.max(0, Math.min(100, w.utilization)) : null;
    const resets = w && instant(w.resets_at);
    return el('div', { class: 'meter' },
      el('div', { class: 'meter-label' }, el('b', {}, label), el('span', {}, used === null ? 'No data' : Math.round(used) + '% used' + (resets ? ' · resets ' + when(resets) : ''))),
      el('div', { class: 'bar' }, el('div', { class: used >= 100 ? 'full' : '', style: 'width:' + (used || 0) + '%' })));
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

  async function signIn(candidate) {
    const resp = await fetch(url('/auth'), { headers: { Authorization: 'Bearer ' + candidate } });
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

  function toggleKindMenu(e) {
    e.stopPropagation();
    if (state.view !== 'new') return;
    const menu = $('#kind-menu');
    const wasHidden = menu.hidden;
    closeMenus();
    menu.hidden = !wasHidden;
    menu.querySelectorAll('[data-kind]').forEach((b) => b.classList.toggle('checked', b.dataset.kind === state.kind));
  }
  $('#kind-btn').addEventListener('click', toggleKindMenu);
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
    menu.querySelectorAll('[data-kind]').forEach((b) => b.classList.toggle('checked', b.dataset.kind === state.kind));
    menu.addEventListener('click', (x) => {
      const b = x.target.closest('[data-kind]');
      if (!b) return;
      state.kind = b.dataset.kind;
      stored('delta.kind', state.kind);
      closeMenus();
      renderComposer();
      input.focus();
    });
    document.body.append(menu);
    const r = anchor.getBoundingClientRect();
    menu.style.position = 'fixed';
    menu.style.top = r.bottom + 6 + 'px';
    menu.style.left = Math.max(8, r.left + r.width / 2 - menu.offsetWidth / 2) + 'px';
  });
  $('#kind-menu').addEventListener('click', (e) => {
    const b = e.target.closest('[data-kind]');
    if (!b) return;
    state.kind = b.dataset.kind;
    stored('delta.kind', state.kind);
    closeMenus();
    renderComposer();
    input.focus();
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
    fetch(url('/auth'), { headers: { Authorization: 'Bearer ' + token } }).then((r) => {
      if (r.status === 401) signOut('Your saved token no longer works. Sign in again.');
      else start();
    }).catch(() => start());
  } else {
    signOut();
  }
})();
