/**
 * AIFStudio — session-cookie auth bootstrap (loaded as <script type="module"> on every page).
 *
 * Responsibilities:
 *  - Skip on /login and /register (those pages handle auth themselves).
 *  - Call GET /api/auth/me to check session validity.
 *  - On 200: populate window.scAuth, resolve scAuth.ready, populate nav.
 *  - On 401: redirect to /login?next=<current path>.
 *  - Cookies are sent automatically — no token management needed.
 */

// Auth pages handle their own flow — nothing to do here.
if (location.pathname !== '/login' && location.pathname !== '/register') {
  initAuth();
}

async function initAuth() {
  let resp, user;
  try {
    resp = await fetch('/api/auth/me', { credentials: 'same-origin' });
  } catch (_) {
    location.assign('/login?next=' + encodeURIComponent(location.pathname + location.search));
    return;
  }

  if (resp.status === 401) {
    location.assign('/login?next=' + encodeURIComponent(location.pathname + location.search));
    return;
  }

  if (!resp.ok) {
    location.assign('/login?next=' + encodeURIComponent(location.pathname + location.search));
    return;
  }

  user = await resp.json();
  window.scAuth.currentUser = user;

  // Cookies are sent automatically — no Bearer token needed.
  window.scAuth.getToken = async function () { return null; };

  window.scAuth.signOut = async function () {
    await fetch('/api/auth/logout', { method: 'POST', credentials: 'same-origin' });
    location.assign('/login');
  };

  window.scAuth._resolveReady();
  populateNavUser(user);
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
