// AI Workspace — SSE state machine
// Globals from app.js: apiFetch, escHtml, formatDate
(function () {
  'use strict';

  // ── State ────────────────────────────────────────────────────────────────────
  var state = 'loading'; // loading | idle | submitting | streaming | committing | error | testing
  var projectId = null;
  var project = null;
  var savedSource = '';   // last confirmed-saved source (for beforeunload guard)
  var buildPollTimer = null;
  var currentBuildHasTest = false; // whether latest succeeded build has a test script

  // ── History panel state ──────────────────────────────────────────────────────
  var historyOpen = false;
  var historyLoaded = false;
  var historyPreviewSource = null; // source text currently shown in preview pane

  // ── DOM refs ─────────────────────────────────────────────────────────────────
  var wsLoading, wsError, wsMain;
  var wsTitleEl, wsStatusEl, wsSourceEditor, wsSourceHint, wsBuildLog;
  var wsChatHistory, wsChatInput, wsSendBtn, chatEmpty;
  var saveBtn, buildBtn, playBtn, testBtn, publishBtn, publishPill, backBtn, deleteBtn;
  var editToggle;
  var sourceCollapseBtn, wsSourcePanel;
  var testCollapseBtn, chatCollapseBtn, wsChatPanel;
  var wsTestPanel, wsTestOutput, wsTestResult;
  var historyBtn, wsHistoryBackdrop, wsHistoryDrawer, wsHistoryCloseBtn;
  var wsHistoryListEl, wsHistoryLoadingEl, wsHistoryPreviewEl;
  var wsHistoryPreviewTitle, wsHistoryPreviewContent, wsHistoryRestoreBtn;
  var wsToastEl;

  // ── Boot ─────────────────────────────────────────────────────────────────────
  function init() {
    var m = location.pathname.match(/^\/projects\/([^/]+)\/ai/);
    if (!m) { showFatalError('Invalid workspace URL.'); return; }
    projectId = m[1];

    wsLoading       = document.getElementById('wsLoading');
    wsError         = document.getElementById('wsError');
    wsMain          = document.getElementById('wsMain');
    wsTitleEl       = document.getElementById('wsTitle');
    wsStatusEl      = document.getElementById('wsStatus');
    wsSourceEditor  = document.getElementById('wsSourceEditor');
    wsSourceHint    = document.getElementById('wsSourceHint');
    wsChatHistory   = document.getElementById('wsChatHistory');
    wsChatInput     = document.getElementById('wsChatInput');
    wsSendBtn       = document.getElementById('wsSendBtn');
    chatEmpty       = document.getElementById('chatEmpty');
    saveBtn         = document.getElementById('saveBtn');
    buildBtn        = document.getElementById('buildBtn');
    playBtn         = document.getElementById('playBtn');
    publishBtn      = document.getElementById('publishBtn');
    publishPill     = document.getElementById('publishPill');
    backBtn         = document.getElementById('backBtn');
    editToggle          = document.getElementById('editToggle');
    sourceCollapseBtn   = document.getElementById('sourceCollapseBtn');
    wsSourcePanel       = document.querySelector('.ws-source-panel');
    testCollapseBtn     = document.getElementById('testCollapseBtn');
    chatCollapseBtn     = document.getElementById('chatCollapseBtn');
    wsChatPanel         = document.querySelector('.ws-chat-panel');
    wsBuildLog      = document.getElementById('wsBuildLog');
    deleteBtn       = document.getElementById('deleteBtn');
    testBtn         = document.getElementById('testBtn');
    wsTestPanel     = document.getElementById('wsTestPanel');
    wsTestOutput    = document.getElementById('wsTestOutput');
    wsTestResult    = document.getElementById('wsTestResult');

    historyBtn              = document.getElementById('historyBtn');
    wsHistoryBackdrop       = document.getElementById('wsHistoryBackdrop');
    wsHistoryDrawer         = document.getElementById('wsHistoryDrawer');
    wsHistoryCloseBtn       = document.getElementById('wsHistoryCloseBtn');
    wsHistoryListEl         = document.getElementById('wsHistoryListEl');
    wsHistoryLoadingEl      = document.getElementById('wsHistoryLoadingEl');
    wsHistoryPreviewEl      = document.getElementById('wsHistoryPreviewEl');
    wsHistoryPreviewTitle   = document.getElementById('wsHistoryPreviewTitle');
    wsHistoryPreviewContent = document.getElementById('wsHistoryPreviewContent');
    wsHistoryRestoreBtn     = document.getElementById('wsHistoryRestoreBtn');
    wsToastEl               = document.getElementById('wsToast');

    // ── Wire up events ────────────────────────────────────────────────────────
    wsSendBtn.addEventListener('click', wsSend);
    saveBtn.addEventListener('click', wsSave);
    buildBtn.addEventListener('click', wsBuild);
    playBtn.addEventListener('click', wsPlay);
    testBtn.addEventListener('click', wsRunTest);
    publishBtn.addEventListener('click', wsTogglePublish);
    deleteBtn.addEventListener('click', wsDeleteProject);
    historyBtn.addEventListener('click', toggleHistoryPanel);
    wsHistoryCloseBtn.addEventListener('click', closeHistoryPanel);
    wsHistoryBackdrop.addEventListener('click', closeHistoryPanel);
    wsHistoryRestoreBtn.addEventListener('click', onRestoreVersion);

    editToggle.addEventListener('change', function () {
      wsSourceEditor.readOnly = !this.checked;
    });

    sourceCollapseBtn.addEventListener('click', function() {
      expandPanel(wsSourcePanel, sourceCollapseBtn, document.getElementById('sourceCollapseIcon'));
    });
    if (testCollapseBtn) {
      testCollapseBtn.addEventListener('click', function() {
        expandPanel(wsTestPanel, testCollapseBtn, document.getElementById('testCollapseIcon'));
      });
    }
    if (chatCollapseBtn) {
      chatCollapseBtn.addEventListener('click', function() {
        expandPanel(wsChatPanel, chatCollapseBtn, document.getElementById('chatCollapseIcon'));
      });
    }

    // On desktop, the test panel starts hidden (shown only during/after test run).
    // On mobile it is always in the accordion but starts collapsed via HTML class.
    if (wsTestPanel && !window.matchMedia('(max-width: 700px)').matches) {
      wsTestPanel.style.display = 'none';
    }

    // Cmd/Ctrl+Enter sends message
    wsChatInput.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        wsSend();
      }
    });

    // Warn on navigation away if source has unsaved changes
    window.addEventListener('beforeunload', function (e) {
      if (state !== 'loading' && wsSourceEditor.value !== savedSource) {
        e.preventDefault();
        e.returnValue = 'You have unsaved changes to your source. Leave anyway?';
      }
    });

    loadProject(true);
  }

  // ── Project loading ──────────────────────────────────────────────────────────
  function loadProject(isFirstLoad) {
    apiFetch('/api/projects/' + encodeURIComponent(projectId))
      .then(function (r) {
        return r.json().then(function (d) { return { ok: r.ok, status: r.status, data: d }; });
      })
      .then(function (res) {
        if (!res.ok) {
          var msg = (res.data && res.data.error) || 'Failed to load project';
          if (res.status === 404) msg = 'Project not found.';
          if (res.status === 403) msg = 'You do not have access to this project.';
          showFatalError(msg);
          return;
        }
        project = res.data;

        // Update page metadata
        wsTitleEl.textContent = project.name;
        document.title = project.name + ' — AI Workspace — StoryCloud';
        backBtn.href = '/projects/' + encodeURIComponent(projectId);

        // Update toolbar UI
        updatePublishUI();

        // Fetch source from signed GCS URL
        fetchSource(project.sourceUrl, function (err, src) {
          if (err) {
            setStatus('Warning: could not load source — ' + err, 'warning');
          }
          wsSourceEditor.value = src || '';
          savedSource = wsSourceEditor.value;

          // If latestBuildId exists, check build status for Play button
          if (project.latestBuildId) {
            checkBuildStatus(project.latestBuildId);
          }

          // Show workspace
          wsLoading.style.display = 'none';
          wsMain.style.display = 'flex';

          setState('idle');

          // Autostart: triggered from create.html "Generate with AI"
          if (isFirstLoad) {
            var params = new URLSearchParams(location.search);
            if (params.get('autostart') === '1') {
              history.replaceState({}, '', location.pathname);
              setTimeout(startGenerate, 150);
            }
          }
        });
      })
      .catch(function (err) {
        showFatalError('Failed to load project: ' + err.message);
      });
  }

  // Fetch source bytes from a signed GCS URL.
  // If the URL is expired (403/410), refreshes it via GET /api/projects/{id}.
  function fetchSource(url, cb) {
    if (!url) { cb(null, ''); return; }

    fetch(url)
      .then(function (r) {
        if (r.status === 403 || r.status === 410) {
          // Signed URL expired — get a fresh one
          return apiFetch('/api/projects/' + encodeURIComponent(projectId))
            .then(function (r2) { return r2.json(); })
            .then(function (d) {
              if (!d.sourceUrl) return '';
              project.sourceUrl = d.sourceUrl;
              return fetch(d.sourceUrl).then(function (r3) {
                return r3.ok ? r3.text() : '';
              });
            });
        }
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.text();
      })
      .then(function (src) { cb(null, src || ''); })
      .catch(function (err) { cb(err.message, ''); });
  }

  // Check the status of a build once (not polling) to determine Play/Test button visibility.
  function checkBuildStatus(buildId) {
    apiFetch('/api/projects/' + encodeURIComponent(projectId) + '/builds/' + encodeURIComponent(buildId))
      .then(function (r) { return r.json(); })
      .then(function (b) {
        if (b.status === 'succeeded') {
          playBtn.style.display = '';
          currentBuildHasTest = !!b.hasTest;
          testBtn.style.display = currentBuildHasTest ? '' : 'none';
        }
        if (b.status === 'pending' || b.status === 'running') {
          // Build in progress — start polling so Play/Test appear when done
          setStatus('⚙ Build in progress…', 'info');
          scheduleBuildPoll(buildId);
        }
      })
      .catch(function () { /* non-fatal — Play/Test buttons stay hidden */ });
  }

  // ── State machine ────────────────────────────────────────────────────────────
  function setState(newState) {
    state = newState;
    var isIdle = (state === 'idle' || state === 'error');
    wsSendBtn.disabled   = !isIdle;
    wsChatInput.disabled = !isIdle;
    editToggle.disabled  = !isIdle;
    saveBtn.disabled     = !isIdle;
    buildBtn.disabled    = !isIdle;
    if (testBtn) testBtn.disabled = !isIdle;

    switch (state) {
      case 'idle':
      case 'error':
        wsSendBtn.textContent = 'Send';
        break;
      case 'submitting':
        wsSendBtn.textContent = '⏳ Sending…';
        setStatus('Connecting to AI…', 'info');
        break;
      case 'streaming':
        wsSendBtn.textContent = '⏳ Streaming…';
        break;
      case 'committing':
        wsSendBtn.textContent = '⏳ Saving…';
        setStatus('Saving updated source…', 'info');
        break;
      case 'testing':
        wsSendBtn.textContent = 'Send';
        break;
    }
  }

  // ── Auto-generate (autostart=1) ──────────────────────────────────────────────
  function startGenerate() {
    if (state !== 'idle') return;
    setState('submitting');

    var userLabel = project && project.description
      ? '✦ Generating: ' + project.description.slice(0, 120) + (project.description.length > 120 ? '…' : '')
      : '✦ Auto-generating from description…';

    var turnEl   = appendChatTurn(userLabel, null, true /* isSystem */);
    var replyEl  = turnEl.querySelector('.chat-ai-reply');

    setStreamingHint(true);

    callAISSE(
      '/api/projects/' + encodeURIComponent(projectId) + '/ai/generate',
      {},
      replyEl,
      function (err) {
        setStreamingHint(false);
        if (err) {
          if (err === 'no_fence') {
            setState('idle');
            setStatus('Generation response missing code block — please try again.', 'error');
            clearStatusAfter(8000);
          } else {
            setState('error');
            replyEl.classList.remove('streaming');
            replyEl.classList.add('error-reply');
            replyEl.textContent = '⚠ ' + err;
            setStatus('Generation failed: ' + err, 'error');
          }
        } else {
          setState('idle');
          setStatus('✓ Source generated', 'success');
          clearStatusAfter(4000);
        }
      }
    );
  }

  // ── Chat send ────────────────────────────────────────────────────────────────
  function wsSend() {
    if (state !== 'idle') return;
    var msg = wsChatInput.value.trim();
    if (!msg) { wsChatInput.focus(); return; }

    wsChatInput.value = '';
    setState('submitting');

    var turnEl  = appendChatTurn(msg, null, false);
    var replyEl = turnEl.querySelector('.chat-ai-reply');

    setStreamingHint(true);

    callAISSE(
      '/api/projects/' + encodeURIComponent(projectId) + '/ai/chat',
      { message: msg },
      replyEl,
      function (err) {
        setStreamingHint(false);
        if (err) {
          if (err === 'no_fence') {
            setState('idle');
            setStatus('Source not updated — try rephrasing your request.', 'error');
            clearStatusAfter(8000);
          } else {
            setState('error');
            replyEl.classList.remove('streaming');
            replyEl.classList.add('error-reply');
            replyEl.textContent = '⚠ ' + err;
            setStatus('AI error: ' + err, 'error');
          }
        } else {
          setState('idle');
          setStatus('');
        }
      }
    );
  }

  // ── SSE consumer ─────────────────────────────────────────────────────────────
  // Opens an authenticated POST to an SSE endpoint, parses the stream,
  // and calls done(error|null) when the stream ends.
  function callAISSE(endpoint, body, replyEl, done) {
    apiFetch(endpoint, {
      method: 'POST',
      body: JSON.stringify(body),
    })
      .then(function (resp) {
        if (!resp.ok) {
          return resp.json()
            .catch(function () { return {}; })
            .then(function (d) {
              var msg = (d && d.error) || ('HTTP ' + resp.status);
              if (resp.status === 409) {
                var code = d && d.code;
                if (code === 'conversation_full') msg = 'Conversation limit reached (200 turns). Please create a new project to continue.';
                if (code === 'ai_already_generated') msg = 'This project already has AI turns. Use chat to continue.';
              }
              if (resp.status === 429) msg = 'AI rate limit reached. Please wait a moment and try again.';
              if (resp.status === 503 || resp.status === 502) msg = 'AI service unavailable. Check your OpenAI configuration.';
              done(msg);
            });
        }

        setState('streaming');

        return consumeSSE(resp, function (event, data) {
          switch (event) {
            case 'start':
              // Stream started — nothing to display yet
              break;

            case 'delta':
              if (data.delta) {
                var current = replyEl.dataset.replyBuf || '';
                current += data.delta;
                replyEl.dataset.replyBuf = current;
                // Find where the fence starts so we don't show source code inline
                var fenceIdx = -1;
                var fenceTags = ['```inform7\n', '```i7\n', '```\n'];
                for (var fi = 0; fi < fenceTags.length; fi++) {
                  var idx = current.indexOf(fenceTags[fi]);
                  if (idx !== -1 && (fenceIdx === -1 || idx < fenceIdx)) fenceIdx = idx;
                }
                if (fenceIdx !== -1) {
                  var prose = current.slice(0, fenceIdx).trim();
                  replyEl.textContent = (prose ? prose + '\n\n' : '') + '✦ Writing source…';
                  replyEl.dataset.fenceSeen = '1';
                } else {
                  replyEl.textContent = current;
                }
                scrollChatToBottom();
              }
              break;

            case 'source':
              if (typeof data.source === 'string') {
                wsSourceEditor.value = data.source;
              }
              break;

            case 'done':
              setState('committing');
              replyEl.classList.remove('streaming');
              delete replyEl.dataset.replyBuf;
              delete replyEl.dataset.fenceSeen;
              if (data.assistantReply) {
                replyEl.textContent = data.assistantReply;
              }
              // Persist updated source as saved baseline
              savedSource = wsSourceEditor.value;
              // Update project metadata
              if (project && data.updatedAt) project.updatedAt = data.updatedAt;
              // Annotate turn with token stats
              var metaEl = replyEl.nextElementSibling;
              if (metaEl && metaEl.className === 'chat-turn-meta' && data.model) {
                var tokens = (data.promptTokens || 0) + (data.completionTokens || 0);
                metaEl.textContent = data.model + ' · ' + tokens + ' tokens';
              }
              scrollChatToBottom();
              done(null);
              break;

            case 'error':
              var errMsg = (data && data.error) || (data && data.code) || 'Unknown AI error';
              var errCode = data && data.code;
              if (errCode === 'no_fence') {
                // Preserve the streamed delta content — don't blank it out.
                replyEl.classList.remove('streaming');
                delete replyEl.dataset.replyBuf;
                // Append a small inline warning after the reply element.
                var warnEl = document.createElement('span');
                warnEl.className = 'no-fence-warning';
                warnEl.textContent = '⚠ Source not updated — AI response was missing the code block. Try rephrasing your request.';
                replyEl.parentNode.insertBefore(warnEl, replyEl.nextSibling);
                done('no_fence');
              } else {
                done(errMsg);
              }
              break;
          }
        }).catch(function (err) {
          done(err.message || 'Stream read error');
        });
      })
      .catch(function (err) {
        done(err.message || 'Request failed');
      });
  }

  // Parse SSE from a fetch Response body (ReadableStream).
  // Calls onEvent(eventType, parsedData) for each complete event.
  // Returns a Promise that resolves after 'done'/'error' or stream close.
  function consumeSSE(resp, onEvent) {
    return new Promise(function (resolve, reject) {
      var reader  = resp.body.getReader();
      var decoder = new TextDecoder();
      var buf     = '';

      function pump() {
        reader.read().then(function (ref) {
          if (ref.done) { resolve(); return; }

          buf += decoder.decode(ref.value, { stream: true });

          var shouldStop = false;
          var idx;
          while (!shouldStop && (idx = buf.indexOf('\n\n')) !== -1) {
            var block = buf.slice(0, idx);
            buf = buf.slice(idx + 2);

            // Skip empty blocks and comment lines (heartbeats)
            var trimmed = block.trim();
            if (!trimmed || trimmed.charAt(0) === ':') continue;

            var eventType = '';
            var dataStr   = '';
            var lines     = block.split('\n');
            for (var i = 0; i < lines.length; i++) {
              var line = lines[i];
              if (line.slice(0, 7) === 'event: ') {
                eventType = line.slice(7).trim();
              } else if (line.slice(0, 6) === 'data: ') {
                dataStr = line.slice(6);
              }
            }

            if (!eventType || !dataStr) continue;

            var data;
            try { data = JSON.parse(dataStr); } catch (e) { continue; }

            onEvent(eventType, data);

            if (eventType === 'done' || eventType === 'error') {
              shouldStop = true;
            }
          }

          if (shouldStop) {
            reader.cancel();
            resolve();
          } else {
            pump();
          }
        }).catch(reject);
      }

      pump();
    });
  }

  // ── Save ──────────────────────────────────────────────────────────────────────
  function wsSave() {
    if (state !== 'idle') return;
    var src = wsSourceEditor.value;
    saveBtn.disabled = true;
    saveBtn.textContent = '⏳ Saving…';
    setStatus('Saving…', 'info');

    apiFetch('/api/projects/' + encodeURIComponent(projectId) + '/source', {
      method: 'PATCH',
      body: JSON.stringify({ source: src }),
    })
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, data: d }; }); })
      .then(function (res) {
        saveBtn.disabled = false;
        saveBtn.textContent = '💾 Save';
        if (!res.ok) {
          var msg = (res.data && res.data.error) || 'Unknown error';
          setStatus('Save failed: ' + msg, 'error');
        } else {
          savedSource = src;
          if (project && res.data.updatedAt) project.updatedAt = res.data.updatedAt;
          setStatus('✓ Saved (' + (res.data.sourceBytes || 0) + ' bytes)', 'success');
          clearStatusAfter(3000);
        }
      })
      .catch(function (err) {
        saveBtn.disabled = false;
        saveBtn.textContent = '💾 Save';
        setStatus('Save failed: ' + err.message, 'error');
      });
  }

  // ── Build ─────────────────────────────────────────────────────────────────────
  function wsBuild() {
    if (state !== 'idle') return;
    buildBtn.disabled = true;
    buildBtn.textContent = '⏳ Building…';
    setStatus('Requesting build…', 'info');

    apiFetch('/api/projects/' + encodeURIComponent(projectId) + '/builds', {
      method: 'POST',
      body: '{}',
    })
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, status: r.status, data: d }; }); })
      .then(function (res) {
        buildBtn.disabled = false;
        buildBtn.textContent = '⚙ Build';
        if (!res.ok) {
          var msg = (res.data && res.data.error) || 'Build request failed';
          if (res.status === 409) msg = 'A build is already in progress.';
          setStatus(msg, 'error');
          return;
        }
        if (project) project.latestBuildId = res.data.id;
        playBtn.style.display = 'none';
        testBtn.style.display = 'none';
        currentBuildHasTest = false;
        hideBuildLog();
        hideTestPanel();
        setStatus('⚙ Build queued…', 'info');
        scheduleBuildPoll(res.data.id);
      })
      .catch(function (err) {
        buildBtn.disabled = false;
        buildBtn.textContent = '⚙ Build';
        setStatus('Build failed: ' + err.message, 'error');
      });
  }

  function hideBuildLog() {
    if (wsBuildLog) {
      wsBuildLog.style.display = 'none';
      wsBuildLog.innerHTML = '';
    }
  }

  function hideTestPanel() {
    if (wsTestPanel) {
      if (window.matchMedia('(max-width: 700px)').matches) {
        // Mobile: collapse the panel but keep it in the DOM (accordion section)
        wsTestPanel.classList.add('collapsed');
        if (testCollapseBtn) testCollapseBtn.setAttribute('aria-expanded', 'false');
        var icon = document.getElementById('testCollapseIcon');
        if (icon) icon.textContent = '▸';
      } else {
        wsTestPanel.style.display = 'none';
      }
      wsTestOutput.textContent = '';
      wsTestResult.style.display = 'none';
      wsTestResult.className = 'ws-test-result';
    }
  }

  // Fetch the build log from a signed URL, render the collapsible panel,
  // and pre-fill the chat input. Falls back to fallbackMsg if fetch fails.
  function showBuildLog(logURL, fallbackMsg) {
    fetch(logURL)
      .then(function (r) { return r.text(); })
      .then(function (text) {
        // Render collapsible log panel
        var lines = text.split('\n').length;
        var details = document.createElement('details');
        var summary = document.createElement('summary');
        summary.textContent = '▶ Build log (' + lines + ' lines) — click to expand';
        var pre = document.createElement('pre');
        pre.textContent = text;
        details.appendChild(summary);
        details.appendChild(pre);
        wsBuildLog.innerHTML = '';
        wsBuildLog.appendChild(details);
        wsBuildLog.className = 'ws-build-log';
        wsBuildLog.style.display = '';

        // Pre-fill chat with full log text (preferred over truncated errorMessage)
        var logContent = text.trim() || fallbackMsg;
        if (logContent && wsChatInput) {
          wsChatInput.value = 'The build failed with the following compiler errors:\n\n' + logContent + '\n\nPlease fix all errors in the Inform 7 source.';
          wsChatInput.focus();
        }
      })
      .catch(function () {
        // Log fetch failed — fall back to errorMessage for the pre-fill
        if (fallbackMsg && wsChatInput) {
          wsChatInput.value = 'The build failed with the following errors:\n\n' + fallbackMsg + '\n\nPlease fix the Inform 7 source.';
          wsChatInput.focus();
        }
      });
  }

  function scheduleBuildPoll(buildId) {
    clearTimeout(buildPollTimer);
    buildPollTimer = setTimeout(function () { pollBuild(buildId); }, 4000);
  }

  function pollBuild(buildId) {
    apiFetch('/api/projects/' + encodeURIComponent(projectId) + '/builds/' + encodeURIComponent(buildId))
      .then(function (r) { return r.json(); })
      .then(function (b) {
        if (b.status === 'pending' || b.status === 'running') {
          setStatus('⚙ Building… (' + b.status + ')', 'info');
          scheduleBuildPoll(buildId);
          return;
        }
        if (b.status === 'succeeded') {
          playBtn.style.display = '';
          currentBuildHasTest = !!b.hasTest;
          testBtn.style.display = currentBuildHasTest ? '' : 'none';
          setStatus('✓ Build succeeded — ▶ Play is ready', 'success');
          clearStatusAfter(5000);
          hideBuildLog();
        } else {
          setStatus('✗ Build failed — see log below', 'error');
          if (b.logURL) {
            // Fetch log: renders collapsible panel and pre-fills chat with full output.
            // Falls back to errorMessage if the fetch fails.
            showBuildLog(b.logURL, b.errorMessage);
          } else if (b.errorMessage && wsChatInput) {
            // No log URL — pre-fill with errorMessage directly.
            wsChatInput.value = 'The build failed with the following errors:\n\n' + b.errorMessage + '\n\nPlease fix the Inform 7 source.';
            wsChatInput.focus();
          }
        }
      })
      .catch(function () {
        setStatus('Could not check build status.', 'warning');
      });
  }

  // ── Run Test ──────────────────────────────────────────────────────────────────
  // Streams the test SSE, shows output in the test panel, and auto-triggers
  // an AI fix via the chat endpoint if the game is not winnable.
  function wsRunTest() {
    if (state !== 'idle') return;
    if (!project || !project.latestBuildId) return;

    setState('testing');
    testBtn.textContent = '⏳ Testing…';

    // Show (or reset) the test panel
    if (window.matchMedia('(max-width: 700px)').matches) {
      // Mobile: expand via accordion
      expandPanel(wsTestPanel, testCollapseBtn, document.getElementById('testCollapseIcon'));
    } else {
      wsTestPanel.style.display = '';
    }
    wsTestOutput.textContent = '';
    wsTestResult.style.display = 'none';
    wsTestResult.className = 'ws-test-result';
    setStatus('▶ Running test…', 'info');

    var testCompleted = false;

    apiFetch('/api/builds/' + encodeURIComponent(project.latestBuildId) + '/test', {
      method: 'POST',
      body: '{}',
    })
      .then(function (resp) {
        if (!resp.ok) {
          return resp.json()
            .catch(function () { return {}; })
            .then(function (d) {
              onTestFinished(null, (d && d.error) || ('HTTP ' + resp.status), '');
              testCompleted = true;
            });
        }

        return consumeTestSSE(resp, function (data) {
          if (data.type === 'output') {
            appendTestLine(data.line || '');
          } else if (data.type === 'result') {
            testCompleted = true;
            onTestFinished(!!data.won, null, data.transcript || '');
          }
        })
          .then(function () {
            if (!testCompleted) {
              onTestFinished(null, 'Test stream ended without a result', '');
            }
          })
          .catch(function (err) {
            if (!testCompleted) {
              onTestFinished(null, err.message || 'Stream error', '');
            }
          });
      })
      .catch(function (err) {
        onTestFinished(null, err.message || 'Request failed', '');
      });
  }

  function appendTestLine(line) {
    wsTestOutput.textContent += line + '\n';
    wsTestOutput.scrollTop = wsTestOutput.scrollHeight;
  }

  function onTestFinished(won, err, transcript) {
    testBtn.textContent = '🧪 Run Test';
    setState('idle');

    // On mobile, ensure test panel stays expanded so results are visible
    if (window.matchMedia('(max-width: 700px)').matches) {
      expandPanel(wsTestPanel, testCollapseBtn, document.getElementById('testCollapseIcon'));
    }

    if (err !== null) {
      wsTestResult.textContent = '⚠ Test error: ' + err;
      wsTestResult.className = 'ws-test-result error-result';
      wsTestResult.style.display = '';
      setStatus('Test error: ' + err, 'error');
      return;
    }

    wsTestResult.style.display = '';
    if (won) {
      wsTestResult.textContent = '✓ Test passed — game is winnable';
      wsTestResult.className = 'ws-test-result pass';
      setStatus('✓ Test passed', 'success');
      clearStatusAfter(5000);
    } else {
      wsTestResult.textContent = '✗ Test failed';
      wsTestResult.className = 'ws-test-result fail';
      setStatus('✗ Test failed', 'error');
      if (wsChatInput) {
        const maxTranscript = 10000;
        let t = transcript;
        if (t.length > maxTranscript) {
          t = '[transcript truncated — showing last ' + maxTranscript + ' characters]\n...\n' +
            t.slice(t.length - maxTranscript);
        }
        wsChatInput.value = 'The automated test failed. Here is the game transcript:\n\n' +
          t + '\n\nPlease analyze what went wrong and fix the source code.';
        wsChatInput.focus();
      }
    }
  }

  // Sends an AI chat message programmatically and renders the response in the
  // conversation panel exactly as a user-initiated message would appear.
  function triggerAIFix(msg) {
    setState('submitting');

    var turnEl  = appendChatTurn(msg, null, false);
    var replyEl = turnEl.querySelector('.chat-ai-reply');

    setStreamingHint(true);

    callAISSE(
      '/api/projects/' + encodeURIComponent(projectId) + '/ai/chat',
      { message: msg },
      replyEl,
      function (err) {
        setStreamingHint(false);
        if (err) {
          if (err === 'no_fence') {
            setState('idle');
            setStatus('AI fix: source not updated — try rephrasing.', 'error');
            clearStatusAfter(8000);
          } else {
            setState('error');
            replyEl.classList.remove('streaming');
            replyEl.classList.add('error-reply');
            replyEl.textContent = '⚠ ' + err;
            setStatus('AI fix error: ' + err, 'error');
          }
        } else {
          setState('idle');
          setStatus('✗ Test failed — AI fix applied. Rebuild to verify.', 'warning');
        }
      }
    );
  }

  // Parse a test SSE stream (data-only events, type embedded in JSON).
  // Calls onData(parsedObject) for each data line; resolves when stream closes.
  function consumeTestSSE(resp, onData) {
    return new Promise(function (resolve, reject) {
      var reader  = resp.body.getReader();
      var decoder = new TextDecoder();
      var buf     = '';

      function pump() {
        reader.read().then(function (ref) {
          if (ref.done) { resolve(); return; }
          buf += decoder.decode(ref.value, { stream: true });

          var idx;
          while ((idx = buf.indexOf('\n')) !== -1) {
            var line = buf.slice(0, idx);
            buf = buf.slice(idx + 1);
            var trimmed = line.trim();
            if (trimmed.slice(0, 6) === 'data: ') {
              var dataStr = trimmed.slice(6);
              try {
                var data = JSON.parse(dataStr);
                onData(data);
              } catch (e) { /* skip malformed data lines */ }
            }
          }
          pump();
        }).catch(reject);
      }

      pump();
    });
  }

  // ── Play ──────────────────────────────────────────────────────────────────────
  // Cache key stores { buildId, runId } so we can restart the same run instead
  // of creating a new history entry on every play click.
  function playRunCacheKey() { return 'ws_run_' + projectId; }

  function wsPlay() {
    if (!project || !project.latestBuildId) return;
    playBtn.disabled = true;
    playBtn.textContent = '⏳ Starting…';

    var buildId = project.latestBuildId;
    var cached = null;
    try {
      var raw = localStorage.getItem(playRunCacheKey());
      if (raw) {
        var parsed = JSON.parse(raw);
        if (parsed && parsed.buildId === buildId) cached = parsed.runId;
      }
    } catch (_) {}

    function createAndGo() {
      apiFetch('/api/runs', {
        method: 'POST',
        body: JSON.stringify({ sourceType: 'build', buildId: buildId }),
      })
        .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, data: d }; }); })
        .then(function (res) {
          if (!res.ok) {
            playBtn.disabled = false;
            playBtn.textContent = '▶ Play';
            setStatus('Could not start: ' + ((res.data && res.data.error) || 'error'), 'error');
            return;
          }
          try { localStorage.setItem(playRunCacheKey(), JSON.stringify({ buildId: buildId, runId: res.data.id })); } catch (_) {}
          window.location.href = '/play/' + res.data.id;
        })
        .catch(function (err) {
          playBtn.disabled = false;
          playBtn.textContent = '▶ Play';
          setStatus('Play failed: ' + err.message, 'error');
        });
    }

    if (cached) {
      apiFetch('/api/runs/' + encodeURIComponent(cached) + '/restart', { method: 'POST' })
        .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, status: r.status, data: d }; }); })
        .then(function (res) {
          if (res.ok) {
            window.location.href = '/play/' + cached;
          } else if (res.status === 404 || res.status === 403) {
            // Run was deleted or belongs to another user — create fresh.
            try { localStorage.removeItem(playRunCacheKey()); } catch (_) {}
            createAndGo();
          } else {
            playBtn.disabled = false;
            playBtn.textContent = '▶ Play';
            setStatus('Could not start: ' + ((res.data && res.data.error) || 'error'), 'error');
          }
        })
        .catch(function (err) {
          playBtn.disabled = false;
          playBtn.textContent = '▶ Play';
          setStatus('Play failed: ' + err.message, 'error');
        });
    } else {
      createAndGo();
    }
  }

  // ── Publish ───────────────────────────────────────────────────────────────────
  function wsTogglePublish() {
    if (!project) return;
    var newPublished = !project.published;
    publishBtn.disabled = true;
    publishBtn.textContent = '⏳ …';

    apiFetch('/api/projects/' + encodeURIComponent(projectId) + '/publish', {
      method: 'PATCH',
      body: JSON.stringify({ published: newPublished }),
    })
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, status: r.status, data: d }; }); })
      .then(function (res) {
        publishBtn.disabled = false;
        if (!res.ok) {
          var msg = (res.data && res.data.error) || 'Failed';
          if (res.status === 422) {
            var code = res.data && res.data.code;
            if (code === 'build_required') msg = 'Build the project first (needs a successful build).';
            if (code === 'empty_source')   msg = 'Save source before publishing.';
          }
          setStatus('Publish failed: ' + msg, 'error');
          updatePublishUI();
          return;
        }
        project.published   = res.data.published;
        project.publishedAt = res.data.publishedAt;
        updatePublishUI();
        setStatus(project.published ? '✓ Published to community' : '✓ Unpublished', 'success');
        clearStatusAfter(3000);
      })
      .catch(function (err) {
        publishBtn.disabled = false;
        updatePublishUI();
        setStatus('Publish failed: ' + err.message, 'error');
      });
  }

  function updatePublishUI() {
    if (!project) return;
    publishPill.style.display = '';
    if (project.published) {
      publishBtn.textContent   = '🔒 Unpublish';
      publishPill.className    = 'publish-pill published';
      publishPill.textContent  = 'Published';
    } else {
      publishBtn.textContent   = '🌐 Publish';
      publishPill.className    = 'publish-pill unpublished';
      publishPill.textContent  = 'Unpublished';
    }
  }

  // ── Delete project ────────────────────────────────────────────────────────────
  function wsDeleteProject() {
    if (!project) return;
    var msg = 'Delete ‘' + project.name + '’? This cannot be undone. All builds, saved games, and AI conversation history will be permanently removed.';
    if (!window.confirm(msg)) return;

    deleteBtn.disabled = true;
    deleteBtn.textContent = '⏳ Deleting…';

    apiFetch('/api/projects/' + encodeURIComponent(projectId), { method: 'DELETE' })
      .then(function (r) {
        if (r.status === 204 || r.ok) {
          window.location.href = '/projects';
        } else {
          return r.json().catch(function () { return {}; }).then(function (d) {
            deleteBtn.disabled = false;
            deleteBtn.textContent = '🗑 Delete';
            setStatus('Delete failed: ' + ((d && d.error) || ('HTTP ' + r.status)), 'error');
          });
        }
      })
      .catch(function (err) {
        deleteBtn.disabled = false;
        deleteBtn.textContent = '🗑 Delete';
        setStatus('Delete failed: ' + err.message, 'error');
      });
  }

  // ── Chat helpers ──────────────────────────────────────────────────────────────
  // Appends a new turn row: user bubble + AI reply placeholder.
  // isSystem=true renders the user message as a subtle system label.
  // Returns the turn DOM element.
  function appendChatTurn(userText, aiText, isSystem) {
    chatEmpty.style.display = 'none';

    var div = document.createElement('div');
    div.className = 'chat-turn';

    var userCls = 'chat-user-msg' + (isSystem ? ' system-msg' : '');
    var replyContent = aiText
      ? escHtml(aiText)
      : '<span class="chat-spinner"></span>Thinking…';

    div.innerHTML =
      '<div class="' + userCls + '">' + escHtml(userText) + '</div>' +
      '<div class="chat-ai-reply streaming">' + replyContent + '</div>' +
      '<div class="chat-turn-meta"></div>';

    wsChatHistory.appendChild(div);
    scrollChatToBottom();
    return div;
  }

  function scrollChatToBottom() {
    wsChatHistory.scrollTop = wsChatHistory.scrollHeight;
  }

  // ── Source hint ───────────────────────────────────────────────────────────────
  function setStreamingHint(on) {
    if (on) {
      wsSourceHint.textContent = '✦ AI is writing…';
      wsSourceHint.className   = 'ws-source-hint streaming';
      wsSourceEditor.readOnly  = true;
      editToggle.checked       = false;
    } else {
      wsSourceHint.textContent = '';
      wsSourceHint.className   = 'ws-source-hint';
      // Restore editable state per toggle
      wsSourceEditor.readOnly  = !editToggle.checked;
    }
  }

  // ── Status bar ────────────────────────────────────────────────────────────────
  var statusClearTimer = null;

  function setStatus(msg, cls) {
    clearTimeout(statusClearTimer);
    wsStatusEl.textContent = msg;
    wsStatusEl.className   = 'ws-status' + (cls ? ' ' + cls : '');
  }

  function clearStatusAfter(ms) {
    statusClearTimer = setTimeout(function () { setStatus(''); }, ms);
  }

  // ── Fatal error ───────────────────────────────────────────────────────────────
  function showFatalError(msg) {
    if (wsLoading) wsLoading.style.display = 'none';
    if (wsError) {
      wsError.style.display = '';
      wsError.innerHTML = '<div class="error-msg">' + escHtml(msg) + '</div>';
    }
  }

  // ── Panel accordion (mobile) ─────────────────────────────────────────────────

  function expandPanel(panelEl, btnEl, iconEl) {
    // Collapse all three panels first
    [wsSourcePanel, wsTestPanel, wsChatPanel].forEach(function(p) {
      if (p) p.classList.add('collapsed');
    });
    [sourceCollapseBtn, testCollapseBtn, chatCollapseBtn].forEach(function(b) {
      if (b) b.setAttribute('aria-expanded', 'false');
    });
    ['sourceCollapseIcon', 'testCollapseIcon', 'chatCollapseIcon'].forEach(function(id) {
      var el = document.getElementById(id);
      if (el) el.textContent = '▸';
    });
    // Expand the target panel
    panelEl.classList.remove('collapsed');
    if (btnEl) btnEl.setAttribute('aria-expanded', 'true');
    if (iconEl) iconEl.textContent = '▾';
  }

  // ── History panel ─────────────────────────────────────────────────────────────

  function toggleHistoryPanel() {
    if (historyOpen) { closeHistoryPanel(); } else { openHistoryPanel(); }
  }

  function openHistoryPanel() {
    historyOpen = true;
    wsHistoryDrawer.classList.add('open');
    wsHistoryBackdrop.classList.add('open');
    historyBtn.textContent = '✕ History';
    if (!historyLoaded) { loadHistory(); }
  }

  function closeHistoryPanel() {
    historyOpen = false;
    wsHistoryDrawer.classList.remove('open');
    wsHistoryBackdrop.classList.remove('open');
    historyBtn.textContent = '📜 History';
  }

  function loadHistory() {
    wsHistoryLoadingEl.style.display = '';
    // Clear previous entries (keep the loading element)
    while (wsHistoryListEl.children.length > 1) {
      wsHistoryListEl.removeChild(wsHistoryListEl.lastChild);
    }
    wsHistoryPreviewEl.style.display = 'none';
    historyPreviewSource = null;

    apiFetch('/api/projects/' + encodeURIComponent(projectId) + '/history')
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, data: d }; }); })
      .then(function (res) {
        wsHistoryLoadingEl.style.display = 'none';
        if (!res.ok) {
          var errEl = document.createElement('div');
          errEl.className = 'ws-history-empty';
          errEl.textContent = 'Failed to load history.';
          wsHistoryListEl.appendChild(errEl);
          return;
        }
        var turns = Array.isArray(res.data) ? res.data : [];
        historyLoaded = true;
        if (turns.length === 0) {
          var emptyEl = document.createElement('div');
          emptyEl.className = 'ws-history-empty';
          emptyEl.textContent = 'No history yet.';
          wsHistoryListEl.appendChild(emptyEl);
          return;
        }
        turns.forEach(function (turn) {
          wsHistoryListEl.appendChild(buildHistoryEntry(turn));
        });
      })
      .catch(function (err) {
        wsHistoryLoadingEl.style.display = 'none';
        var errEl = document.createElement('div');
        errEl.className = 'ws-history-empty';
        errEl.textContent = 'Error loading history: ' + err.message;
        wsHistoryListEl.appendChild(errEl);
      });
  }

  function buildHistoryEntry(turn) {
    var div = document.createElement('div');
    var hasSource = !!turn.hasSource;
    div.className = 'ws-history-entry' + (hasSource ? '' : ' no-source');
    div.dataset.turnId = turn.id;

    var kindLabel = turn.kind === 'generate' ? 'Generate' : 'Chat';
    var kindCls   = turn.kind === 'generate' ? 'generate' : 'chat';
    var timeStr   = histRelativeTime(turn.createdAt);
    var msgText   = (turn.userMessage || '');
    var preview   = msgText.slice(0, 80) + (msgText.length > 80 ? '…' : '');

    div.innerHTML =
      '<div class="ws-history-entry-top">' +
        '<span class="ws-hist-badge ' + escHtml(kindCls) + '">' + escHtml(kindLabel) + '</span>' +
        '<span class="ws-history-entry-time">' + escHtml(timeStr) + '</span>' +
      '</div>' +
      '<div class="ws-history-entry-msg">' + escHtml(preview || '(no message)') + '</div>';

    if (hasSource) {
      div.addEventListener('click', function () {
        // Deselect all
        var sel = wsHistoryListEl.querySelectorAll('.ws-history-entry.selected');
        for (var i = 0; i < sel.length; i++) { sel[i].classList.remove('selected'); }
        div.classList.add('selected');
        loadTurnSource(turn.id);
      });
    }

    return div;
  }

  function loadTurnSource(turnId) {
    wsHistoryPreviewEl.style.display = '';
    wsHistoryPreviewTitle.textContent = 'Loading preview…';
    wsHistoryPreviewContent.textContent = '';
    wsHistoryRestoreBtn.disabled = true;
    historyPreviewSource = null;

    apiFetch(
      '/api/projects/' + encodeURIComponent(projectId) +
      '/history/' + encodeURIComponent(turnId) + '/source'
    )
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, data: d }; }); })
      .then(function (res) {
        if (!res.ok) {
          wsHistoryPreviewTitle.textContent = 'Preview failed';
          wsHistoryPreviewContent.textContent = 'Could not load source for this version.';
          return;
        }
        var src = (res.data && typeof res.data.source === 'string') ? res.data.source : '';
        historyPreviewSource = src;
        wsHistoryPreviewContent.textContent = src;
        wsHistoryPreviewTitle.textContent = 'Source preview';
        wsHistoryRestoreBtn.disabled = false;
      })
      .catch(function (err) {
        wsHistoryPreviewTitle.textContent = 'Preview error';
        wsHistoryPreviewContent.textContent = err.message;
      });
  }

  function onRestoreVersion() {
    if (!historyPreviewSource) return;
    var ok = window.confirm(
      'Restore this version? The current source will be replaced. This won\'t delete any history.'
    );
    if (!ok) return;

    wsHistoryRestoreBtn.disabled = true;
    wsHistoryRestoreBtn.textContent = '⏳ Restoring…';

    apiFetch('/api/projects/' + encodeURIComponent(projectId) + '/source', {
      method: 'PUT',
      body: JSON.stringify({ source: historyPreviewSource }),
    })
      .then(function (r) {
        wsHistoryRestoreBtn.disabled = false;
        wsHistoryRestoreBtn.textContent = '↩ Restore this version';
        if (r.ok || r.status === 204) {
          // Update the editor with the restored source
          wsSourceEditor.value = historyPreviewSource;
          savedSource = historyPreviewSource;
          closeHistoryPanel();
          showWsToast('✓ Version restored');
          setStatus('✓ Version restored', 'success');
          clearStatusAfter(3000);
        } else {
          return r.json().catch(function () { return {}; }).then(function (d) {
            setStatus('Restore failed: ' + ((d && d.error) || ('HTTP ' + r.status)), 'error');
          });
        }
      })
      .catch(function (err) {
        wsHistoryRestoreBtn.disabled = false;
        wsHistoryRestoreBtn.textContent = '↩ Restore this version';
        setStatus('Restore failed: ' + err.message, 'error');
      });
  }

  function showWsToast(msg) {
    if (!wsToastEl) return;
    wsToastEl.textContent = msg;
    wsToastEl.classList.remove('ws-toast-hide');
    wsToastEl.style.display = '';
    setTimeout(function () {
      wsToastEl.classList.add('ws-toast-hide');
      setTimeout(function () { wsToastEl.style.display = 'none'; }, 320);
    }, 2500);
  }

  // Relative time helper (local to history panel — avoids dep on global formatDate)
  function histRelativeTime(isoStr) {
    if (!isoStr) return '';
    var diff = Date.now() - new Date(isoStr).getTime();
    var sec  = Math.floor(diff / 1000);
    if (sec < 60)  return 'just now';
    var min = Math.floor(sec / 60);
    if (min < 60)  return min + ' min ago';
    var hr = Math.floor(min / 60);
    if (hr < 24)   return hr + ' hr ago';
    var days = Math.floor(hr / 24);
    return days + ' day' + (days !== 1 ? 's' : '') + ' ago';
  }

  // ── Launch ────────────────────────────────────────────────────────────────────
  init();
}());
