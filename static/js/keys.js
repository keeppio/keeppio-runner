// Keyboard shortcuts.
//
//   ⌘K / Ctrl+K       open the command palette (resources + actions)
//   g h               navigate to env root  (/r/)
//   g t               navigate to tasks     (/tasks)
//   g s               navigate to settings  (/settings)
//   g c               toggle bottom console
//   r                 replay the task currently open in the bottom console
//   ?                 open this help overlay
//   Esc               close any open overlay (handled in _layout.html)
//
// Chord shortcuts (g + letter) wait up to 1.2 s for the second key. The
// first press flashes a small hint near the top-right so the user knows
// the chord is armed — Linear, Vercel, etc. all do this and operators
// pick it up quickly.

(function() {
  'use strict';

  var d = document;

  // Don't grab keystrokes while the user is typing in a field.
  function inEditable(t) {
    if (!t) return false;
    var tag = t.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
    if (t.isContentEditable) return true;
    return false;
  }

  // ---------- Chord prefix ----------
  var prefix = null, prefixTimer = null;
  function flashHint(text) {
    var hint = d.getElementById('shortcut-hint');
    if (!hint) {
      hint = d.createElement('div');
      hint.id = 'shortcut-hint';
      hint.style.cssText = 'position:fixed;top:54px;right:14px;background:var(--color-surface-3);color:var(--color-ink-1);border:1px solid var(--color-line);border-radius:6px;padding:4px 10px;font-family:var(--font-mono);font-size:12px;box-shadow:var(--shadow-2);z-index:60;transition:opacity 120ms';
      d.body.appendChild(hint);
    }
    hint.textContent = text;
    hint.style.opacity = '1';
  }
  function clearHint() {
    var hint = d.getElementById('shortcut-hint');
    if (hint) hint.style.opacity = '0';
  }
  function armPrefix(p) {
    prefix = p;
    flashHint(p + ' …');
    if (prefixTimer) clearTimeout(prefixTimer);
    prefixTimer = setTimeout(function() { prefix = null; clearHint(); }, 1200);
  }
  function consumePrefix() {
    prefix = null;
    if (prefixTimer) clearTimeout(prefixTimer);
    clearHint();
  }

  // ---------- Replay shortcut ----------
  function replayCurrentTask() {
    // The bottom console writes the active task id to localStorage.
    var id = null;
    try { id = parseInt(localStorage.getItem('console-active'), 10); } catch (_) {}
    if (!id) return false;
    if (!confirm('Replay task #' + id + ' with the same args?')) return true;
    var f = d.createElement('form');
    f.method = 'POST'; f.action = '/tasks/replay/' + id;
    d.body.appendChild(f); f.submit();
    return true;
  }

  // ---------- Help overlay ----------
  function openHelp() {
    var modalRoot = d.getElementById('modal-root');
    var modalContent = d.getElementById('modal-content');
    if (!modalRoot || !modalContent) return;
    modalContent.innerHTML =
      '<div class="modal-header"><strong style="font-size:14px">Keyboard shortcuts</strong>' +
        '<button class="btn btn-ghost btn-icon btn-sm" type="button" data-action="close-modal" aria-label="Close">' +
          '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M18 6 6 18M6 6l12 12"/></svg>' +
        '</button></div>' +
      '<div class="modal-body">' +
        '<dl class="kv">' +
          '<dt><kbd>⌘K</kbd> / <kbd>Ctrl+K</kbd></dt><dd>Search resources + actions</dd>' +
          '<dt><kbd>g</kbd> <kbd>h</kbd></dt><dd>Go to environment root</dd>' +
          '<dt><kbd>g</kbd> <kbd>t</kbd></dt><dd>Go to tasks</dd>' +
          '<dt><kbd>g</kbd> <kbd>s</kbd></dt><dd>Go to settings</dd>' +
          '<dt><kbd>g</kbd> <kbd>c</kbd></dt><dd>Toggle bottom console</dd>' +
          '<dt><kbd>r</kbd></dt><dd>Replay the task open in the console</dd>' +
          '<dt><kbd>?</kbd></dt><dd>Show this help</dd>' +
          '<dt><kbd>Esc</kbd></dt><dd>Close modal / drawer / mobile console</dd>' +
        '</dl>' +
      '</div>';
    modalRoot.style.display = 'flex';
    d.body.dataset.modal = 'open';
  }

  // ---------- Command palette ----------
  var paletteOpen = false;
  var paletteState = { items: [], filtered: [], idx: 0, q: '' };

  function buildResourceItems() {
    var items = [];
    d.querySelectorAll('#resource-tree .tree-node').forEach(function(node) {
      var label = (node.querySelector(':scope > .tree-row > .tree-label') || {}).textContent || '';
      var sub   = (node.querySelector(':scope > .tree-row > .tree-meta')  || {}).textContent || '';
      var type  = node.getAttribute('data-type') || '';
      var id    = node.getAttribute('data-id')   || '';
      var href  = node.querySelector(':scope > .tree-row > a.tree-label');
      if (!label || !href) return;
      items.push({
        kind: 'resource',
        type: type,
        label: label.trim(),
        sub:   sub.trim(),
        href:  href.getAttribute('href'),
        id:    id,
      });
    });
    return items;
  }

  function loadCatalog() {
    return fetch('/ui/catalog', { headers: { Accept: 'application/json' } })
      .then(function(r) { return r.ok ? r.json() : { actions: [] }; })
      .catch(function() { return { actions: [] }; });
  }

  function ensurePaletteData() {
    paletteState.items = buildResourceItems();
    return loadCatalog().then(function(d) {
      (d.actions || []).forEach(function(a) {
        paletteState.items.push({
          kind: 'action',
          type: 'action',
          label: a.label,
          sub:   a.group + (a.severity === 'danger' ? ' · danger' : ''),
          href:  '/run/' + a.id,
          id:    a.id,
          severity: a.severity,
        });
      });
    });
  }

  function fuzzy(q, label) {
    // Cheap subsequence match: every char of q must appear in label in order.
    if (!q) return 1;
    q = q.toLowerCase(); var l = label.toLowerCase(), i = 0;
    for (var c = 0; c < l.length && i < q.length; c++) if (l[c] === q[i]) i++;
    return i === q.length ? (q.length / l.length) : 0;
  }

  function renderPalette() {
    var list = d.getElementById('palette-list');
    if (!list) return;
    var q = paletteState.q;
    var matches = [];
    paletteState.items.forEach(function(it) {
      var s = fuzzy(q, it.label);
      if (s > 0) matches.push({ it: it, score: s });
    });
    matches.sort(function(a, b) { return b.score - a.score; });
    paletteState.filtered = matches.slice(0, 30).map(function(m) { return m.it; });
    if (paletteState.idx >= paletteState.filtered.length) paletteState.idx = 0;
    if (!paletteState.filtered.length) {
      list.innerHTML = '<div class="muted" style="padding:14px;font-size:12px">No matches</div>';
      return;
    }
    list.innerHTML = paletteState.filtered.map(function(it, i) {
      var icon = it.kind === 'action'
        ? (it.severity === 'danger' ? '<span class="dot dot-bad"></span>' : '<span class="pill">action</span>')
        : '<span class="pill">' + it.type.replace('host-', '') + '</span>';
      return '<div class="palette-item"' + (i === paletteState.idx ? ' aria-selected="true"' : '') +
        ' data-idx="' + i + '" style="display:flex;align-items:center;gap:8px;padding:6px 12px;cursor:pointer;font-size:13px;border-left:2px solid transparent">' +
        icon +
        '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + escapeHTML(it.label) + '</span>' +
        '<span class="muted" style="font-size:11px">' + escapeHTML(it.sub) + '</span>' +
        '</div>';
    }).join('');
    syncPaletteHighlight();
  }
  function syncPaletteHighlight() {
    d.querySelectorAll('.palette-item').forEach(function(el, i) {
      if (i === paletteState.idx) {
        el.style.background = 'var(--color-accent-soft)';
        el.style.borderLeftColor = 'var(--color-accent)';
        el.setAttribute('aria-selected', 'true');
        el.scrollIntoView({ block: 'nearest' });
      } else {
        el.style.background = '';
        el.style.borderLeftColor = 'transparent';
        el.removeAttribute('aria-selected');
      }
    });
  }
  function escapeHTML(s) { return String(s == null ? '' : s).replace(/[<>&"]/g, function(c){ return {'<':'&lt;','>':'&gt;','&':'&amp;','"':'&quot;'}[c]; }); }

  function selectPaletteItem() {
    var it = paletteState.filtered[paletteState.idx];
    if (!it) return;
    closePalette();
    if (it.kind === 'action') {
      // Open the run-modal directly so the operator stays in flow.
      if (window.htmx) {
        htmx.ajax('GET', it.href + '?modal=1', { target: '#modal-content', swap: 'innerHTML' });
      } else {
        window.location.href = it.href;
      }
    } else {
      window.location.href = it.href;
    }
  }

  function openPalette() {
    paletteOpen = true;
    paletteState.q = ''; paletteState.idx = 0;
    var modalRoot = d.getElementById('modal-root');
    var modalContent = d.getElementById('modal-content');
    if (!modalRoot || !modalContent) return;
    modalContent.innerHTML =
      '<div class="modal-header" style="padding:0;border-bottom:1px solid var(--color-line)">' +
        '<input id="palette-input" type="text" autocomplete="off" placeholder="Jump to resource or run an action…" ' +
          'style="border:none;border-radius:0;background:transparent;height:44px;padding:0 16px;font-size:14px" class="raw">' +
      '</div>' +
      '<div class="modal-body" style="padding:0;max-height:60vh;overflow:auto">' +
        '<div id="palette-list"></div>' +
      '</div>' +
      '<div class="modal-footer" style="padding:8px 12px;justify-content:space-between">' +
        '<span class="muted" style="font-size:11px"><kbd>↑↓</kbd> navigate · <kbd>↵</kbd> open · <kbd>Esc</kbd> close</span>' +
      '</div>';
    modalRoot.style.display = 'flex';
    d.body.dataset.modal = 'open';
    var input = d.getElementById('palette-input');
    if (input) {
      input.style.cssText = 'border:none;border-radius:0;background:transparent;height:44px;padding:0 16px;font-size:14px;width:100%;color:var(--color-ink-1);outline:none';
      input.focus();
      input.addEventListener('input', function() {
        paletteState.q = input.value; paletteState.idx = 0; renderPalette();
      });
      input.addEventListener('keydown', function(e) {
        if (e.key === 'ArrowDown') { paletteState.idx = Math.min(paletteState.idx + 1, paletteState.filtered.length - 1); syncPaletteHighlight(); e.preventDefault(); }
        else if (e.key === 'ArrowUp') { paletteState.idx = Math.max(paletteState.idx - 1, 0); syncPaletteHighlight(); e.preventDefault(); }
        else if (e.key === 'Enter') { selectPaletteItem(); e.preventDefault(); }
      });
    }
    ensurePaletteData().then(renderPalette);
  }
  function closePalette() {
    paletteOpen = false;
    var modalRoot = d.getElementById('modal-root');
    if (modalRoot) modalRoot.style.display = 'none';
    d.body.removeAttribute('data-modal');
    var modalContent = d.getElementById('modal-content');
    if (modalContent) modalContent.innerHTML = '';
  }

  // Click-to-select inside the palette.
  d.addEventListener('click', function(e) {
    var item = e.target.closest('.palette-item');
    if (!item) return;
    paletteState.idx = parseInt(item.getAttribute('data-idx'), 10) || 0;
    selectPaletteItem();
  });

  // ---------- Master keydown handler ----------
  d.addEventListener('keydown', function(e) {
    // Modifiers: only ⌘K / Ctrl+K bypass the editable check.
    var k = e.key.toLowerCase();
    if ((e.metaKey || e.ctrlKey) && k === 'k') {
      e.preventDefault();
      if (paletteOpen) closePalette(); else openPalette();
      return;
    }
    if (paletteOpen) return; // palette has its own input handler
    if (inEditable(e.target)) return;
    // Help overlay.
    if (k === '?' || (e.shiftKey && k === '/')) {
      e.preventDefault(); openHelp(); return;
    }
    // Replay: only when a non-running task is selected in the console.
    if (k === 'r' && !e.metaKey && !e.ctrlKey && !e.altKey) {
      if (replayCurrentTask()) e.preventDefault();
      return;
    }
    // Chord prefix.
    if (prefix === 'g') {
      consumePrefix();
      switch (k) {
        case 'h': window.location.href = '/r/'; e.preventDefault(); return;
        case 't': window.location.href = '/tasks'; e.preventDefault(); return;
        case 's': window.location.href = '/settings'; e.preventDefault(); return;
        case 'c': {
          var c = d.getElementById('console');
          if (c) {
            var cur = c.getAttribute('data-collapsed') === 'true';
            c.setAttribute('data-collapsed', cur ? 'false' : 'true');
            try { localStorage.setItem('console-collapsed', cur ? 'false' : 'true'); } catch (_) {}
          }
          e.preventDefault();
          return;
        }
      }
      return;
    }
    if (k === 'g' && !e.metaKey && !e.ctrlKey && !e.altKey) {
      armPrefix('g'); e.preventDefault(); return;
    }
  });

  // Replace the simple "open-cmdk" placeholder hint in _layout.html with
  // the real palette opener — that button now triggers this handler.
  d.addEventListener('click', function(e) {
    if (e.target.closest('[data-action="open-cmdk"]')) {
      e.preventDefault();
      if (paletteOpen) closePalette(); else openPalette();
    }
  });

  // The layout's closeModal() runs for Esc / submit / backdrop-click /
  // explicit close buttons. When that happens while the palette is up,
  // keep our local paletteOpen flag in sync -- otherwise the next ⌘K
  // press would think the palette is still open and close-without-open.
  d.addEventListener('keeppio:modal-closed', function() { paletteOpen = false; });
})();
