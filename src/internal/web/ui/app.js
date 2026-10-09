'use strict';

(() => {
  const API = '/_helmo/api';
  const STATUS_EVERY_MS = 5000;
  const JOB_EVERY_MS = 1500;
  const MAX_LOG_LINES = 1500;
  const VERSIONS_SHOWN = 10; // newest versions offered for deploy
  const LOG_RETRY_MIN_MS = 2000;
  const LOG_RETRY_MAX_MS = 30000;

  const $ = (sel) => document.querySelector(sel);
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  };

  let jobRunning = false;
  let opRunning = false;

  // ---- helpers ------------------------------------------------------------

  function show(text, kind = 'info') {
    const m = $('#message');
    m.textContent = text || '';
    m.className = 'message ' + kind;
    m.hidden = !text;
  }

  async function api(path, opts = {}) {
    const res = await fetch(API + path, { credentials: 'same-origin', cache: 'no-store', ...opts });
    const text = await res.text();
    let body = null;
    try { body = JSON.parse(text); } catch (_) { /* plain-text error */ }
    if (!res.ok) {
      const err = new Error((body && (body.error || body.output)) || text.trim() || res.statusText);
      err.status = res.status;
      err.output = body && body.output;
      throw err;
    }
    return body;
  }

  function post(path, params) {
    const opts = { method: 'POST' };
    if (params) opts.body = new URLSearchParams(params);
    return api(path, opts);
  }

  function formatVersion(v) {
    if (!v) return '–';
    const [tag, digest] = v.split('@');
    return digest ? `${tag} (${digest.replace('sha256:', '').slice(0, 12)})` : tag;
  }

  function formatTime(iso) {
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
  }

  function updateButtons() {
    const busy = jobRunning || opRunning;
    document.querySelectorAll('button[data-action]').forEach((b) => {
      const noVersion = $('#version').disabled || !$('#version').value;
      b.disabled = busy || (b.id === 'deploy' && noVersion);
    });
  }

  // ---- status and actions -------------------------------------------------

  function setState(state) {
    const b = $('#state');
    b.textContent = state;
    b.className = 'badge state-' + state;
  }

  function renderStatus(s) {
    setState(s.state);
    const body = $('#containers');
    body.replaceChildren();
    if (!s.containers.length) {
      const tr = el('tr');
      const td = el('td', 'muted', 'No containers (the app was never started or has been removed).');
      td.colSpan = 5;
      tr.append(td);
      body.append(tr);
    }
    for (const c of s.containers) {
      const tr = el('tr');
      tr.append(el('td', '', c.service), el('td', 'mono', c.name), el('td', '', c.state),
        el('td', '', c.health || '–'), el('td', 'mono', c.image));
      body.append(tr);
    }
    updateLogServices(s.containers);
  }

  async function refreshStatus() {
    try {
      renderStatus(await api('/status'));
    } catch (e) {
      setState('unknown');
      show('Cannot read status: ' + e.message, 'error');
    }
  }

  async function operate(op) {
    if (op !== 'start' && !window.confirm(`${op[0].toUpperCase()}${op.slice(1)} the whole app?`)) return;
    opRunning = true;
    updateButtons();
    show(`${op}…`);
    try {
      const r = await post('/' + op);
      show(`${op}: done` + (r && r.output ? '\n' + r.output : ''), 'ok');
    } catch (e) {
      // The server already says "<op> failed"; do not repeat it.
      const head = e.message.startsWith(op) ? e.message : `${op} failed: ${e.message}`;
      show(head + (e.output ? '\n' + e.output : ''), 'error');
    } finally {
      opRunning = false;
      updateButtons();
      refreshStatus();
    }
  }

  // ---- versions, deploy, rollback -----------------------------------------

  async function loadVersions() {
    const sel = $('#version');
    const note = $('#version-note');
    try {
      const v = await api('/versions?limit=' + VERSIONS_SHOWN);
      sel.replaceChildren();
      for (const tag of v.tags) {
        const o = el('option', '', tag + (tag === v.current ? '  (current)' : ''));
        o.value = tag;
        sel.append(o);
      }
      sel.disabled = !v.tags.length;
      note.hidden = false;
      // A current tag missing from the list (such as latest, or an old version
      // beyond the newest shown) is named here, with its digest.
      const current = v.current && !v.tags.includes(v.current)
        ? ` · current: ${formatVersion(v.current + (v.current_digest ? '@' + v.current_digest : ''))}` : '';
      note.textContent = `${v.image} · ${v.total} version${v.total === 1 ? '' : 's'}` +
        (v.truncated ? `, showing the newest ${v.tags.length}` : '') + current;
    } catch (e) {
      const none = el('option', '', 'unavailable');
      none.value = '';
      sel.replaceChildren(none);
      sel.disabled = true;
      note.hidden = false;
      note.textContent = 'Versions unavailable: ' + e.message;
    }
    updateButtons();
  }

  async function deploy() {
    const tag = $('#version').value;
    if (!tag) return;
    let plan;
    try {
      plan = await post('/deploy', { tag, dry_run: '1' });
    } catch (e) {
      show('Cannot deploy: ' + e.message, 'error');
      return;
    }
    if (!plan.changes) {
      show(`Already running ${formatVersion(plan.current)}.`);
      return;
    }
    const question = `Deploy ${plan.image}\n${formatVersion(plan.current)}  →  ${formatVersion(plan.target)}`;
    if (!window.confirm(question)) return;
    await startJob(() => post('/deploy', { tag }));
  }

  async function rollback() {
    if (!window.confirm('Roll back to the previous version?')) return;
    await startJob(() => post('/rollback'));
  }

  async function startJob(start) {
    try {
      trackJob(await start());
    } catch (e) {
      show('Cannot start: ' + e.message, 'error');
    }
  }

  function describeJob(j) {
    return `${j.action} ${formatVersion(j.from)} → ${formatVersion(j.to)}`;
  }

  async function trackJob(job) {
    jobRunning = true;
    updateButtons();
    const box = $('#job');
    box.hidden = false;
    show('');
    let j = job;
    try {
      while (j.state === 'running') {
        box.textContent = `${describeJob(j)}: ${j.phase}…`;
        await new Promise((r) => setTimeout(r, JOB_EVERY_MS));
        j = await api('/deploy');
      }
    } catch (e) {
      show('Lost track of the deployment: ' + e.message, 'error');
      jobRunning = false;
      updateButtons();
      return;
    }
    jobRunning = false;
    box.hidden = true;
    if (j.state === 'ok') show(`Done: ${describeJob(j)}`, 'ok');
    else if (j.state === 'rolled_back') show(`The new version failed and the previous one was restored.\n${j.error}`, 'error');
    else show(`${j.action} failed: ${j.error}`, 'error');
    updateButtons();
    refreshStatus();
    loadVersions();
    loadHistory();
  }

  async function resumeJob() {
    try {
      const j = await api('/deploy');
      if (j.state === 'running') trackJob(j);
    } catch (_) { /* no deployment since Helmo started */ }
  }

  // ---- history ------------------------------------------------------------

  async function loadHistory() {
    const body = $('#history');
    try {
      const entries = await api('/history?limit=20');
      body.replaceChildren();
      if (!entries.length) {
        const tr = el('tr');
        const td = el('td', 'muted', 'No version changes recorded yet.');
        td.colSpan = 6;
        tr.append(td);
        body.append(tr);
      }
      for (const e of entries) {
        const tr = el('tr');
        tr.append(el('td', '', formatTime(e.time)), el('td', '', e.action),
          el('td', 'mono', formatVersion(e.from)), el('td', 'mono', formatVersion(e.to)),
          el('td', 'res-' + e.result, e.result), el('td', '', e.by || ''));
        if (e.error) tr.title = e.error;
        body.append(tr);
      }
    } catch (e) {
      body.replaceChildren();
      const tr = el('tr');
      const td = el('td', 'muted', 'History unavailable: ' + e.message);
      td.colSpan = 6;
      tr.append(td);
      body.append(tr);
    }
  }

  // ---- logs ---------------------------------------------------------------

  let source = null;
  let logsPaused = false;
  let logServices = '';
  let gotLine = false;    // the current stream delivered a line
  let lastMs = 0;         // time of the newest line shown, to continue after it
  let lastText = '';
  let skipUntilMs = 0;    // after a reconnect: skip lines up to the last one shown
  let skipText = '';
  let retryTimer = null;
  let retryDelay = LOG_RETRY_MIN_MS;

  function logNote(text) {
    const n = $('#log-note');
    n.textContent = text || '';
    n.hidden = !text;
  }

  function addLogLine(ev) {
    const box = $('#log');
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
    const line = el('div', 'line ' + (ev.stream === 'stderr' ? 'stderr' : 'stdout'));
    if (ev.ts) {
      const ts = el('span', 'ts', formatLogTime(ev.ts));
      ts.title = new Date(ev.ts).toString();
      // A real space, so a copied line does not glue the time to the text.
      line.append(ts, document.createTextNode(' '));
    }
    line.append(document.createTextNode(ev.text));
    box.append(line);
    while (box.childElementCount > MAX_LOG_LINES) box.firstElementChild.remove();
    if (atBottom) box.scrollTop = box.scrollHeight;
  }

  // formatLogTime shows the local time as 24-hour HH:MM:SS, with the date in
  // front when the line is not from today (a tail can reach weeks back).
  function formatLogTime(iso) {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    const p = (n) => String(n).padStart(2, '0');
    const time = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
    const now = new Date();
    if (d.toDateString() === now.toDateString()) return time;
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${time}`;
  }

  function addMarker(text) {
    const box = $('#log');
    if (box.childElementCount && !box.lastElementChild.classList.contains('marker')) {
      box.append(el('div', 'line marker', text));
    }
  }

  // A reconnect asks for lines since the second of the last one shown; skip
  // those already on the page. Only at the start of that stream: within a
  // stream lines are never dropped, even when their times are out of order.
  function seen(ev) {
    if (!skipUntilMs) return false;
    const ms = ev.ts ? Date.parse(ev.ts) : NaN;
    if (ms < skipUntilMs) return true;
    if (ms === skipUntilMs && ev.text === skipText) {
      skipUntilMs = 0;
      return true;
    }
    skipUntilMs = 0; // the first new line: show everything from here on
    return false;
  }

  function forget() {
    lastMs = 0;
    lastText = '';
    skipUntilMs = 0;
  }

  function cancelReconnect() {
    clearTimeout(retryTimer);
    retryTimer = null;
  }

  function stopLogs() {
    if (source) {
      source.close();
      source = null;
    }
  }

  function startLogs(tail, sinceSeconds) {
    stopLogs();
    cancelReconnect();
    const service = $('#log-service').value;
    if (!service) {
      logNote('The app has no containers, so there are no logs.');
      return;
    }
    let url = `${API}/logs?tail=${tail}&service=${encodeURIComponent(service)}`;
    skipUntilMs = 0;
    if (sinceSeconds) {
      url += `&since=${sinceSeconds}`;
      skipUntilMs = lastMs;
      skipText = lastText;
    }
    gotLine = false;
    source = new EventSource(url);
    source.onopen = () => {
      if (!gotLine) logNote('Connected; no output yet. New lines appear here as the app writes them.');
    };
    source.onmessage = (m) => {
      let ev;
      try { ev = JSON.parse(m.data); } catch (_) { return; /* ignore a malformed event */ }
      if (seen(ev)) return;
      if (!gotLine) {
        gotLine = true;
        retryDelay = LOG_RETRY_MIN_MS;
        logNote('');
      }
      addLogLine(ev);
      const ms = ev.ts ? Date.parse(ev.ts) : NaN;
      if (ms >= lastMs) {
        lastMs = ms;
        lastText = ev.text;
      }
    };
    source.addEventListener('end', (m) => {
      stopLogs();
      let failure = '';
      try { failure = JSON.parse(m.data).text || ''; } catch (_) { /* no detail */ }
      if (failure) {
        logNote(`Log stream ended: ${failure}. Press Resume to try again.`);
        setPaused(true);
        return;
      }
      // Docker ends a followed stream when the container stops, also for a
      // restart; follow the container again once it is back.
      addMarker('— container stopped or restarted —');
      logNote('The container stopped; reconnecting when it runs again…');
      scheduleReconnect();
    });
    source.onerror = () => {
      stopLogs();
      logNote('Log stream disconnected. Press Resume to reconnect.');
      setPaused(true);
    };
  }

  // resumeLogs continues after the last line shown, or starts with a tail.
  function resumeLogs() {
    if (lastMs) startLogs(1000, Math.floor(lastMs / 1000));
    else startLogs(200);
  }

  function scheduleReconnect() {
    cancelReconnect();
    retryTimer = setTimeout(() => {
      retryTimer = null;
      if (!logsPaused) resumeLogs();
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, LOG_RETRY_MAX_MS);
  }

  function setPaused(paused) {
    logsPaused = paused;
    $('#log-toggle').textContent = paused ? 'Resume' : 'Pause';
  }

  function restartForService() {
    $('#log').replaceChildren();
    forget();
    retryDelay = LOG_RETRY_MIN_MS;
    if (!logsPaused) startLogs(200);
  }

  function updateLogServices(containers) {
    const services = [...new Set(containers.map((c) => c.service).filter(Boolean))];
    const key = services.join('\n');
    if (key === logServices) return;
    const sel = $('#log-service');
    const previous = sel.value;
    logServices = key;
    sel.replaceChildren();
    for (const s of services) {
      const o = el('option', '', s);
      o.value = s;
      sel.append(o);
    }
    if (services.includes(previous)) sel.value = previous;
    if (sel.value === previous && lastMs) {
      if (!logsPaused && !source) resumeLogs();
    } else {
      restartForService();
    }
  }

  // ---- wiring -------------------------------------------------------------

  document.querySelectorAll('button[data-op]').forEach((b) => {
    b.addEventListener('click', () => operate(b.dataset.op));
  });
  $('#deploy').addEventListener('click', deploy);
  $('#rollback').addEventListener('click', rollback);
  $('#version').addEventListener('change', updateButtons);
  $('#log-toggle').addEventListener('click', () => {
    if (logsPaused) {
      setPaused(false);
      retryDelay = LOG_RETRY_MIN_MS;
      resumeLogs();
    } else {
      setPaused(true);
      stopLogs();
      cancelReconnect();
      logNote('Paused.');
    }
  });
  $('#log-clear').addEventListener('click', () => $('#log').replaceChildren());
  $('#log-service').addEventListener('change', restartForService);

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refreshStatus();
  });
  setInterval(() => { if (!document.hidden) refreshStatus(); }, STATUS_EVERY_MS);

  updateButtons();
  refreshStatus();
  loadVersions();
  loadHistory();
  resumeJob();
})();
