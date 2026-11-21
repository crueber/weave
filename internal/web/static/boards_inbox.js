// Board Inbox (templates/_boards_inbox.html): "Move to column" publishes a draft (op card.draft.publish) and puts
// the new card at the end of its column, in view, with a toast. Capturing uses the shared composer
// (boards_compose.js), dragging a draft onto a column uses boards_dnd.js, live refresh boards_board.js.
// Without JS the menu's buttons are a plain form post that returns to the board.
(function () {
  var B = window.Boards; if (!B || B.inboxInit) return; B.inboxInit = true;

  function inbox() { return document.querySelector('.boards-inbox'); }

  document.addEventListener('submit', function (e) {
    var f = e.target; if (!f.matches || !f.matches('form[data-publish]')) return;
    e.preventDefault();
    var btn = e.submitter && e.submitter.name === 'listId' ? e.submitter : null; if (!btn) return;
    var id = f.querySelector('input[name=id]').value, listId = btn.value, listName = btn.textContent.trim();
    var menu = B.menuOf(f); if (menu) menu.removeAttribute('open');
    var draft = document.getElementById('tile-' + id);
    if (draft) draft.classList.add('is-pending');            // keeps the live refresh off this column meanwhile
    B.post(f.action, { id: id, listId: listId }).then(function (r) {
      if (draft) draft.classList.remove('is-pending');
      if (!r.ok) { B.toast(r.error || 'Could not move the draft', { error: true }); B.refresh(true); return; }
      var col = document.getElementById('col-' + listId), box = col && col.querySelector('.boards-cards');
      var tile = [].slice.call(B.fragment(r.html).children).filter(function (n) { return n.classList.contains('boards-tile'); })[0];
      var next = draft && (draft.nextElementSibling && draft.nextElementSibling.classList.contains('boards-draft') ? draft.nextElementSibling : draft.previousElementSibling);
      if (draft) draft.remove();
      var ib = inbox(); if (ib) B.recount(ib);
      if (tile && box) {
        if (col.classList.contains('is-collapsed')) B.setCollapsed(col, false);
        tile.classList.add('is-new');
        box.insertBefore(tile, box.querySelector('.boards-empty-list'));
        B.process(tile); B.recount(col);
        box.scrollTop = box.scrollHeight;
        tile.scrollIntoView({ block: 'nearest', inline: 'nearest', behavior: B.reduced() ? 'auto' : 'smooth' });
      } else B.refresh(true);                                 // column filtered out / not on screen: let the board catch up
      // focus stays in the Inbox (next draft), so triaging several in a row is quick
      var l = next && next.isConnected && next.querySelector('.boards-tile-link');
      if (l) l.focus({ preventScroll: true });
      B.announce('Moved the draft to ' + listName);
      B.toast('Moved to ' + listName, tile ? { action: 'Open', onAction: function () { B.openCard(id); } } : {});
    });
  });
})();
