// MMP admin UI behaviour. No inline handlers (the CSP forbids them):
// everything hangs off data- attributes.
(() => {
  'use strict';

  // Drag-and-drop reordering of [data-sortable] lists. On drop, the
  // enclosing [data-sortable-form] is submitted (htmx), carrying the
  // rows' hidden inputs in their new order.
  let dragged = null;
  let before = '';
  const order = (list) => [...list.children].map((li) => li.querySelector('input')?.value).join(',');

  document.addEventListener('dragstart', (e) => {
    const li = e.target.closest?.('[data-sortable] > li');
    if (!li) return;
    dragged = li;
    before = order(li.parentNode);
    li.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', '');
  });
  document.addEventListener('dragover', (e) => {
    if (!dragged) return;
    const li = e.target.closest?.('[data-sortable] > li');
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
    if (order(li.parentNode) !== before) li.closest('[data-sortable-form]')?.requestSubmit();
  });

  document.addEventListener('change', (e) => {
    const t = e.target;
    // [data-check-all="name"] toggles every checkbox called name in its form.
    if (t.matches('[data-check-all]')) {
      t.form.querySelectorAll(`input[type=checkbox][name="${t.dataset.checkAll}"]`).forEach((c) => { c.checked = t.checked; });
    }
    if (t.matches('[data-autosubmit]')) t.form.requestSubmit();
  });
})();
