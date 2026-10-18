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

  // <div data-sse="/url" data-sse-state="#el">: the server re-renders the
  // block and sends it as a "live" event; the browser reconnects on its own.
  const bars = (root) => root.querySelectorAll('[data-pct]').forEach((el) => { el.style.width = `${el.dataset.pct}%`; });
  bars(document);
  document.querySelectorAll('[data-sse]').forEach((el) => {
    const state = document.querySelector(el.dataset.sseState);
    const es = new EventSource(el.dataset.sse);
    // While a control in the block has focus (an open stream picker), hold
    // updates so the re-render does not reset it; apply the latest on blur.
    let pending = null;
    const busy = () => el.contains(document.activeElement) && document.activeElement.matches('select');
    const apply = (html) => { el.innerHTML = html; bars(el); };
    el.addEventListener('focusout', () => setTimeout(() => {
      if (pending !== null && !busy()) { apply(pending); pending = null; }
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
    // [data-check-all="name"] toggles every checkbox called name in its form.
    if (t.matches('[data-check-all]')) {
      // Rows hidden by a [data-filter] box are left alone.
      t.form.querySelectorAll(`input[type=checkbox][name="${t.dataset.checkAll}"]`).forEach((c) => { if (!c.closest('[hidden]')) c.checked = t.checked; });
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
})();
