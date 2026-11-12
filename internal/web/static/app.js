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

  // [data-count-of="name"] shows how many name checkboxes of its form are
  // ticked, including ones outside it that join it with form="…".
  const counts = () => document.querySelectorAll('[data-count-of]').forEach((el) => {
    const n = Array.from(el.closest('form').elements).filter((c) => c.type === 'checkbox' && c.name === el.dataset.countOf && c.checked).length;
    el.textContent = `${n} ${el.dataset.countLabel || ''}`.trim();
  });
  document.addEventListener('htmx:afterSwap', counts);

  // details.menu: one open at a time; a click outside or Escape closes it.
  const closeMenus = (except) => document.querySelectorAll('details.menu[open]').forEach((m) => { if (m !== except) m.open = false; });
  document.addEventListener('click', (e) => closeMenus(e.target.closest?.('details.menu')));
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    const open = document.querySelector('details.menu[open]');
    if (open) { open.open = false; open.querySelector('summary').focus(); }
  });
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

  // nav[data-spy]: its #links mark (aria-current) the section being read.
  document.querySelectorAll('nav[data-spy]').forEach((nav) => {
    const links = [...nav.querySelectorAll('a[href^="#"]')];
    const sections = links.map((a) => document.getElementById(a.hash.slice(1))).filter(Boolean);
    const mark = () => {
      // A section jumped to sits a little below the index (scroll-margin-top).
      const top = nav.getBoundingClientRect().bottom + 32;
      let cur = sections[0];
      sections.forEach((s) => { if (s.getBoundingClientRect().top <= top) cur = s; });
      if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2) cur = sections[sections.length - 1];
      links.forEach((a) => {
        if (a.hash !== `#${cur?.id}`) { a.removeAttribute('aria-current'); return; }
        if (a.hasAttribute('aria-current')) return;
        a.setAttribute('aria-current', 'true');
        // On phones the index scrolls sideways: keep the marked link in it.
        nav.scrollTo({ left: a.offsetLeft - (nav.clientWidth - a.offsetWidth) / 2, behavior: 'smooth' });
      });
    };
    mark();
    window.addEventListener('scroll', mark, { passive: true });
  });
  // [data-scroll-here]: a message shown in the middle of a long page (the
  // section just saved) is scrolled to when the page opens without #anchor.
  if (!window.location.hash) document.querySelector('[data-scroll-here]')?.closest('section')?.scrollIntoView();

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

  // Confirmations use the #confirm dialog instead of the browser's
  // confirm(). <form data-confirm="question"> (or its clicked button) asks
  // before submitting; hx-confirm asks before htmx sends its request. The
  // confirm button takes the label of the button that was pressed, and is
  // red when that button (or its form) is a danger one.
  const confirmDlg = document.getElementById('confirm');
  let confirmed = null; // the form whose next submit was already confirmed
  const ask = (msg, from, then) => {
    if (!confirmDlg) { if (window.confirm(msg)) then(); return; }
    const ok = confirmDlg.querySelector('[data-confirm-ok]');
    const btn = from?.matches?.('button, input[type=submit]') ? from : null;
    const label = btn ? (btn.textContent || btn.value || '').trim() || btn.getAttribute('aria-label') || '' : '';
    ok.textContent = label && label.length <= 40 ? label : 'OK';
    ok.classList.toggle('danger', !!from?.closest('.danger, form.danger') || !!from?.querySelector?.('.danger'));
    confirmDlg.querySelector('.confirm-msg').textContent = msg;
    confirmDlg.returnValue = '';
    confirmDlg.addEventListener('close', () => { if (confirmDlg.returnValue === 'ok') then(); }, { once: true });
    confirmDlg.showModal();
  };
  document.addEventListener('submit', (e) => {
    const form = e.target;
    if (confirmed === form) { confirmed = null; return; }
    const msg = e.submitter?.dataset?.confirm || form.dataset?.confirm;
    if (!msg) return;
    e.preventDefault();
    e.stopImmediatePropagation();
    const by = e.submitter;
    ask(msg, by || form, () => {
      confirmed = form;
      if (by && by.form === form) form.requestSubmit(by); else form.requestSubmit();
    });
  }, true);
  document.addEventListener('htmx:confirm', (e) => {
    const q = e.detail.question;
    if (!q) return;
    e.preventDefault();
    const elt = e.detail.elt;
    ask(q, elt.querySelector?.('button[type=submit], button:not([type])') || elt, () => e.detail.issueRequest(true));
  });

  // Result messages (.flash) get a close button, and the ?msg=… that
  // asked for them leaves the address bar so a reload does not repeat
  // them. [data-keep] flashes describe a state and stay.
  const dismissible = (root) => root.querySelectorAll?.('.flash:not([data-keep]):not(:has(> .dismiss))').forEach((el) => {
    const text = document.createElement('span');
    text.append(...el.childNodes);
    const btn = document.createElement('button');
    Object.assign(btn, { type: 'button', className: 'dismiss', title: 'Dismiss' });
    btn.setAttribute('aria-label', 'Dismiss');
    btn.innerHTML = '<svg aria-hidden="true"><use href="#i-x"/></svg>';
    el.append(text, btn);
  });
  dismissible(document);
  document.addEventListener('htmx:load', (e) => dismissible(e.target));
  document.addEventListener('click', (e) => { if (e.target.closest?.('.flash > .dismiss')) e.target.closest('.flash').remove(); });
  {
    const url = new URL(window.location.href);
    if (url.searchParams.has('msg') || url.searchParams.has('vod_deleted')) {
      ['msg', 'n', 'of', 'vod_deleted'].forEach((k) => url.searchParams.delete(k));
      history.replaceState(history.state, '', url.pathname + url.search + url.hash);
    }
  }

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

  // <select data-narrows="#other">: #other offers only the <optgroup>
  // labelled with this select's value (all of them for an empty value).
  // Picking an option in #other while this is empty sets this to its group.
  const narrow = (sel) => {
    const other = document.querySelector(sel.dataset.narrows);
    if (!other) return;
    other.querySelectorAll('optgroup').forEach((g) => { g.disabled = g.hidden = !!sel.value && g.label !== sel.value; });
    if (other.selectedOptions[0]?.parentElement.disabled) other.value = '';
  };
  document.querySelectorAll('select[data-narrows]').forEach(narrow);
  document.addEventListener('change', (e) => {
    if (e.target.matches?.('select[data-narrows]')) { narrow(e.target); return; }
    const by = e.target.id && document.querySelector(`select[data-narrows="#${CSS.escape(e.target.id)}"]`);
    const group = e.target.selectedOptions?.[0]?.parentElement;
    if (by && !by.value && group?.tagName === 'OPTGROUP') { by.value = group.label; narrow(by); }
  });

  // [data-no-enter]: Enter in this field does not submit its form (a
  // search box inside another form).
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.target.closest?.('[data-no-enter]')) e.preventDefault();
  });

  // Logo picker in the channel form: [data-logo-pick="url"] tiles fill
  // #logo_url (and its [data-logo-preview]); [data-logo-upload] uploads the
  // chosen file and picks it.
  const logoPreview = () => {
    const field = document.getElementById('logo_url');
    const img = document.querySelector('[data-logo-preview]');
    if (!field || !img) return;
    if (field.value) {
      img.src = field.value;
      img.hidden = false;
    } else {
      img.removeAttribute('src');
      img.hidden = true;
    }
    document.querySelectorAll('[data-logo-pick]').forEach((b) => {
      b.classList.toggle('picked', b.dataset.logoPick === field.value);
    });
  };
  document.addEventListener('click', (e) => {
    const btn = e.target.closest?.('[data-logo-pick]');
    const field = document.getElementById('logo_url');
    if (!btn || !field) return;
    field.value = btn.dataset.logoPick;
    logoPreview();
  });
  document.addEventListener('input', (e) => { if (e.target.id === 'logo_url') logoPreview(); });
  document.addEventListener('htmx:afterSwap', (e) => { if (e.target.id === 'logo-hits') logoPreview(); });
  // Groups page icon picker: [data-icon-picker="<form id>"] opens
  // #icon-picker for that group; picking a tile fills the group's logo_url
  // field (and its preview), uploads are picked once saved.
  {
    const dlg = document.getElementById('icon-picker');
    let field = null;
    let groupName = '';
    const hits = dlg?.querySelector('[data-icon-hits]');
    const show = (url) => {
      field.value = url;
      field.dispatchEvent(new Event('input', { bubbles: true }));
    };
    const load = async (params) => {
      hits.textContent = 'Loading…';
      try {
        const resp = await fetch(`/admin/logos/pick?${new URLSearchParams(params)}`, { credentials: 'same-origin' });
        hits.innerHTML = await resp.text();
      } catch {
        hits.textContent = 'Could not load logos; try again.';
      }
    };
    let timer;
    document.addEventListener('click', (e) => {
      const open = e.target.closest?.('[data-icon-picker]');
      if (open && dlg) {
        field = document.querySelector(`input[name="logo_url"][form="${CSS.escape(open.dataset.iconPicker)}"]`);
        groupName = open.dataset.groupName || '';
        dlg.querySelector('[data-icon-picker-name]').textContent = groupName;
        dlg.querySelector('[data-icon-search]').value = '';
        dlg.showModal();
        load({ name: groupName });
        return;
      }
      if (!dlg?.open || !field) return;
      const tile = e.target.closest?.('[data-logo-pick]');
      if (tile && dlg.contains(tile)) {
        show(tile.dataset.logoPick);
        dlg.close();
      } else if (e.target.closest?.('[data-icon-clear]')) {
        show('');
        dlg.close();
      }
    });
    dlg?.querySelector('[data-icon-search]').addEventListener('input', (e) => {
      clearTimeout(timer);
      const q = e.target.value.trim();
      timer = setTimeout(() => load(q ? { q } : { name: groupName }), 300);
    });
    dlg?.querySelector('[data-icon-search]').addEventListener('keydown', (e) => { if (e.key === 'Enter') e.preventDefault(); });
    dlg?.querySelector('[data-icon-upload]').addEventListener('change', async (e) => {
      const input = e.target;
      if (!input.files.length) return;
      const body = new FormData();
      body.append('file', input.files[0]);
      body.append('name', groupName);
      input.value = '';
      hits.textContent = 'Uploading…';
      try {
        const resp = await fetch('/admin/logos/upload?picker=1', { method: 'POST', body, credentials: 'same-origin' });
        hits.innerHTML = await resp.text();
        hits.querySelector('[data-logo-pick]')?.click();
      } catch {
        hits.textContent = 'The upload failed; try again.';
      }
    });
  }

  // Groups page: an icon URL field updates the [data-icon-preview] beside it.
  document.addEventListener('input', (e) => {
    const img = e.target.closest?.('.logo-field')?.querySelector('[data-icon-preview]');
    if (!img) return;
    const v = e.target.value.trim();
    if (/^https?:\/\//.test(v)) {
      img.src = v;
      img.hidden = false;
    } else {
      img.removeAttribute('src');
      img.hidden = true;
    }
  });
  document.addEventListener('change', async (e) => {
    const input = e.target.closest?.('[data-logo-upload]');
    const hits = document.getElementById('logo-hits');
    if (!input || !input.files.length || !hits) return;
    const body = new FormData();
    body.append('file', input.files[0]);
    body.append('name', document.getElementById('channel_name')?.value || '');
    input.value = '';
    hits.textContent = 'Uploading…';
    try {
      const resp = await fetch('/admin/logos/upload?picker=1', { method: 'POST', body, credentials: 'same-origin' });
      hits.innerHTML = await resp.text();
      hits.querySelector('[data-logo-pick]')?.click();
    } catch {
      hits.textContent = 'The upload failed; try again.';
    }
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
    // An open confirmation holds too: its form must still be there to submit.
    const busy = () => pressed || !!confirmDlg?.open || (el.contains(document.activeElement) && document.activeElement.matches('select'));
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
    confirmDlg?.addEventListener('close', () => setTimeout(flush, 0));
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
