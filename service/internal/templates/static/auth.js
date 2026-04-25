/**
 * StoryCloud — Firebase auth bootstrap (loaded as <script type="module"> on every page).
 *
 * Responsibilities:
 *  - Skip on /login and /register (those pages initialise Firebase themselves).
 *  - Fetch /api/config and initialise the Firebase JS SDK.
 *  - On auth state confirmed (user present): populate window.scAuth and resolve
 *    window.scAuth.ready so that apiFetch() calls unblock.
 *  - On auth state absent: redirect to /login (ready never resolves — page navigates).
 *  - Local-dev mode (/api/config returns 503 auth_disabled): inject stub user so
 *    the app functions without Firebase credentials.
 *  - Populate the nav user area and wire the Sign-out button.
 */

import { initializeApp } from 'https://www.gstatic.com/firebasejs/10.12.0/firebase-app.js';
import {
  getAuth,
  onAuthStateChanged,
  signOut as fbSignOut,
} from 'https://www.gstatic.com/firebasejs/10.12.0/firebase-auth.js';

// Auth pages initialise Firebase themselves — nothing to do here.
if (location.pathname !== '/login' && location.pathname !== '/register') {
  initAuth();
}

async function initAuth() {
  let resp, cfg;
  try {
    resp = await fetch('/api/config');
    cfg  = await resp.json();
  } catch (_) {
    // Network failure on config fetch — fall back to local-dev stub so the UI
    // does not completely break in offline development.
    applyLocalDev();
    return;
  }

  if (!resp.ok || cfg.error === 'auth_disabled') {
    applyLocalDev();
    return;
  }

  const app  = initializeApp(cfg.firebase);
  const auth = getAuth(app);

  // Install real implementations before onAuthStateChanged fires.
  window.scAuth.getToken = async function () {
    const user = auth.currentUser;
    if (!user) {
      location.assign('/login?next=' + encodeURIComponent(location.pathname + location.search));
      return null;
    }
    // Never cache the result — SDK transparently refreshes when < 5 min remain.
    return user.getIdToken(/* forceRefresh */ false);
  };

  window.scAuth.signOut = async function () {
    await fbSignOut(auth);
    location.assign('/login');
  };

  onAuthStateChanged(auth, function (user) {
    window.scAuth.currentUser = user;
    if (!user) {
      // Not authenticated — redirect and leave scAuth.ready pending so any
      // in-flight apiFetch calls never fire their .then() callbacks.
      location.assign(
        '/login?next=' + encodeURIComponent(location.pathname + location.search),
      );
      return;
    }
    populateNavUser(user);
    // Unblock all apiFetch calls that are awaiting scAuth.ready.
    window.scAuth._resolveReady();
  });
}

function applyLocalDev() {
  window.scAuth.getToken    = async function () { return 'local-dev-stub'; };
  window.scAuth.signOut     = function () { location.assign('/login'); };
  window.scAuth.currentUser = { uid: 'local-dev', email: 'dev@local' };
  window.scAuth._resolveReady();
  populateNavUser({ email: 'dev@local' });
}

function populateNavUser(user) {
  const area = document.getElementById('nav-user-area');
  if (!area) return;
  const email = escHtml(user.email || '');
  area.innerHTML =
    '<span class="nav-user">' + email + '</span>' +
    '<button class="nav-signout" id="scSignOutBtn">Sign out</button>' +
    '<button class="nav-user-icon-btn" id="navUserIconBtn" aria-label="User menu" aria-expanded="false">👤</button>' +
    '<div class="nav-user-dropdown" id="navUserDropdown">' +
      '<span class="nav-user-dropdown-email">' + email + '</span>' +
      '<button class="nav-user-dropdown-signout" id="scSignOutBtnMobile">Sign out</button>' +
    '</div>';

  document.getElementById('scSignOutBtn').addEventListener('click', function () { window.scAuth.signOut(); });
  document.getElementById('scSignOutBtnMobile').addEventListener('click', function () { window.scAuth.signOut(); });

  const iconBtn  = document.getElementById('navUserIconBtn');
  const dropdown = document.getElementById('navUserDropdown');

  iconBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    const isOpen = dropdown.classList.toggle('open');
    iconBtn.setAttribute('aria-expanded', String(isOpen));
    // Close hamburger menu if open
    const navMenu = document.getElementById('navMobileDropdown');
    if (navMenu) {
      navMenu.classList.remove('open');
      const hamBtn = document.getElementById('navHamburger');
      if (hamBtn) hamBtn.setAttribute('aria-expanded', 'false');
    }
  });

  document.addEventListener('click', function (e) {
    if (!dropdown.contains(e.target) && !iconBtn.contains(e.target)) {
      dropdown.classList.remove('open');
      iconBtn.setAttribute('aria-expanded', 'false');
    }
  });
}

function escHtml(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
