// Bottom-console controller.
//
// Drives the recent-tasks tab strip + log viewer + KV args panel that
// lives at the bottom of every page. Communicates with the rest of the
// app via DOM events:
//
//   runner:task-started       fired by _layout.html after a successful
//                             modal submit (HX-Trigger event is
//                             rebroadcast as this name). Consumes the
//                             new task id and switches focus to it.
//
// Server endpoints used:
//   GET  /ui/console/recent?scope=<id>[&show_all=1]
//   GET  /ui/tasks/<id>/log         (text/plain, finished tasks only)
//   WS   /ws/tasks/<id>             (running/queued tasks; LogEvent JSON)
//
// State persistence: the "show_all" toggle and the active task id are
// stored in localStorage so the choice survives page navigation. The
// scope itself is read from <body data-scope="..."> on every load.

(function() {
  'use strict';

  var d = document;
  var POLL_MS = 8000;            // refresh recent-tasks list while open
  var KEEP_ALIVE = 30000;        // browser-side WS heartbeat detection

  var state = {
    scope: '',                   // resource path slug (e.g. "api-01/demo")
    showAll: false,
    tasks: [],                   // most-recent-first
    activeId: null,
    ws: null,                    // active WebSocket
    pollTimer: null,
  };

  function el(id) { return d.getElementById(id); }
  function escapeHTML(s) {
    return String(s == null ? '' : s).replace(/[<>&"]/g, function(c) {
      return { '<': '&lt;', '>': '&gt;', '&': '&amp;', '"': '&quot;' }[c];
    });
  }
  // ANSI strip — playbook output is colour-coded; the log pane stays
  // legible without parsing all the SGR codes.
  function stripAnsi(s) {
    return String(s == null ? '' : s).replace(/\x1b\[[0-9;]*[a-zA-Z]/g, '');
  }
  function fmtElapsed(ms) {
    if (ms < 1000) return ms + 'ms';
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60); s = s % 60;
    if (m < 60) return m + 'm ' + s + 's';
    var h = Math.floor(m / 60); m = m % 60;
    return h + 'h ' + m + 'm';
  }
  function statusDot(status) {
    if (status === 'running') return 'dot-running';
    if (status === 'queued')  return 'dot-info';
    if (status === 'success') return 'dot-ok';
    if (status === 'cancelled') return 'dot-warn';
    if (status === 'error' || status === 'failed') return 'dot-bad';
    return 'dot-idle';
  }

  // ---------- Render ----------
  function renderTabs() {
    var tabs = el('console-tabs');
    if (!tabs) return;
    if (!state.tasks.length) {
      tabs.innerHTML = '<span class="console-tab"><span class="dot dot-idle"></span><span class="muted">No tasks yet</span></span>';
      return;
    }
    var html = state.tasks.map(function(t) {
      var active = (t.id === state.activeId) ? ' aria-current="true"' : '';
      var oos = t.out_of_scope ? ' data-out-of-scope="true"' : '';
      var scope = t.scope ? ' <span class="pill pill-mono" style="height:14px;font-size:10px;padding:0 4px">' + escapeHTML(t.scope) + '</span>' : '';
      return '<span class="console-tab" data-task-id="' + t.id + '"' + active + oos + ' title="' + escapeHTML(t.action_label) + '">' +
        '<span class="dot ' + statusDot(t.status) + '"></span>' +
        '<span style="font-weight:500">' + escapeHTML(t.action_label) + '</span>' +
        scope +
        '<span class="muted" style="font-size:10px;margin-left:4px;font-variant-numeric:tabular-nums">#' + t.id + '</span>' +
        '</span>';
    }).join('');
    tabs.innerHTML = html;
  }

  function renderMeta(t) {
    var summary = el('console-summary');
    var args = el('console-args');
    if (!t) {
      if (summary) summary.textContent = 'select a task';
      if (args) { args.style.display = 'none'; args.innerHTML = ''; }
      removeFollowUp();
      return;
    }
    var elapsed = '';
    var endedAt = t.ended_at || (t.status === 'running' ? Date.now() : null);
    if (t.started_at && endedAt) elapsed = fmtElapsed(endedAt - t.started_at);
    var parts = [];
    parts.push('<span class="dot ' + statusDot(t.status) + '"></span>');
    parts.push('<span style="font-weight:500">' + escapeHTML(t.action_label) + '</span>');
    if (t.scope) parts.push('<span class="pill pill-mono">' + escapeHTML(t.scope) + '</span>');
    parts.push('<span class="muted">·</span>');
    parts.push('<span class="mono muted" style="font-size:11px">#' + t.id + '</span>');
    if (elapsed) {
      parts.push('<span class="muted">·</span>');
      parts.push('<span class="muted tabular">' + escapeHTML(elapsed) + '</span>');
    }
    if (t.username) {
      parts.push('<span class="muted">·</span>');
      parts.push('<span class="muted">' + escapeHTML(t.username) + '</span>');
    }
    if (summary) summary.innerHTML = parts.join(' ');
    renderFollowUp(t);

    // KV args: keep collapsed by default; toggle with the "params" btn.
    if (args) {
      if (t.args && Object.keys(t.args).length) {
        var rows = Object.keys(t.args).sort().map(function(k) {
          var v = t.args[k];
          var isSecret = (v === '<redacted>' || v === '***');
          return '<dt>' + escapeHTML(k) + '</dt>' +
                 '<dd' + (isSecret ? ' style="color:var(--color-ink-3)"' : '') + '>' +
                 escapeHTML(v) + '</dd>';
        }).join('');
        args.innerHTML = '<dl class="kv">' + rows + '</dl>';
      } else {
        args.innerHTML = '<span class="muted">no parameters</span>';
      }
    }
  }

  // ---------- Follow-up prompt (Now deploy? after tenant-version) ----------
  //
  // The `tenant-version` action only pins image tags into git; running
  // containers stay on whatever they had. Operators almost always want
  // to deploy right after pinning, so when the bottom console focuses
  // on a successful tenant-version task we surface a one-click prompt
  // that opens the deploy-current modal pre-filled with the same tenant.
  // No action ID hardcoded elsewhere; this is the one quality-of-life
  // hook we care about.
  function removeFollowUp() {
    var node = el('console-followup');
    if (node && node.parentNode) node.parentNode.removeChild(node);
  }
  function renderFollowUp(t) {
    removeFollowUp();
    if (!t || t.action_id !== 'tenant-version' || t.status !== 'success') return;
    var tenant = (t.args && (t.args.tenant || t.args.target_host || t.args.host)) || t.scope;
    if (!tenant) return;
    var meta = el('console-meta');
    if (!meta) return;
    var node = d.createElement('div');
    node.id = 'console-followup';
    node.style.cssText = 'display:flex;align-items:center;gap:8px;padding:6px 12px;background:var(--color-info-soft);border-bottom:1px solid var(--color-line);font-size:12px';
    node.innerHTML =
      '<span class="dot dot-info"></span>' +
      '<span>Versions pinned for <span class="mono">' + escapeHTML(tenant) + '</span>. Deploy now?</span>' +
      '<span style="margin-left:auto;display:flex;gap:6px">' +
        '<button class="btn btn-sm btn-primary" type="button" data-followup-deploy="' + escapeHTML(tenant) + '">Deploy current versions</button>' +
        '<button class="btn btn-sm btn-ghost" type="button" data-followup-dismiss aria-label="Dismiss">Later</button>' +
      '</span>';
    meta.parentNode.insertBefore(node, meta.nextSibling);
  }

  function renderScopeLabel() {
    var lbl = el('console-scope-label');
    var btn = el('console-scope-btn');
    if (!lbl) return;
    if (state.showAll || !state.scope) {
      lbl.textContent = 'all';
      lbl.className = 'pill';
      if (btn) btn.title = 'Toggle scope filter (currently showing all)';
    } else {
      lbl.textContent = state.scope;
      lbl.className = 'pill pill-mono';
      if (btn) btn.title = 'Toggle scope filter (currently scoped to ' + state.scope + ')';
    }
  }

  // ---------- Network ----------
  function refreshTasks(activate) {
    var url = '/ui/console/recent';
    var qs = [];
    if (state.showAll) qs.push('show_all=1');
    else if (state.scope) qs.push('scope=' + encodeURIComponent(state.scope));
    if (qs.length) url += '?' + qs.join('&');
    return fetch(url, { headers: { Accept: 'application/json' } })
      .then(function(r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function(data) {
        state.tasks = data.tasks || [];
        renderTabs();
        if (activate || state.activeId == null) {
          // Default selection: prefer a running task, then most recent.
          var pick = state.tasks.find(function(t) { return t.status === 'running'; }) || state.tasks[0];
          if (pick) selectTask(pick.id);
          else { renderMeta(null); el('console-log').textContent = 'No tasks to display.'; }
        } else {
          // Refresh the meta in case status changed.
          var cur = state.tasks.find(function(t) { return t.id === state.activeId; });
          if (cur) renderMeta(cur);
        }
      })
      .catch(function(err) {
        var t = el('console-tabs');
        if (t) t.innerHTML = '<span class="console-tab"><span class="dot dot-bad"></span><span class="muted">Failed to load: ' + err + '</span></span>';
      });
  }

  function selectTask(id) {
    if (state.activeId === id) return;
    state.activeId = id;
    try { localStorage.setItem('console-active', String(id)); } catch (_) {}
    var t = state.tasks.find(function(x) { return x.id === id; });
    renderTabs();
    renderMeta(t || null);
    var log = el('console-log');
    if (!t) { if (log) log.textContent = ''; return; }
    closeWS();
    log.textContent = '';
    if (t.status === 'running' || t.status === 'queued') {
      openWS(id);
    } else {
      fetch('/ui/tasks/' + id + '/log', { headers: { Accept: 'text/plain' } })
        .then(function(r) { return r.text(); })
        .then(function(txt) {
          log.textContent = stripAnsi(txt) || '(empty log)';
          log.scrollTop = log.scrollHeight;
        })
        .catch(function() { log.textContent = '(log fetch failed)'; });
    }
  }

  function openWS(id) {
    var log = el('console-log');
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    var ws = new WebSocket(proto + '//' + location.host + '/ws/tasks/' + id);
    state.ws = ws;
    ws.onmessage = function(ev) {
      try {
        var msg = JSON.parse(ev.data);
        if (msg.line) {
          log.textContent += stripAnsi(msg.line) + '\n';
          // Auto-tail unless user has scrolled up.
          var atBottom = (log.scrollHeight - log.scrollTop - log.clientHeight) < 40;
          if (atBottom) log.scrollTop = log.scrollHeight;
        } else if (msg.status) {
          // Status transition — refresh tabs so the dot updates.
          refreshTasks(false);
          if (msg.status !== 'running' && msg.status !== 'queued') {
            // Task ended; close WS, status is now in the row.
            try { ws.close(); } catch (_) {}
          }
        }
      } catch (_) {}
    };
    ws.onclose = function() { state.ws = null; };
    ws.onerror = function() { try { ws.close(); } catch (_) {} };
  }

  function closeWS() {
    if (state.ws) {
      try { state.ws.close(); } catch (_) {}
      state.ws = null;
    }
  }

  // ---------- Wiring ----------
  function init() {
    state.scope = d.body.getAttribute('data-scope') || '';
    try { state.showAll = localStorage.getItem('console-show-all') === '1'; } catch (_) {}
    renderScopeLabel();

    refreshTasks(true);

    // Switch tabs.
    d.addEventListener('click', function(e) {
      var tab = e.target.closest('.console-tab[data-task-id]');
      if (tab) { selectTask(parseInt(tab.getAttribute('data-task-id'), 10)); return; }
      if (e.target.closest('[data-action="toggle-console-scope"]')) {
        state.showAll = !state.showAll;
        try { localStorage.setItem('console-show-all', state.showAll ? '1' : '0'); } catch (_) {}
        renderScopeLabel();
        refreshTasks(true);
        return;
      }
      if (e.target.closest('#console-args-toggle')) {
        var args = el('console-args');
        if (!args) return;
        args.style.display = (args.style.display === 'none' ? 'block' : 'none');
        return;
      }
      var deploy = e.target.closest('[data-followup-deploy]');
      if (deploy) {
        var tenant = deploy.getAttribute('data-followup-deploy');
        if (window.htmx) {
          htmx.ajax('GET', '/run/deploy-current?modal=1&tenant=' + encodeURIComponent(tenant),
            { target: '#modal-content', swap: 'innerHTML' });
        } else {
          window.location.href = '/run/deploy-current?tenant=' + encodeURIComponent(tenant);
        }
        return;
      }
      if (e.target.closest('[data-followup-dismiss]')) {
        removeFollowUp();
        return;
      }
    });

    // Light up when a modal submit succeeds.
    d.addEventListener('runner:task-started', function(e) {
      var detail = e.detail || {};
      // Pull the fresh list — the new row is now in the DB.
      refreshTasks(false).then(function() {
        if (detail.task_id) selectTask(detail.task_id);
      });
    });

    // Polling for status changes while open. Lightweight; pauses when
    // tab is hidden so background tabs don't burn cycles.
    function tick() {
      if (state.pollTimer) clearTimeout(state.pollTimer);
      state.pollTimer = setTimeout(function() {
        if (!d.hidden) refreshTasks(false);
        tick();
      }, POLL_MS);
    }
    tick();
  }

  if (d.readyState === 'loading') d.addEventListener('DOMContentLoaded', init);
  else init();
})();
