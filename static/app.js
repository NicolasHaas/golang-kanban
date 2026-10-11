// All behaviour is attached here by event delegation, so the pages carry no
// inline script and the Content-Security-Policy can forbid it.
(function () {
  'use strict';

  // --- Messages: a line at the bottom of the screen instead of alert(),
  // optionally with one action such as Undo.

  var toastBox = document.createElement('div');
  toastBox.className = 'toast';
  toastBox.setAttribute('role', 'status');
  toastBox.setAttribute('aria-live', 'polite');
  document.body.appendChild(toastBox);
  var toastTimer = null;

  function toast(message, actionLabel, action) {
    toastBox.replaceChildren();
    var text = document.createElement('span');
    text.textContent = message;
    toastBox.appendChild(text);
    if (actionLabel) {
      var button = document.createElement('button');
      button.type = 'button';
      button.textContent = actionLabel;
      button.addEventListener('click', function () { hideToast(); action(); });
      toastBox.appendChild(button);
    }
    toastBox.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(hideToast, actionLabel ? 8000 : 5000);
  }

  function hideToast() { toastBox.classList.remove('show'); }

  // Something the page cannot recover from by itself: say so, then show what
  // the server has.
  function failAndReload(message) {
    toast(message + ' Reloading the board…');
    setTimeout(function () { location.reload(); }, 1500);
  }

  function post(url, params) {
    return fetch(url, { method: 'POST', body: new URLSearchParams(params || {}) }).then(function (res) {
      if (!res.ok) { throw new Error(res.status); }
      return res.text();
    });
  }

  // --- Dialogs. A dialog with unsaved changes is not closed by a stray tap
  // next to it, and asks before Cancel, the close button or Esc drop them.

  var lastFocus = null;

  function showModal(modal) {
    lastFocus = document.activeElement;
    modal.dataset.dirty = '';
    modal.classList.add('show');
    modal.showModal();
    // A new card starts at its title. The editor takes focus itself rather
    // than a field, so a phone does not open its keyboard over the card.
    var title = modal.id === 'addCardModal' && modal.querySelector('input[name=title]');
    (title || modal.querySelector('.modal-content')).focus();
  }

  function hideModal(modal, force) {
    if (!modal || !modal.classList.contains('show')) { return; }
    if (!force && modal.dataset.dirty && !confirm('Discard your changes?')) { return; }
    modal.classList.remove('show');
    modal.close();
    modal.dataset.dirty = '';
    if (lastFocus && document.contains(lastFocus)) { lastFocus.focus(); }
  }

  function markDirty(el) {
    var modal = el.closest('.modal');
    if (modal) { modal.dataset.dirty = '1'; }
  }

  document.addEventListener('input', function (e) { markDirty(e.target); });

  function addSubtaskRow(container, after) {
    var row = document.getElementById('subtask-row-template').content.firstElementChild.cloneNode(true);
    container.insertBefore(row, after ? after.nextSibling : null);
    row.querySelector('.subtask-text').focus();
    markDirty(container);
  }

  // Enter in a subtask starts the next one instead of saving the card.
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') {
      if (document.querySelector('.modal.show')) { e.preventDefault(); }
      document.querySelectorAll('.modal.show').forEach(function (m) { hideModal(m); });
      return;
    }
    if (e.key === 'Enter' && !e.isComposing && e.target.classList.contains('subtask-text')) {
      e.preventDefault();
      var row = e.target.closest('.subtask-row');
      addSubtaskRow(row.parentNode, row);
    }
  });

  var toggle = document.getElementById('darkModeToggle');
  if (toggle) {
    toggle.addEventListener('click', function () {
      var dark = document.documentElement.classList.toggle('dark');
      try { localStorage.setItem('theme', dark ? 'dark' : 'light'); } catch (e) { /* storage disabled */ }
    });
  }

  // When the last drag ended; a drag is not also a tap that opens the card.
  var lastDrag = 0;

  document.addEventListener('click', function (e) {
    var t = e.target;
    var open = t.closest('[data-open-modal]');
    if (open) {
      var modal = document.getElementById(open.dataset.openModal);
      // The add form remembers which column it was opened from.
      var lane = modal.querySelector('input[name=lane]');
      if (lane && open.dataset.lane) { lane.value = open.dataset.lane; }
      showModal(modal);
      return;
    }
    if (t.closest('[data-close-modal]')) { hideModal(t.closest('.modal')); return; }
    if (t.classList.contains('modal')) {
      if (!t.dataset.dirty) { hideModal(t); }
      return;
    }
    if (t.closest('.add-subtask-btn')) {
      addSubtaskRow(t.closest('form').querySelector('.subtasks-container'));
      return;
    }
    var remove = t.closest('.remove-subtask-btn');
    if (remove) { markDirty(remove); remove.closest('.subtask-row').remove(); return; }
    var tab = t.closest('.lane-tab');
    if (tab) { showLane(tab.dataset.tab); return; }
    var move = t.closest('.move-btn');
    if (move) { moveToNeighbour(move); return; }
    var archive = t.closest('.archive-btn');
    if (archive) { archiveCard(archive); return; }

    // A tap on the card itself opens it, unless it was on something of its
    // own, ended a drag, or selected text.
    var card = t.closest('.card');
    if (card && !t.closest('button, a, form, input, select, textarea') &&
        Date.now() - lastDrag > 300 && !String(window.getSelection())) {
      var edit = card.querySelector('[hx-get$="/edit"]');
      if (edit) { edit.click(); }
    }
  });

  // A plain form that destroys something asks first.
  document.addEventListener('submit', function (e) {
    var question = e.target.dataset.confirm;
    if (question && !confirm(question)) { e.preventDefault(); }
  });

  // Serialise the subtask rows into the hidden field as "1|text" / "0|text"
  // lines. Capture phase, so it runs before htmx reads the form.
  document.addEventListener('submit', function (e) {
    var form = e.target;
    var hidden = form.querySelector('.subtasks-hidden');
    if (!hidden) { return; }
    var lines = [];
    form.querySelectorAll('.subtask-row').forEach(function (row) {
      var text = row.querySelector('.subtask-text').value.trim();
      if (text !== '') {
        lines.push((row.querySelector('.subtask-complete').checked ? '1' : '0') + '|' + text);
      }
    });
    hidden.value = lines.join('\n');
  }, true);

  // --- Columns as tabs (phones; the CSS decides when they are shown)

  var tabKey = 'kanban-tab:' + location.pathname;

  function showLane(id) {
    var lanes = document.querySelectorAll('.lane[data-tab]');
    if (!Array.from(lanes).some(function (lane) { return lane.dataset.tab === id; })) { return; }
    lanes.forEach(function (lane) {
      var on = lane.dataset.tab === id;
      lane.classList.toggle('active', on);
    });
    document.querySelectorAll('.lane-tab').forEach(function (tab) {
      var on = tab.dataset.tab === id;
      tab.classList.toggle('active', on);
      tab.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    // The big add button adds to the column that is showing.
    var add = document.querySelector('.add-row [data-open-modal]');
    if (add) { add.dataset.lane = id; }
    try { sessionStorage.setItem(tabKey, id); } catch (e) { /* storage disabled */ }
  }

  try {
    var saved = sessionStorage.getItem(tabKey);
    if (saved) { showLane(saved); }
  } catch (e) { /* storage disabled */ }

  // A link to a card (from search) shows its column and points it out.
  if (location.hash.indexOf('#card-') === 0) {
    var target = document.getElementById(location.hash.slice(1));
    if (target) {
      var section = target.closest('.lane');
      if (section) { showLane(section.dataset.tab); }
      target.scrollIntoView({ block: 'center' });
    }
  }

  // The counts on the tabs and column headers follow the cards, however they
  // moved: added, archived, dragged or moved with the buttons.
  //
  // Only the card lists are watched, and only their own children: a count
  // sits in a column header, and writing it must never be a change the
  // observer sees, or every count update triggers the next one and the page
  // hangs. A count is also only written when it differs, for the same reason.
  function updateCounts() {
    document.querySelectorAll('.lane-count').forEach(function (count) {
      var lane = document.getElementById('lane-' + count.dataset.countFor);
      if (!lane) { return; }
      var n = String(lane.querySelectorAll(':scope > .card').length);
      if (count.textContent !== n) { count.textContent = n; }
    });
  }
  var counter = new MutationObserver(updateCounts);
  document.querySelectorAll('.lane-cards').forEach(function (lane) {
    counter.observe(lane, { childList: true });
  });

  // The left and right buttons on a card. The request names the column the
  // card is going to, so a tap that arrives twice cannot move it two columns.
  function moveToNeighbour(button) {
    var card = button.closest('.card');
    var section = card.closest('.lane');
    var target = button.dataset.move === '-1' ? section.previousElementSibling : section.nextElementSibling;
    if (!target || !target.dataset.tab) { return; }
    button.disabled = true;
    post('/card/' + card.dataset.id + '/move', { lane: target.dataset.tab }).then(function () {
      button.disabled = false;
      document.getElementById('lane-' + target.dataset.tab).appendChild(card);
      // Show where it went without switching away from this column.
      var tab = document.querySelector('.lane-tab[data-tab="' + target.dataset.tab + '"]');
      if (tab) {
        tab.classList.remove('bump');
        void tab.offsetWidth; // restart the animation
        tab.classList.add('bump');
      }
    }).catch(function () { failAndReload('The card could not be moved.'); });
  }

  // Archive takes the card off the board at once; Undo puts the same card
  // back in the same place.
  function archiveCard(button) {
    var card = button.closest('.card');
    var list = card.parentNode;
    var next = card.nextElementSibling;
    var id = card.dataset.id;
    button.disabled = true;
    post('/card/' + id + '/archive').then(function () {
      card.remove();
      toast('Card archived.', 'Undo', function () {
        post('/card/' + id + '/restore').then(function (html) {
          var tpl = document.createElement('template');
          tpl.innerHTML = html.trim();
          var restored = tpl.content.firstElementChild;
          list.insertBefore(restored, next && next.parentNode === list ? next : null);
          htmx.process(restored);
        }).catch(function () { failAndReload('The card could not be restored.'); });
      });
    }).catch(function () { failAndReload('The card could not be archived.'); });
  }

  // --- htmx

  // Saved in the editor, which says which column the card is in: if that is
  // another one now, move the card to its end.
  document.body.addEventListener('cardMoved', function (e) {
    var card = document.getElementById('card-' + e.detail.id);
    var lane = document.getElementById('lane-' + e.detail.lane);
    if (card && lane && card.parentNode !== lane) { lane.appendChild(card); }
  });

  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (e.detail.target.id === 'editCardModalContent') {
      showModal(document.getElementById('editCardModal'));
    }
  });

  // A form that saved closes its modal; the add form is emptied for the next card.
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var form = e.detail.elt;
    if (!e.detail.successful || form.tagName !== 'FORM') { return; }
    var modal = form.closest('.modal');
    if (modal) { hideModal(modal, true); }
    if (form.id === 'addCardForm') {
      form.reset();
      form.querySelector('.subtasks-container').replaceChildren();
    }
  });

  // htmx does not swap error responses, so without this a failed save is
  // silent. A 409 means the page is out of date: show what is stored.
  document.body.addEventListener('htmx:responseError', function (e) {
    var why = e.detail.xhr.responseText.trim() || 'error ' + e.detail.xhr.status;
    if (e.detail.xhr.status === 409) {
      failAndReload('Not saved: ' + why + '.');
    } else {
      toast('Not saved: ' + why);
    }
  });
  document.body.addEventListener('htmx:sendError', function () {
    toast('Not saved: the server could not be reached.');
  });

  // --- Drag and drop. One request per drop: the column the card was dropped
  // in, with its cards in their new order. A card leaving another column
  // needs no second request, the server moves it. If saving fails the page
  // reloads, so it shows what is stored rather than an order that only
  // exists in this tab.
  document.querySelectorAll('.lane-cards').forEach(function (lane) {
    new Sortable(lane, {
      group: 'kanban',
      animation: 150,
      // On a touch screen a card is dragged after a short hold; a finger that
      // moves before then is scrolling the page.
      delay: 250,
      delayOnTouchOnly: true,
      touchStartThreshold: 5,
      // Pressing a link opens it rather than picking the card up.
      filter: 'a',
      preventOnFilter: false,
      onEnd: function (evt) {
        lastDrag = Date.now();
        if (evt.from === evt.to && evt.oldIndex === evt.newIndex) { return; }
        var ids = Array.from(evt.to.children)
          .filter(function (el) { return el.dataset.id; })
          .map(function (el) { return Number(el.dataset.id); });
        fetch(document.querySelector('.board').dataset.orderUrl, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ lane: Number(evt.to.dataset.lane), order: ids })
        }).then(function (res) {
          if (!res.ok) { throw new Error(res.status); }
        }).catch(function () { failAndReload('The new order could not be saved.'); });
      }
    });
  });
})();
