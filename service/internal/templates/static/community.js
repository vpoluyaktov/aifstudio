// Community — browse and play published games
// Globals from app.js: apiFetch, escHtml, formatDate
(function () {
  'use strict';

  var loadingEl = document.getElementById('commLoading');
  var errorEl   = document.getElementById('commError');
  var emptyEl   = document.getElementById('commEmpty');
  var gridEl    = document.getElementById('commGrid');

  function init() {
    apiFetch('/api/community?limit=50')
      .then(function (r) {
        return r.json().then(function (d) { return { ok: r.ok, status: r.status, data: d }; });
      })
      .then(function (res) {
        loadingEl.style.display = 'none';

        if (!res.ok) {
          var msg = (res.data && res.data.error) || 'Failed to load community games';
          if (res.status === 401) msg = 'You must be signed in to browse community games.';
          errorEl.innerHTML = '<div class="error-msg">' + escHtml(msg) + '</div>';
          errorEl.style.display = '';
          return;
        }

        var games = (res.data && res.data.games) || [];
        if (games.length === 0) {
          emptyEl.style.display = '';
          return;
        }

        gridEl.style.display = '';
        games.forEach(function (game) {
          gridEl.appendChild(buildCard(game));
        });
      })
      .catch(function (err) {
        loadingEl.style.display = 'none';
        errorEl.innerHTML = '<div class="error-msg">Failed to load: ' + escHtml(err.message) + '</div>';
        errorEl.style.display = '';
      });
  }

  function buildCard(game) {
    var card = document.createElement('div');
    card.className = 'game-card';

    // Truncate description to ~200 chars for the card excerpt
    var desc = game.description || 'No description provided.';
    var descTrunc = desc.length > 200 ? desc.slice(0, 197) + '…' : desc;

    // Shorten UID for display (first 8 chars, prefixed)
    var authorLabel = 'by ' + (game.ownerUid ? game.ownerUid.slice(0, 8) + '…' : 'unknown');

    var publishedDate = game.publishedAt
      ? new Date(game.publishedAt).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
      : '';

    card.innerHTML =
      '<div class="game-card-title">' + escHtml(game.name) + '</div>' +
      '<div class="game-card-desc">' + escHtml(descTrunc) + '</div>' +
      '<div class="game-card-footer">' +
        '<div class="game-card-meta">' +
          '<span class="game-card-author">' + escHtml(authorLabel) + '</span>' +
          (publishedDate ? '<span>' + escHtml(publishedDate) + '</span>' : '') +
        '</div>' +
        '<button class="btn-play-sm" data-id="' + escHtml(game.id) + '">▶ Play</button>' +
      '</div>';

    var playBtn = card.querySelector('.btn-play-sm');
    playBtn.addEventListener('click', function () {
      launchPlay(game.id, playBtn);
    });

    return card;
  }

  function launchPlay(gameId, btn) {
    btn.disabled = true;
    btn.textContent = '⏳ Starting…';

    apiFetch('/api/community/' + encodeURIComponent(gameId) + '/play', {
      method: 'POST',
      body: '{}',
    })
      .then(function (r) {
        return r.json().then(function (d) { return { ok: r.ok, status: r.status, data: d }; });
      })
      .then(function (res) {
        if (!res.ok) {
          btn.disabled = false;
          btn.textContent = '▶ Play';
          var msg = (res.data && res.data.error) || 'Could not start game';
          if (res.status === 422) msg = 'This game is not ready to play yet.';
          if (res.status === 403) msg = 'This game is no longer available.';
          alert(msg);
          return;
        }
        window.location.href = '/play/' + res.data.id;
      })
      .catch(function (err) {
        btn.disabled = false;
        btn.textContent = '▶ Play';
        alert('Failed to start game: ' + err.message);
      });
  }

  init();
}());
