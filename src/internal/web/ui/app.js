'use strict';

(() => {
  const API = '/_helmo/api';
  const STATUS_EVERY_MS = 5000;
  const JOB_EVERY_MS = 1500;
  const MAX_LOG_LINES = 1500;

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
      show(`${op} failed: ${e.message}` + (e.output ? '\n' + e.output : ''), 'error');
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
      const v = await api('/versions?limit=30');
      sel.replaceChildren();
      for (const tag of v.tags) {
        const o = el('option', '', tag + (tag === v.current ? '  (current)' : ''));
        o.value = tag;
        sel.append(o);
      }
      sel.disabled = !v.tags.length;
      note.hidden = false;
      note.textContent = `${v.image} · ${v.total} version${v.total === 1 ? '' : 's'}` +
        (v.truncated ? `, showing the newest ${v.tags.length}` : '');
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

  function logNote(text) {
    const n = $('#log-note');
    n.textContent = text || '';
    n.hidden = !text;
  }

  function addLogLine(ev) {
    const box = $('#log');
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
    const line = el('div', 'line ' + (ev.stream === 'stderr' ? 'stderr' : 'stdout'));
    if (ev.ts) line.append(el('span', 'ts', new Date(ev.ts).toLocaleTimeString()));
    line.append(document.createTextNode(ev.text));
    box.append(line);
    while (box.childElementCount > MAX_LOG_LINES) box.firstElementChild.remove();
    if (atBottom) box.scrollTop = box.scrollHeight;
  }

  function stopLogs() {
    if (source) {
      source.close();
      source = null;
    }
  }

  function startLogs(tail) {
    stopLogs();
    const service = $('#log-service').value;
    if (!service) {
      logNote('The app has no containers, so there are no logs.');
      return;
    }
    logNote('');
    const url = `${API}/logs?tail=${tail}&service=${encodeURIComponent(service)}`;
    source = new EventSource(url);
    source.onmessage = (m) => {
      try { addLogLine(JSON.parse(m.data)); } catch (_) { /* ignore a malformed event */ }
    };
    source.addEventListener('end', (m) => {
      stopLogs();
      let msg = 'Log stream ended.';
      try { const d = JSON.parse(m.data); if (d.text) msg += ' ' + d.text; } catch (_) { /* no detail */ }
      logNote(msg);
    });
    source.onerror = () => {
      stopLogs();
      logNote('Log stream disconnected. Press Resume to reconnect.');
      setPaused(true);
    };
  }

  function setPaused(paused) {
    logsPaused = paused;
    $('#log-toggle').textContent = paused ? 'Resume' : 'Pause';
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
    if (!logsPaused) startLogs(200);
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
      startLogs(50);
    } else {
      setPaused(true);
      stopLogs();
      logNote('Paused.');
    }
  });
  $('#log-clear').addEventListener('click', () => $('#log').replaceChildren());
  $('#log-service').addEventListener('change', () => {
    $('#log').replaceChildren();
    if (!logsPaused) startLogs(200);
  });

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
