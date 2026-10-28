// MMP admin UI behaviour. No inline handlers (the CSP forbids them):
// everything hangs off data- attributes.
(() => {
  'use strict';

  // Drag-and-drop reordering of [data-sortable] lists (list items or table
  // rows). On drop, the enclosing [data-sortable-form] is submitted (htmx),
  // carrying the rows' inputs in their new order; with
  // data-sortable-submit="#form" that form is filled with the rows' first
  // inputs (name and value) and submitted instead. [data-move=up|down]
  // buttons move their row one place, for touch screens.
  const rowSel = '[data-sortable] > li, [data-sortable] > tr';
  let dragged = null;
  let before = '';
  const order = (list) => [...list.children].map((li) => li.querySelector('input')?.value).join(',');
  const submitOrder = (list) => {
    const sel = list.dataset.sortableSubmit;
    if (!sel) { list.closest('[data-sortable-form]')?.requestSubmit(); return; }
    const form = document.querySelector(sel);
    if (!form) return;
    form.querySelectorAll(`input[data-order]`).forEach((i) => i.remove());
    [...list.children].forEach((row) => {
      const src = row.querySelector('input');
      if (!src) return;
      const i = document.createElement('input');
      Object.assign(i, { type: 'hidden', name: src.name, value: src.value });
      i.dataset.order = '';
      form.appendChild(i);
    });
    form.requestSubmit();
  };

  // Table rows drag by their .grip only, so their inputs and text stay
  // usable: grabbing the grip makes the row draggable until the drag ends.
  document.addEventListener('pointerdown', (e) => {
    const grip = e.target.closest?.('[data-sortable] > tr .grip');
    if (grip) grip.closest('tr').draggable = true;
  });
  document.addEventListener('pointerup', () => {
    document.querySelectorAll('[data-sortable] > tr[draggable=true]').forEach((tr) => { if (tr !== dragged) tr.draggable = false; });
  });

  document.addEventListener('click', (e) => {
    const btn = e.target.closest?.('[data-move]');
    if (!btn) return;
    const row = btn.closest(rowSel);
    if (!row) return;
    const sib = btn.dataset.move === 'up' ? row.previousElementSibling : row.nextElementSibling;
    if (!sib) return;
    row.parentNode.insertBefore(row, btn.dataset.move === 'up' ? sib : sib.nextSibling);
    submitOrder(row.parentNode);
  });

  document.addEventListener('dragstart', (e) => {
    const li = e.target.closest?.(rowSel);
    if (!li) return;
    dragged = li;
    before = order(li.parentNode);
    li.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', '');
  });
  document.addEventListener('dragover', (e) => {
    if (!dragged) return;
    const li = e.target.closest?.(rowSel);
    if (!li || li.parentNode !== dragged.parentNode) return;
    e.preventDefault();
    if (li === dragged) return;
    const r = li.getBoundingClientRect();
    li.parentNode.insertBefore(dragged, e.clientY > r.top + r.height / 2 ? li.nextSibling : li);
  });
  document.addEventListener('drop', (e) => { if (dragged) e.preventDefault(); });
  document.addEventListener('dragend', () => {
    if (!dragged) return;
    const li = dragged;
    dragged = null;
    li.classList.remove('dragging');
    if (li.tagName === 'TR') li.draggable = false;
    if (order(li.parentNode) !== before) submitOrder(li.parentNode);
  });

  // [data-count-of="name"] shows how many name checkboxes are ticked.
  const counts = () => document.querySelectorAll('[data-count-of]').forEach((el) => {
    const n = el.closest('form').querySelectorAll(`input[type=checkbox][name="${el.dataset.countOf}"]:checked`).length;
    el.textContent = `${n} ${el.dataset.countLabel || ''}`.trim();
  });
  document.addEventListener('htmx:afterSwap', counts);
  document.addEventListener('input', (e) => {
    const row = e.target.closest?.('[data-row]');
    if (row && e.target.matches('input[type=text], input:not([type])')) { const k = row.querySelector('input[name=key]'); if (k) { k.checked = true; counts(); } }
  });

  // <input data-filter="#list"> hides the list's <li>s whose text does not
  // contain what was typed.
  document.addEventListener('input', (e) => {
    const box = e.target.closest?.('[data-filter]');
    if (!box) return;
    const q = box.value.trim().toLowerCase();
    document.querySelectorAll(`${box.dataset.filter} > li`).forEach((li) => { li.hidden = q !== '' && !li.textContent.toLowerCase().includes(q); });
  });

  // [data-nav-toggle] opens and closes the sidebar on small screens.
  document.addEventListener('click', (e) => {
    const t = e.target.closest?.('[data-nav-toggle]');
    if (!t) return;
    const open = document.body.classList.toggle('nav-open');
    document.querySelectorAll('button[data-nav-toggle]').forEach((b) => b.setAttribute('aria-expanded', String(open)));
    document.querySelector('.scrim')?.toggleAttribute('hidden', !open);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && document.body.classList.contains('nav-open')) document.querySelector('button[data-nav-toggle]')?.click();
  });

  // <input data-enter="#button"> presses that button on Enter instead of the
  // form's first one.
  document.addEventListener('keydown', (e) => {
    const sel = e.key === 'Enter' && e.target.dataset?.enter;
    if (!sel) return;
    e.preventDefault();
    document.querySelector(sel)?.click();
  });

  // <button data-dialog="#id"> opens that <dialog> as a modal.
  document.addEventListener('click', (e) => {
    const btn = e.target.closest?.('[data-dialog]');
    if (!btn) return;
    document.querySelector(btn.dataset.dialog)?.showModal();
  });

  // <form data-confirm="question"> (or its clicked button) asks before submitting.
  document.addEventListener('submit', (e) => {
    const msg = e.submitter?.dataset?.confirm || e.target.dataset?.confirm;
    if (msg && !window.confirm(msg)) e.preventDefault();
  }, true);

  // <button data-copy="#id"> copies that input's value. Clipboard access
  // needs a secure context (https or localhost); elsewhere the text is
  // selected for a manual copy.
  document.addEventListener('click', async (e) => {
    const btn = e.target.closest?.('[data-copy]');
    if (!btn) return;
    e.preventDefault();
    const field = document.querySelector(btn.dataset.copy);
    if (!field) return;
    const label = btn.textContent;
    try {
      await navigator.clipboard.writeText(field.value);
      btn.textContent = 'Copied';
    } catch {
      field.focus();
      field.select();
      btn.textContent = 'Press Ctrl+C';
    }
    setTimeout(() => { btn.textContent = label; }, 1500);
  });

  // <button data-fill="#input" data-value="v"> puts v in that input and
  // clears the suggestion list it sits in.
  document.addEventListener('click', (e) => {
    const btn = e.target.closest?.('[data-fill]');
    if (!btn) return;
    const field = document.querySelector(btn.dataset.fill);
    if (!field) return;
    field.value = btn.dataset.value;
    btn.closest('ul')?.remove();
    field.focus();
  });

  // <div data-sse="/url" data-sse-state="#el">: the server re-renders the
  // block and sends it as a "live" event; the browser reconnects on its own.
  const bars = (root) => root.querySelectorAll('[data-pct]').forEach((el) => { el.style.width = `${el.dataset.pct}%`; });
  bars(document);
  // [data-left]: blocks placed along a track (the guide grid), in percent.
  const place = (root) => root.querySelectorAll('[data-left]').forEach((el) => { el.style.left = `${el.dataset.left}%`; });
  place(document);
  // <time datetime data-local="clock|when|day">: the server writes times in
  // its own zone; show them in the browser's.
  const pad = (n) => String(n).padStart(2, '0');
  const localFormats = {
    clock: (d) => `${pad(d.getHours())}:${pad(d.getMinutes())}`,
    when: (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${localFormats.clock(d)}`,
    day: (d) => `${d.toLocaleDateString('en', { weekday: 'long' })} ${d.getDate()} ${d.toLocaleDateString('en', { month: 'long' })}`,
  };
  const localize = (root) => root.querySelectorAll('time[data-local]').forEach((el) => {
    const d = new Date(el.dateTime);
    const f = localFormats[el.dataset.local];
    if (f && !Number.isNaN(d.getTime())) el.textContent = f(d);
  });
  localize(document);
  document.addEventListener('htmx:load', (e) => { place(e.target); bars(e.target); localize(e.target); });
  document.querySelectorAll('[data-sse]').forEach((el) => {
    const state = document.querySelector(el.dataset.sseState);
    const es = new EventSource(el.dataset.sse);
    // While a control in the block has focus (an open stream picker), hold
    // updates so the re-render does not reset it; apply the latest on blur.
    // A pressed pointer holds too: swapping the button out between press and
    // release loses the click.
    let pending = null;
    let pressed = false;
    const busy = () => pressed || (el.contains(document.activeElement) && document.activeElement.matches('select'));
    // [data-keep-scroll="key"] boxes keep their scroll position across
    // re-renders, so a table scrolled sideways stays where it was.
    // Bars start at the width they had before the re-render, so only a
    // real change animates instead of every bar regrowing from zero.
    const apply = (html) => {
      const scroll = {};
      el.querySelectorAll('[data-keep-scroll]').forEach((s) => { scroll[s.dataset.keepScroll] = [s.scrollLeft, s.scrollTop]; });
      const widths = Array.from(el.querySelectorAll('[data-pct]'), (b) => b.style.width);
      el.innerHTML = html;
      el.querySelectorAll('[data-keep-scroll]').forEach((s) => {
        const p = scroll[s.dataset.keepScroll];
        if (p) [s.scrollLeft, s.scrollTop] = p;
      });
      const fresh = el.querySelectorAll('[data-pct]');
      fresh.forEach((b, i) => {
        if (!widths[i]) return;
        b.style.transition = 'none';
        b.style.width = widths[i];
      });
      if (fresh.length) void el.offsetWidth; // commit the old widths before animating
      fresh.forEach((b) => { b.style.transition = ''; });
      bars(el);
    };
    const flush = () => { if (pending !== null && !busy()) { apply(pending); pending = null; } };
    el.addEventListener('pointerdown', () => { pressed = true; });
    // Released after the click event has run, so the click lands first.
    const release = () => { if (pressed) { pressed = false; setTimeout(flush, 0); } };
    document.addEventListener('pointerup', release);
    document.addEventListener('pointercancel', release);
    // Focus moving to another control in the block (a button being
    // clicked) keeps holding: re-rendering now would swallow the click.
    el.addEventListener('focusout', (e) => setTimeout(() => {
      if (!el.contains(e.relatedTarget)) flush();
    }, 0));
    es.addEventListener('live', (e) => {
      if (busy()) pending = e.data; else apply(e.data);
      if (state) { state.textContent = '● live'; state.dataset.state = 'live'; }
    });
    es.addEventListener('error', () => {
      if (state) { state.textContent = '○ reconnecting…'; state.dataset.state = 'down'; }
    });
  });

  document.addEventListener('change', (e) => {
    const t = e.target;
    // [data-check-all="name"] toggles every checkbox called name in its
    // form, including ones outside it that join it with form="…".
    if (t.matches('[data-check-all]')) {
      // Rows hidden by a [data-filter] box are left alone.
      Array.from(t.form.elements).forEach((c) => {
        if (c.type === 'checkbox' && c.name === t.dataset.checkAll && !c.closest('[hidden]')) c.checked = t.checked;
      });
    }
    // select[data-reveal] shows the [data-reveal-for] fields of its form
    // that belong to the chosen option, and hides the rest. A select inside
    // [data-reveal-scope] only drives the fields of that scope, and the
    // form's select leaves them alone.
    if (t.matches('select[data-reveal]')) {
      const scope = t.closest('[data-reveal-scope]') || t.form;
      scope.querySelectorAll('[data-reveal-for]').forEach((el) => {
        if ((el.parentElement.closest('[data-reveal-scope]') || t.form) === scope) el.hidden = el.dataset.revealFor !== t.value;
      });
    }
    if (t.matches('[data-autosubmit]')) t.form.requestSubmit();
    // Editing a row of [data-row] (other than its key box) selects the row.
    const row = t.closest('[data-row]');
    if (row && t.name !== 'key') { const k = row.querySelector('input[name=key]'); if (k) k.checked = true; }
    counts();
    // Ticking inside [data-check-on-change="#radio"] also checks that radio.
    const on = t.closest('[data-check-on-change]');
    if (on && t.type === 'checkbox') document.querySelector(on.dataset.checkOnChange).checked = true;
  });

  // Pop-up preview player. A [data-preview] button plays that URL (live
  // MPEG-TS) in #pip with mpegts.js, which is loaded on first use. The
  // window drags by its title bar and resizes from its corner. Closing it
  // (or opening another preview) ends the request, which frees the
  // provider connection on the server at once.
  const pip = document.getElementById('pip');
  if (pip) {
    const video = pip.querySelector('video');
    const msg = pip.querySelector('.pip-msg');
    const title = pip.querySelector('.pip-title');
    let player = null;
    let lib = null;
    const say = (text) => { msg.textContent = text; msg.hidden = !text; };
    const loadLib = () => lib ??= new Promise((ok, fail) => {
      const s = document.createElement('script');
      s.src = pip.dataset.lib;
      s.onload = () => ok(window.mpegts);
      s.onerror = () => { lib = null; fail(new Error('load failed')); };
      document.head.appendChild(s);
    });
    // The server's reason is in the response body, which mpegts.js does
    // not expose: the status code has to do.
    const httpText = {
      404: 'This stream no longer exists.',
      409: 'The provider is disabled.',
      502: 'The provider did not deliver the stream.',
      503: 'Not available right now: every connection to the provider may be in use.',
    };
    const errorText = (mpegts, type, detail, info) => {
      if (type === mpegts.ErrorTypes.MEDIA_ERROR) return 'This browser cannot play the stream’s format (often HEVC/H.265 video).';
      if (detail === mpegts.ErrorDetails.NETWORK_STATUS_CODE_INVALID) return httpText[info?.code] || `The server answered ${info?.code}.`;
      return 'The stream stopped. Press ▶ to try again.';
    };
    const stop = () => {
      if (player) {
        const p = player;
        player = null;
        p.pause(); p.unload(); p.detachMediaElement(); p.destroy();
      }
      say('');
    };
    const close = () => { stop(); pip.hidden = true; };
    const open = async (url, name) => {
      stop();
      title.textContent = name || 'Preview';
      pip.hidden = false;
      say('Connecting…');
      let mpegts;
      try { mpegts = await loadLib(); } catch { say('The player could not be loaded.'); return; }
      if (!mpegts.isSupported()) { say('This browser cannot play live streams here (no Media Source Extensions).'); return; }
      const p = mpegts.createPlayer({ type: 'mpegts', isLive: true, url },
        { enableWorker: false, lazyLoad: false, liveBufferLatencyChasing: true });
      player = p;
      p.on(mpegts.Events.ERROR, (type, detail, info) => { if (player === p) say(errorText(mpegts, type, detail, info)); });
      p.on(mpegts.Events.LOADING_COMPLETE, () => { if (player === p) say('The stream ended. Press ▶ to watch again.'); });
      p.attachMediaElement(video);
      p.load();
      video.muted = false;
      // The click allows sound; should the browser still refuse, play muted.
      Promise.resolve(p.play()).catch(() => { if (player === p) { video.muted = true; Promise.resolve(p.play()).catch(() => {}); } });
    };
    video.addEventListener('playing', () => { if (player) say(''); });

    document.addEventListener('click', (e) => {
      const btn = e.target.closest?.('[data-preview]');
      if (btn) { e.preventDefault(); open(btn.dataset.preview, btn.dataset.previewTitle); return; }
      if (e.target.closest?.('[data-pip-close]')) close();
    });
    document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !pip.hidden) close(); });
    window.addEventListener('pagehide', stop);

    // Dragging, kept inside the window.
    const bar = pip.querySelector('[data-pip-drag]');
    bar.addEventListener('pointerdown', (e) => {
      if (e.button !== 0 || e.target.closest('button')) return;
      const r = pip.getBoundingClientRect();
      const dx = e.clientX - r.left, dy = e.clientY - r.top;
      bar.setPointerCapture(e.pointerId);
      const move = (ev) => {
        pip.style.left = `${Math.min(Math.max(0, ev.clientX - dx), window.innerWidth - r.width)}px`;
        pip.style.top = `${Math.min(Math.max(0, ev.clientY - dy), window.innerHeight - r.height)}px`;
        pip.style.right = 'auto';
        pip.style.bottom = 'auto';
      };
      const up = () => { bar.removeEventListener('pointermove', move); bar.removeEventListener('pointerup', up); };
      bar.addEventListener('pointermove', move);
      bar.addEventListener('pointerup', up);
    });
  }
})();
