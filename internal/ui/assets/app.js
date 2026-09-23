// The web interface's behaviour (whitepaper §12.1.1).
//
// Two rules shape all of it, and both are about *not* doing things.
//
// 1. This is a REST client. It calls the same endpoints odctl calls, and it has no
//    other way to reach OpenDrive. There is no SDK here to bypass the daemon with,
//    and there must never be one: a second path would mean a second place
//    credentials are handled and a second set of error wordings, undoing the one
//    property six phases of work went into establishing.
//
// 2. Errors are displayed exactly as the daemon words them. §4.5's `message` field
//    is already written for a person — "OpenDrive turned this request away for a
//    moment even though your account has the right to do it" rather than a status
//    code — and the classification layer behind it is the only thing entitled to say
//    what an upstream failure meant. Rewriting it here would be a third translation
//    of something already translated once, and the one furthest from the code that
//    knows what happened.
//
// No framework, no build step, no dependency. The file is meant to be read.

'use strict';

// ---------------------------------------------------------------- the key
//
// sessionStorage, not localStorage, and never a URL.
//
// §12.1.1 asks for sessionStorage specifically: it is gone when the tab closes,
// which for a key that opens somebody's cloud storage is the right lifetime. A URL
// is out of the question — it reaches the browser history, the Referer header and
// any log in between, which is the whole of what §9.4 exists to prevent.

const KEY_NAME = 'odb-api-key';

function apiKey() {
  try {
    return sessionStorage.getItem(KEY_NAME) || '';
  } catch (e) {
    // A browser with storage disabled still works; the key just has to be typed
    // again after a reload.
    return '';
  }
}

function rememberKey(value) {
  try {
    sessionStorage.setItem(KEY_NAME, value);
  } catch (e) {
    /* nothing to do; see above */
  }
}

// ---------------------------------------------------------------- talking to it

// call makes one request and returns the parsed body.
//
// On failure it throws an Error whose message is the daemon's own, so that every
// call site can display it without deciding anything.
async function call(method, path, body) {
  const headers = {};
  const key = apiKey();
  if (key) {
    // A header, never a query parameter.
    headers['Authorization'] = 'Bearer ' + key;
  }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }

  let res;
  try {
    res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      // No cookies: the interface has no session of its own and the API does not
      // use them, so sending them would only be a way to be surprised later.
      credentials: 'omit',
      cache: 'no-store',
    });
  } catch (e) {
    // The daemon is not answering at all. That is the one message this file writes
    // itself, because there is no daemon to ask for one.
    throw new Error('The bridge is not answering. Is it still running?');
  }

  const text = await res.text();
  let parsed = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch (e) {
      parsed = null;
    }
  }

  if (!res.ok) {
    if (parsed && parsed.error && parsed.error.message) {
      const err = new Error(parsed.error.message);
      err.code = parsed.error.code;
      err.status = res.status;
      throw err;
    }
    const err = new Error('The bridge answered ' + res.status + ' without explaining why.');
    err.status = res.status;
    throw err;
  }
  return parsed;
}

// ---------------------------------------------------------------- display

const el = (id) => document.getElementById(id);

function showError(err) {
  const box = el('errors');
  const line = document.createElement('div');
  line.className = 'error';
  // textContent, not innerHTML: the message comes from the daemon and may quote a
  // filename somebody else chose.
  line.textContent = err.message;
  const dismiss = document.createElement('button');
  dismiss.type = 'button';
  dismiss.className = 'dismiss';
  dismiss.textContent = '×';
  dismiss.addEventListener('click', () => line.remove());
  line.appendChild(dismiss);
  box.prepend(line);
  while (box.children.length > 4) {
    box.lastElementChild.remove();
  }
}

function clearErrors() {
  el('errors').textContent = '';
}

// bytes formats a byte count the way a person reads one. Deliberately the same
// thresholds odctl uses, so the two do not disagree about what a megabyte is.
function bytes(n) {
  if (n === null || n === undefined) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = Number(n);
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  return (i === 0 ? v.toFixed(0) : v.toFixed(1)) + ' ' + units[i];
}

function seconds(s) {
  if (!s || s < 1) return '—';
  if (s < 60) return Math.round(s) + 's';
  if (s < 3600) return Math.round(s / 60) + ' min';
  return (s / 3600).toFixed(1) + ' h';
}

function setConn(state, text) {
  const pill = el('conn');
  pill.className = 'pill pill-' + state;
  pill.textContent = text;
}

// ---------------------------------------------------------------- 1. status

async function refreshStatus() {
  const s = await call('GET', '/v1/auth/status');

  const account = s.account && s.account.username ? s.account.username : 'not signed in';
  el('s-account').textContent = account;
  el('s-state').textContent = s.state || '—';
  el('s-seamless').textContent = s.seamless ? 'yes' : 'no';

  if (s.keystore) {
    el('s-keystore').textContent = s.keystore.backend +
      (s.keystore.available ? '' : ' — not readable');
  }

  if (s.quota && s.quota.storage_max) {
    const pct = Math.round((s.quota.storage_used / s.quota.storage_max) * 100);
    el('s-quota').textContent = bytes(s.quota.storage_used) + ' of ' +
      bytes(s.quota.storage_max) + ' (' + pct + '%)';
  } else {
    el('s-quota').textContent = '—';
  }

  // The auth panel explains what to do, using the daemon's state rather than
  // guessing. reauth_required and captcha_required are the two the user has to act
  // on, and §4.5 is explicit that the bridge stops trying by itself in both.
  const explain = el('auth-explain');
  switch (s.state) {
    case 'authenticated':
      explain.textContent = 'Signed in, and the bridge keeps itself that way. ' +
        'You should not have to do anything here again.';
      break;
    case 'reauth_required':
      explain.textContent = 'OpenDrive is no longer accepting the saved sign-in — ' +
        'usually because the password changed. Sign in again below.';
      break;
    case 'captcha_required':
      explain.textContent = 'OpenDrive is asking for a captcha, which cannot be ' +
        'answered from here. Sign in on the OpenDrive website once, then come back.';
      break;
    case 'not_configured':
      explain.textContent = 'No account yet. Sign in once and the bridge will stay ' +
        'signed in afterwards.';
      break;
    default:
      explain.textContent = '';
  }
  return s;
}

// ---------------------------------------------------------------- 3. rate curve

// A canvas drawn by hand rather than a charting library, so that nothing is fetched
// from anywhere and there is no vendored blob to review. It is a line; a library
// would be a lot of bytes to draw a line.
const RATE_SAMPLES = 90;
const rate = [];

function pushRate(bytesPerSecond) {
  rate.push(bytesPerSecond);
  while (rate.length > RATE_SAMPLES) rate.shift();
  drawRate();
}

function drawRate() {
  const canvas = el('ratechart');
  const ctx = canvas.getContext('2d');
  const w = canvas.width;
  const h = canvas.height;
  ctx.clearRect(0, 0, w, h);

  const peak = Math.max(1, ...rate);
  const style = getComputedStyle(document.body);
  const ink = style.getPropertyValue('--ink').trim() || '#222';
  const faint = style.getPropertyValue('--faint').trim() || '#ccc';

  // A baseline, and a line at the peak so the scale is readable without axes.
  ctx.strokeStyle = faint;
  ctx.lineWidth = 1;
  ctx.beginPath();
  ctx.moveTo(0, h - 0.5);
  ctx.lineTo(w, h - 0.5);
  ctx.stroke();

  // Nothing has moved, so there is nothing to draw and nothing to claim. An
  // earlier version drew a flat line and labelled it "peak 1.0 B/s", because the
  // peak was floored at 1 to avoid dividing by zero — a number on the screen that
  // described a transfer which never happened. Small, and exactly the kind of thing
  // this project spends its time catching upstream.
  const moved = rate.some((v) => v > 0);
  if (rate.length < 2 || !moved) {
    ctx.fillStyle = faint;
    ctx.font = '12px system-ui, sans-serif';
    ctx.fillText('no transfers yet', 8, 18);
    return;
  }

  const step = w / (RATE_SAMPLES - 1);
  ctx.strokeStyle = ink;
  ctx.lineWidth = 1.5;
  ctx.beginPath();
  rate.forEach((v, i) => {
    const x = i * step;
    const y = h - (v / peak) * (h - 12) - 1;
    if (i === 0) ctx.moveTo(x, y);
    else ctx.lineTo(x, y);
  });
  ctx.stroke();

  ctx.fillStyle = faint;
  ctx.font = '12px system-ui, sans-serif';
  ctx.fillText('peak ' + bytes(peak) + '/s', 8, 14);
}

// ---------------------------------------------------------------- 4. jobs

async function refreshJobs() {
  const out = await call('GET', '/v1/jobs');
  const jobs = (out && out.jobs) || [];
  const body = el('jobs-body');
  body.textContent = '';

  let running = 0;
  if (jobs.length === 0) {
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    td.colSpan = 6;
    td.className = 'quiet';
    td.textContent = 'No transfers.';
    tr.appendChild(td);
    body.appendChild(tr);
  }

  jobs.sort((a, b) => String(b.updated_at).localeCompare(String(a.updated_at)));
  for (const j of jobs) {
    if (j.state === 'running') running += Number(j.speed) || 0;

    const tr = document.createElement('tr');
    tr.appendChild(cell(j.remote_path || j.local_path || j.id));
    tr.appendChild(cell(j.kind));
    tr.appendChild(cell(j.state, 'state-' + j.state));

    // The leg, in words. "caching" is the one worth spelling out: it is why a large
    // upload can finish its first leg in a moment and then sit in the second.
    let leg = j.phase || '';
    if (leg === 'caching') leg = 'copying to the bridge';
    else if (leg === 'uploading') leg = 'sending to OpenDrive';
    else if (leg === 'downloading') leg = 'fetching';
    tr.appendChild(cell(leg));

    const total = Number(j.bytes_total) || 0;
    const done = Number(j.bytes_done) || 0;
    const pct = total > 0 ? Math.round((done / total) * 100) : 0;
    tr.appendChild(cell(total > 0 ? pct + '% (' + bytes(done) + ' of ' + bytes(total) + ')' : '—'));

    const actions = document.createElement('td');
    if (j.state === 'running' || j.state === 'queued') {
      const stop = document.createElement('button');
      stop.type = 'button';
      stop.className = 'secondary';
      stop.textContent = 'Stop';
      stop.addEventListener('click', () => act(() => call('DELETE', '/v1/jobs/' + j.id)));
      actions.appendChild(stop);
    }
    tr.appendChild(actions);

    // A failed job shows the daemon's message, unaltered.
    body.appendChild(tr);
    if (j.error && j.error.message) {
      const note = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 6;
      td.className = 'joberror';
      td.textContent = j.error.message;
      note.appendChild(td);
      body.appendChild(note);
    }
  }
  pushRate(running);
}

function cell(text, className) {
  const td = document.createElement('td');
  td.textContent = text === undefined || text === null || text === '' ? '—' : String(text);
  if (className) td.className = className;
  return td;
}

// ---------------------------------------------------------------- 5. cache

async function refreshCache() {
  const c = await call('GET', '/v1/cache/status');
  const panel = el('panel-cache');

  if (!c || c.enabled === false) {
    panel.classList.add('off');
    setVerdict('unknown', 'The cache is switched off on this bridge.');
    el('cache-actions').hidden = true;
    return;
  }
  panel.classList.remove('off');
  el('cache-actions').hidden = false;

  el('c-holding').textContent = bytes(c.bytes) + ' of ' + bytes(c.max_bytes) +
    ' in ' + (c.objects || 0) + ' file(s)';
  el('c-dir').textContent = c.dir || '—';

  if (c.write_back === false) {
    el('c-dirty').textContent = 'nothing — writes go straight to OpenDrive';
    el('c-oldest').textContent = '—';
    setVerdict('safe', 'Nothing is waiting to be uploaded. It is safe to stop the bridge.');
  } else {
    el('c-dirty').textContent = bytes(c.dirty_bytes) + ' in ' +
      (c.dirty_objects || 0) + ' file(s), limit ' + bytes(c.max_dirty_bytes);
    el('c-oldest').textContent = seconds(c.oldest_dirty_age_seconds);

    // §3.5.2 rule 3, on the screen. The daemon decides; this displays.
    if (c.safe_to_shut_down) {
      setVerdict('safe', 'Everything has reached OpenDrive. It is safe to stop the bridge.');
    } else {
      setVerdict('unsafe', 'NOT safe to stop the bridge yet — ' + bytes(c.dirty_bytes) +
        ' has not reached OpenDrive.');
    }
  }

  const hits = Number(c.hits) || 0;
  const misses = Number(c.misses) || 0;
  el('c-hits').textContent = hits + misses > 0
    ? hits + ' of ' + (hits + misses) + ' (' + Math.round((c.hit_rate || 0) * 100) + '%)'
    : '—';

  // §8.3.1: a container whose cache is not on a persistent volume. Shown in full,
  // because the consequence is losing data on an operation people do every day.
  const warn = el('cache-warning');
  if (c.durable === false && c.durability_note) {
    warn.textContent = c.durability_note;
    warn.hidden = false;
  } else {
    warn.hidden = true;
    warn.textContent = '';
  }

  await refreshCacheObjects();
}

function setVerdict(kind, text) {
  const v = el('cache-verdict');
  v.className = 'verdict verdict-' + kind;
  v.textContent = text;
}

async function refreshCacheObjects() {
  const out = await call('GET', '/v1/cache/objects');
  const objects = (out && out.objects) || [];
  const body = el('cache-objects-body');
  body.textContent = '';

  if (objects.length === 0) {
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    td.colSpan = 3;
    td.className = 'quiet';
    td.textContent = 'The cache is empty.';
    tr.appendChild(td);
    body.appendChild(tr);
    return;
  }

  for (const o of objects.slice(0, 50)) {
    const unsent = o.state === 'dirty' || o.state === 'uploading';
    const tr = document.createElement('tr');
    tr.appendChild(cell(o.remote_path));
    tr.appendChild(cell(bytes(o.size)));
    // Words, not the API's state names: somebody looking at a list of their own
    // files should be told what it means for them.
    tr.appendChild(cell(unsent ? 'NOT uploaded yet' : 'on OpenDrive',
      unsent ? 'unsent' : 'sent'));
    body.appendChild(tr);
    if (o.last_error) {
      const note = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 3;
      td.className = 'joberror';
      td.textContent = o.last_error;
      note.appendChild(td);
      body.appendChild(note);
    }
  }
}

// ---------------------------------------------------------------- actions

// act runs something that changes state, shows whatever the daemon says about it,
// and refreshes. Every button goes through here so that none of them can forget to
// report a failure.
async function act(fn) {
  try {
    await fn();
    clearErrors();
  } catch (e) {
    showError(e);
  }
  await refreshAll();
}

function wire() {
  el('keyform').addEventListener('submit', (ev) => {
    ev.preventDefault();
    const value = el('keyinput').value.trim();
    if (!value) return;
    rememberKey(value);
    el('keyinput').value = '';
    act(async () => {});
  });

  el('loginform').addEventListener('submit', (ev) => {
    ev.preventDefault();
    const username = el('username').value.trim();
    const password = el('password').value;
    if (!username || !password) return;
    // Cleared immediately: there is no reason for it to sit in a form field.
    el('password').value = '';
    act(() => call('POST', '/v1/auth/login', { username, password }));
  });

  el('logout').addEventListener('click', () => {
    act(() => call('POST', '/v1/auth/logout'));
  });

  el('flush').addEventListener('click', () => {
    // wait:false, so the page is not held open by a long upload. The verdict above
    // is what says when it is done.
    act(() => call('POST', '/v1/cache/flush', { wait: false }));
  });

  el('clear').addEventListener('click', () => {
    // No confirmation dialog, because the daemon refuses to discard anything unsent
    // and says so — a 409 with its reason is a better guard than a prompt somebody
    // clicks through.
    act(() => call('DELETE', '/v1/cache'));
  });
}

// ---------------------------------------------------------------- the loop

let failures = 0;

async function refreshAll() {
  try {
    await refreshStatus();
    await refreshJobs();
    await refreshCache();
    setConn('ok', 'connected');
    el('keybox').hidden = true;
    failures = 0;
  } catch (e) {
    // A 401 is the one failure with an obvious next step, and it is the only reason
    // the key box appears.
    if (e.status === 401) {
      el('keybox').hidden = false;
      setConn('auth', 'needs an API key');
      return;
    }
    failures++;
    setConn('down', 'not answering');
    // Reported once rather than on every poll, so a bridge that is down does not
    // fill the page with the same line.
    if (failures === 1) showError(e);
  }
}

function start() {
  wire();
  refreshAll();
  setInterval(refreshAll, 2000);
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', start);
} else {
  start();
}
