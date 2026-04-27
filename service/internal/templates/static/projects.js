// My Projects — list projects owned by the current user
// Globals from app.js: apiFetch, escHtml, formatDate
(function () {
  'use strict';

  var loadingEl = document.getElementById('projLoading');
  var errorEl   = document.getElementById('projError');
  var emptyEl   = document.getElementById('projEmpty');
  var gridEl    = document.getElementById('projGrid');

  function init() {
    apiFetch('/api/projects')
      .then(function (r) {
        return r.json().then(function (d) { return { ok: r.ok, status: r.status, data: d }; });
      })
      .then(function (res) {
        loadingEl.style.display = 'none';

        if (!res.ok) {
          var msg = (res.data && res.data.error) || 'Failed to load projects';
          if (res.status === 401) msg = 'You must be signed in to view your projects.';
          errorEl.innerHTML = '<div class="error-msg">' + escHtml(msg) + '</div>';
          errorEl.style.display = '';
          return;
        }

        // API returns a bare array (not wrapped in an object).
        var projects = Array.isArray(res.data) ? res.data : [];
        if (projects.length === 0) {
          emptyEl.style.display = '';
          return;
        }

        gridEl.style.display = '';
        projects.forEach(function (p) {
          gridEl.appendChild(buildCard(p));
        });
      })
      .catch(function (err) {
        loadingEl.style.display = 'none';
        errorEl.innerHTML = '<div class="error-msg">Failed to load: ' + escHtml(err.message) + '</div>';
        errorEl.style.display = '';
      });
  }

  function buildCard(p) {
    var card = document.createElement('div');
    card.className = 'proj-card';

    var desc = p.description || 'No description provided.';
    var descTrunc = desc.length > 200 ? desc.slice(0, 197) + '…' : desc;

    var updatedLabel = '';
    if (p.updatedAt) {
      updatedLabel = 'Updated ' + relativeDate(p.updatedAt);
    }

    var publishedBadge = p.published
      ? '<span class="badge-published">Published</span>'
      : '';

    card.innerHTML =
      '<div class="proj-card-title-row">' +
        '<div class="proj-card-title">' + escHtml(p.name) + '</div>' +
        publishedBadge +
      '</div>' +
      '<div class="proj-card-desc">' + escHtml(descTrunc) + '</div>' +
      (updatedLabel ? '<div class="proj-card-meta">' + escHtml(updatedLabel) + '</div>' : '') +
      '<div class="proj-card-actions">' +
        '<a class="btn-ai" href="/projects/' + encodeURIComponent(p.id) + '/ai">Open AI Workspace</a>' +
        '<a class="btn-view" href="/projects/' + encodeURIComponent(p.id) + '">View</a>' +
        '<button class="btn-delete-proj" data-id="' + escHtml(p.id) + '" data-name="' + escHtml(p.name) + '" title="Delete project">🗑</button>' +
      '</div>';

    card.querySelector('.btn-delete-proj').addEventListener('click', function () {
      deleteProject(p.id, p.name, card);
    });

    return card;
  }

  function deleteProject(id, name, cardEl) {
    var msg = 'Delete ‘' + name + '’? This cannot be undone. All builds, saved games, and AI conversation history will be permanently removed.';
    if (!window.confirm(msg)) return;

    apiFetch('/api/projects/' + encodeURIComponent(id), { method: 'DELETE' })
      .then(function (r) {
        if (r.status === 204 || r.ok) {
          cardEl.remove();
          // Show empty state if no cards left
          if (gridEl.querySelectorAll('.proj-card').length === 0) {
            gridEl.style.display = 'none';
            emptyEl.style.display = '';
          }
        } else {
          return r.json().catch(function () { return {}; }).then(function (d) {
            alert('Delete failed: ' + ((d && d.error) || ('HTTP ' + r.status)));
          });
        }
      })
      .catch(function (err) {
        alert('Delete failed: ' + err.message);
      });
  }

  // Format an ISO date string as a human-readable relative time.
  // Falls back to a short absolute date if the date is older than 30 days.
  function relativeDate(isoStr) {
    var d = new Date(isoStr);
    if (isNaN(d.getTime())) return '';

    var diffMs  = Date.now() - d.getTime();
    var diffSec = Math.floor(diffMs / 1000);
    var diffMin = Math.floor(diffSec / 60);
    var diffHr  = Math.floor(diffMin / 60);
    var diffDay = Math.floor(diffHr / 24);

    if (diffSec < 60)  return 'just now';
    if (diffMin < 60)  return diffMin + ' minute' + (diffMin === 1 ? '' : 's') + ' ago';
    if (diffHr < 24)   return diffHr  + ' hour'   + (diffHr  === 1 ? '' : 's') + ' ago';
    if (diffDay < 30)  return diffDay + ' day'    + (diffDay === 1 ? '' : 's') + ' ago';

    return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }

  init();
}());
