/* StoryCloud — shared client helpers */

// Auth token storage (Firebase ID token)
let _idToken = null;

function setAuthToken(token) { _idToken = token; }
function getAuthToken() { return _idToken; }

// Build fetch headers with optional auth
function authHeaders(extra) {
  const h = Object.assign({ 'Content-Type': 'application/json' }, extra || {});
  if (_idToken) h['Authorization'] = 'Bearer ' + _idToken;
  return h;
}

// Convenience fetch wrappers
async function apiGet(url) {
  const res = await fetch(url, { headers: authHeaders() });
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw Object.assign(new Error(body.error || res.statusText), { status: res.status, code: body.code });
  }
  return res.json();
}

async function apiPost(url, data) {
  const res = await fetch(url, {
    method: 'POST',
    headers: authHeaders(),
    body: JSON.stringify(data),
  });
  const body = await res.json().catch(() => ({ error: res.statusText }));
  if (!res.ok) throw Object.assign(new Error(body.error || res.statusText), { status: res.status, code: body.code });
  return body;
}

async function apiPut(url, data) {
  const res = await fetch(url, {
    method: 'PUT',
    headers: authHeaders(),
    body: JSON.stringify(data),
  });
  const body = await res.json().catch(() => ({ error: res.statusText }));
  if (!res.ok) throw Object.assign(new Error(body.error || res.statusText), { status: res.status, code: body.code });
  return body;
}

// Render star rating (0–5)
function renderStars(rating) {
  if (!rating) return '';
  const full = Math.round(rating);
  return '★'.repeat(full) + '☆'.repeat(Math.max(0, 5 - full));
}

// Format ISO date string as relative or absolute
function formatDate(iso) {
  if (!iso) return '';
  return new Date(iso).toLocaleString();
}

// Show an error message in an element
function showError(el, msg) {
  el.innerHTML = '<div class="error-msg">' + escHtml(msg) + '</div>';
}

// Basic HTML escaping
function escHtml(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/**
 * Authenticated fetch wrapper.
 *
 * - Awaits window.scAuth.ready (resolves once Firebase confirms a signed-in user).
 * - Injects Authorization: Bearer <token> on every request.
 * - Defaults Content-Type to application/json for POST/PUT/PATCH.
 * - On HTTP 401: redirects to /login preserving the current path in ?next=.
 *   The returned Promise never resolves in this case — the page is navigating away.
 *
 * @param {string}      url
 * @param {RequestInit} [options]
 * @returns {Promise<Response>}
 */
async function apiFetch(url, options) {
  await window.scAuth.ready;
  const token = await window.scAuth.getToken();

  const method  = (options && options.method) || 'GET';
  const headers = Object.assign({}, (options && options.headers) || {});

  if (token) headers['Authorization'] = 'Bearer ' + token;
  if ((method === 'POST' || method === 'PUT' || method === 'PATCH') &&
      !headers['Content-Type']) {
    headers['Content-Type'] = 'application/json';
  }

  const resp = await fetch(url, Object.assign({}, options, { headers: headers }));

  if (resp.status === 401) {
    window.location.assign(
      '/login?next=' + encodeURIComponent(location.pathname + location.search),
    );
    // Return a promise that never resolves — the page is redirecting.
    return new Promise(function () {});
  }

  return resp;
}

// ── Hamburger nav (mobile ≤768px) ──
(function () {
  const btn = document.getElementById('navHamburger');
  const menu = document.getElementById('navMobileDropdown');
  if (!btn || !menu) return;

  btn.addEventListener('click', function (e) {
    e.stopPropagation();
    const open = menu.classList.toggle('open');
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
  });

  document.addEventListener('click', function (e) {
    if (!menu.contains(e.target) && !btn.contains(e.target)) {
      menu.classList.remove('open');
      btn.setAttribute('aria-expanded', 'false');
    }
  });
}());
