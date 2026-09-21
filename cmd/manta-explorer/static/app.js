/* manta-explorer front end: a thin client over the JSON API in server.go. */
(() => {
  'use strict';

  // ---- stream definitions ------------------------------------------------

  const STREAMS = {
    entities: {
      label: 'Entity ops',
      endpoint: '/api/entities',
      pickerKey: 'classes',
      pickerParam: 'class',
      pickerId: c => String(c.id),
      countKey: 'entities',
      columns: ['tick', 'op', 'class', 'index', 'serial'],
      render: renderEntityRow,
      detail: showEntityDetail,
    },
    events: {
      label: 'Game events',
      endpoint: '/api/game-events',
      pickerKey: 'gameEventNames',
      pickerParam: 'name',
      pickerId: c => c.name,
      countKey: 'gameEvents',
      columns: ['tick', 'event', 'keys'],
      render: renderGameEventRow,
      detail: showGameEventDetail,
    },
    combatlog: {
      label: 'Combat log',
      endpoint: '/api/combat-log',
      pickerKey: 'combatLogTypes',
      pickerParam: 'type',
      pickerId: c => c.name,
      countKey: 'combatLog',
      columns: ['tick', 'type', 'attacker', 'target', 'inflictor', 'value', 'health', 'time'],
      render: renderCombatLogRow,
      detail: showCombatLogDetail,
    },
    stupdates: {
      label: 'String table updates',
      endpoint: '/api/string-table-updates',
      pickerKey: 'stringTables',
      pickerParam: 'table',
      pickerId: c => c.name,
      pickerCount: c => c.updates,
      countKey: 'stringTableUpdates',
      columns: ['tick', 'table', 'entries changed'],
      render: renderStringTableUpdateRow,
      detail: showStringTableUpdateDetail,
    },
    tables: {
      label: 'String table contents',
      endpoint: null, // /api/string-tables/{name}
      pickerKey: null,
      columns: ['index', 'key'],
      render: renderTableEntryRow,
      detail: showTableEntryDetail,
    },
  };

  const state = {
    stream: 'entities',
    summary: null,
    offset: 0,
    limit: 100,
    total: 0,
    rows: [],
    selectedId: null,
    // Per stream: a Set of selected picker ids, or null meaning "all".
    picks: { entities: null, events: null, combatlog: null, stupdates: null },
    ops: null, // Set of selected op values for entities, or null for all.
    from: '', to: '', index: '',
    table: '', q: '',
    pickerFilter: '',
    // Parsed from the URL hash (#stream/rowId[/inspect]) and consumed once.
    deepLink: null,
  };

  // ---- DOM helpers ----------------------------------------------------------

  const $ = id => document.getElementById(id);

  function h(tag, attrs, ...children) {
    const el = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v == null) continue;
        if (k === 'class') el.className = v;
        else if (k === 'text') el.textContent = v;
        else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
        else el.setAttribute(k, v);
      }
    }
    for (const c of children.flat()) {
      if (c == null) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }

  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }

  const fmt = new Intl.NumberFormat('en-US');
  const n = x => fmt.format(x);

  async function getJSON(url) {
    const res = await fetch(url);
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `${res.status} ${res.statusText}`);
    return body;
  }

  function setStatus(msg, isError) {
    const el = $('status');
    el.textContent = msg || '';
    el.className = isError ? 'danger' : 'muted';
  }

  // ---- summary / header -------------------------------------------------------

  function renderHeader() {
    const s = state.summary;
    $('file-name').textContent = s.file.name || '';
    const m = s.match || {};
    const parts = [];
    if (m.matchId) parts.push(h('span', null, 'match ', h('b', { text: m.matchId })));
    if (m.winner != null) parts.push(h('span', null, 'winner ', h('b', { text: m.winner === 2 ? 'Radiant' : m.winner === 3 ? 'Dire' : String(m.winner) })));
    if (s.file.gameBuild) parts.push(h('span', null, 'build ', h('b', { text: s.file.gameBuild })));
    if (s.file.lastTick != null) parts.push(h('span', null, 'ticks ', h('b', { text: n(s.file.lastTick) })));
    if (s.file.server) parts.push(h('span', { class: 'muted', text: s.file.server }));
    if (s.file.parseError) parts.push(h('span', { class: 'danger', text: 'parse error: ' + s.file.parseError }));
    if (!s.file.fieldsRecorded) parts.push(h('span', { class: 'muted', text: '(changed field names are not exposed by manta; inspect state at a tick instead)' }));
    clear($('match-summary')).append(...parts.flatMap((p, i) => i ? [' · ', p] : [p]));
  }

  function renderTabs() {
    const wrap = clear($('stream-tabs'));
    for (const [key, def] of Object.entries(STREAMS)) {
      let count = '';
      if (def.countKey) count = n(state.summary.counts[def.countKey]);
      else if (key === 'tables') count = String(state.summary.stringTables.length) + ' tables';
      wrap.append(h('button', {
        type: 'button', class: key === state.stream ? 'active' : '',
        onclick: () => switchStream(key),
      }, h('span', { text: def.label }), h('span', { class: 'count', text: count })));
    }
  }

  // ---- picker -------------------------------------------------------------------

  function pickerItems() {
    const def = STREAMS[state.stream];
    if (!def.pickerKey) return [];
    return state.summary[def.pickerKey];
  }

  function renderPicker() {
    const def = STREAMS[state.stream];
    const list = clear($('picker-list'));
    const picker = $('picker');
    picker.hidden = !def.pickerKey;
    if (!def.pickerKey) { clear($('op-picker')); return; }

    const picks = state.picks[state.stream];
    const filter = state.pickerFilter.toLowerCase();
    let shown = 0;
    for (const item of pickerItems()) {
      const id = def.pickerId(item);
      if (filter && !item.name.toLowerCase().includes(filter)) continue;
      shown++;
      const checked = picks === null || picks.has(id);
      const count = def.pickerCount ? def.pickerCount(item) : item.count;
      list.append(h('label', { title: item.name },
        h('input', { type: 'checkbox', checked: checked ? '' : null, onchange: e => togglePick(id, e.target.checked) }),
        h('span', { class: 'name', text: item.name }),
        h('span', { class: 'count', text: n(count) })));
    }
    if (!shown) list.append(h('div', { class: 'muted', style: 'padding: 8px 10px', text: 'no matches' }));

    const opWrap = clear($('op-picker'));
    if (state.stream === 'entities') {
      opWrap.append(h('div', { class: 'head', text: 'operation' }));
      for (const op of state.summary.ops) {
        const id = String(op.id);
        const checked = state.ops === null || state.ops.has(id);
        opWrap.append(h('label', null,
          h('input', { type: 'checkbox', checked: checked ? '' : null, onchange: e => toggleOp(id, e.target.checked) }),
          h('span', { class: 'name', text: op.name }),
          h('span', { class: 'count', text: n(op.count) })));
      }
    }
  }

  function allPickerIds() {
    const def = STREAMS[state.stream];
    return pickerItems().map(def.pickerId);
  }

  function togglePick(id, on) {
    let picks = state.picks[state.stream];
    if (picks === null) picks = new Set(allPickerIds());
    if (on) picks.add(id); else picks.delete(id);
    if (picks.size === allPickerIds().length) picks = null;
    state.picks[state.stream] = picks;
    state.offset = 0;
    load();
  }

  function toggleOp(id, on) {
    let ops = state.ops;
    if (ops === null) ops = new Set(state.summary.ops.map(o => String(o.id)));
    if (on) ops.add(id); else ops.delete(id);
    if (ops.size === state.summary.ops.length) ops = null;
    state.ops = ops;
    state.offset = 0;
    load();
  }

  function setAllPicks(on) {
    const def = STREAMS[state.stream];
    if (!def.pickerKey) return;
    // "all"/"none" respect the current search filter: they act on the visible items.
    const filter = state.pickerFilter.toLowerCase();
    const visible = pickerItems().filter(i => !filter || i.name.toLowerCase().includes(filter)).map(def.pickerId);
    let picks = state.picks[state.stream];
    if (picks === null) picks = new Set(allPickerIds());
    for (const id of visible) { if (on) picks.add(id); else picks.delete(id); }
    if (picks.size === allPickerIds().length) picks = null;
    state.picks[state.stream] = picks;
    state.offset = 0;
    renderPicker();
    load();
  }

  // ---- filter bar -------------------------------------------------------------------

  function renderFilterBar() {
    const isEntities = state.stream === 'entities';
    const isTables = state.stream === 'tables';
    $('index-filter').hidden = !isEntities;
    $('table-filter').hidden = !isTables;
    document.querySelector('.tick-filter').hidden = isTables;
    $('f-from').value = state.from;
    $('f-to').value = state.to;
    $('f-index').value = state.index;
    $('f-q').value = state.q;
    $('f-limit').value = String(state.limit);

    if (isTables) {
      const sel = clear($('f-table'));
      for (const t of state.summary.stringTables) {
        sel.append(h('option', { value: t.name, text: `${t.name} (${n(t.entries)})` }));
      }
      if (!state.table && state.summary.stringTables.length) state.table = state.summary.stringTables[0].name;
      sel.value = state.table;
    }
  }

  function applyFilters() {
    state.from = $('f-from').value.trim();
    state.to = $('f-to').value.trim();
    state.index = $('f-index').value.trim();
    state.q = $('f-q').value.trim();
    state.table = $('f-table').value || state.table;
    state.limit = parseInt($('f-limit').value, 10) || 100;
    state.offset = 0;
    load();
  }

  function resetFilters() {
    state.from = state.to = state.index = state.q = '';
    state.picks[state.stream] = null;
    if (state.stream === 'entities') state.ops = null;
    state.pickerFilter = '';
    $('picker-search').value = '';
    state.offset = 0;
    renderFilterBar();
    renderPicker();
    load();
  }

  // ---- loading rows -------------------------------------------------------------------

  function buildURL() {
    const def = STREAMS[state.stream];
    const params = new URLSearchParams();
    params.set('offset', state.offset);
    params.set('limit', state.limit);

    if (state.stream === 'tables') {
      if (!state.table) return null;
      if (state.q) params.set('q', state.q);
      return `/api/string-tables/${encodeURIComponent(state.table)}?${params}`;
    }

    if (state.from) params.set('from', state.from);
    if (state.to) params.set('to', state.to);
    const picks = state.picks[state.stream];
    if (picks !== null) {
      if (picks.size === 0) return ''; // nothing selected: no request needed
      params.set(def.pickerParam, [...picks].join(','));
    }
    if (state.stream === 'entities') {
      if (state.ops !== null) {
        if (state.ops.size === 0) return '';
        params.set('op', [...state.ops].join(','));
      }
      if (state.index) params.set('index', state.index);
    }
    return `${def.endpoint}?${params}`;
  }

  let loadSeq = 0;
  async function load() {
    const seq = ++loadSeq;
    const url = buildURL();
    if (url === '' || url === null) {
      state.rows = []; state.total = 0;
      renderRows();
      setStatus(url === '' ? 'nothing selected' : '');
      return;
    }
    setStatus('loading…');
    try {
      const page = await getJSON(url);
      if (seq !== loadSeq) return; // a newer request superseded this one
      state.rows = page.rows || [];
      state.total = page.total || 0;
      renderRows();
      setStatus('');
    } catch (err) {
      if (seq !== loadSeq) return;
      setStatus(err.message, true);
    }
  }

  function renderRows() {
    const def = STREAMS[state.stream];
    const thead = clear(document.querySelector('#results thead'));
    thead.append(h('tr', null, def.columns.map(c => h('th', { text: c }))));

    const tbody = clear(document.querySelector('#results tbody'));
    if (!state.rows.length) {
      tbody.append(h('tr', { class: 'empty' }, h('td', { colspan: def.columns.length, text: 'no rows' })));
    }
    for (const row of state.rows) {
      const rowId = row.id != null ? row.id : row.index;
      const tr = def.render(row);
      tr.dataset.id = rowId;
      if (rowId === state.selectedId) tr.classList.add('selected');
      tr.addEventListener('click', () => selectRow(row, tr));
      tbody.append(tr);
    }

    const from = state.total ? state.offset + 1 : 0;
    const to = Math.min(state.offset + state.rows.length, state.total);
    $('pg-info').textContent = `${n(from)}–${n(to)} of ${n(state.total)}`;
    $('pg-prev').disabled = state.offset === 0;
    $('pg-next').disabled = state.offset + state.limit >= state.total;
    $('table-wrap').scrollTop = 0;
    consumeDeepLinkRow();
  }

  function selectRow(row, tr) {
    document.querySelectorAll('#results tbody tr.selected').forEach(el => el.classList.remove('selected'));
    tr.classList.add('selected');
    state.selectedId = row.id != null ? row.id : row.index;
    if (state.stream !== 'tables') history.replaceState(null, '', `#${state.stream}/${state.selectedId}`);
    STREAMS[state.stream].detail(row);
  }

  // ---- deep links -------------------------------------------------------------------
  // #<stream>/<rowId>[/inspect] opens a stream on the page holding that row,
  // selects it, and optionally runs the entity state inspection.

  function parseDeepLink() {
    const m = /^#([a-z]+)(?:\/(\d+))?(?:\/(inspect))?$/.exec(location.hash);
    if (!m || !STREAMS[m[1]]) return null;
    return { stream: m[1], row: m[2] != null ? parseInt(m[2], 10) : null, inspect: m[3] === 'inspect' };
  }

  function applyDeepLinkOffset() {
    const dl = state.deepLink;
    if (!dl || dl.row == null) return;
    // With no filters active, row ids are positions in the stream, so the page
    // holding the row is known without a round trip.
    state.offset = Math.floor(dl.row / state.limit) * state.limit;
  }

  function consumeDeepLinkRow() {
    const dl = state.deepLink;
    if (!dl || dl.row == null) return;
    const tr = document.querySelector(`#results tbody tr[data-id="${dl.row}"]`);
    const row = state.rows.find(r => (r.id != null ? r.id : r.index) === dl.row);
    dl.row = null;
    if (tr && row) selectRow(row, tr);
    else state.deepLink = null;
  }

  function consumeDeepLinkInspect() {
    const dl = state.deepLink;
    if (!dl || !dl.inspect) return false;
    state.deepLink = null;
    return true;
  }

  function switchStream(key) {
    if (state.stream === key) return;
    state.stream = key;
    state.offset = 0;
    state.selectedId = null;
    state.pickerFilter = '';
    $('picker-search').value = '';
    clear($('detail-body')).append(h('p', { class: 'muted', text: 'Select a row to inspect it.' }));
    renderTabs();
    renderPicker();
    renderFilterBar();
    load();
  }

  // ---- row renderers ------------------------------------------------------------------

  function opClass(name) {
    const l = name.toLowerCase();
    if (l.includes('created')) return 'op-created';
    if (l.includes('deleted') || l.includes('left')) return 'op-deleted';
    if (l.includes('entered')) return 'op-entered';
    return 'op-updated';
  }

  function renderEntityRow(r) {
    return h('tr', null,
      h('td', { class: 'num', text: n(r.tick) }),
      h('td', { class: opClass(r.opName), text: r.opName }),
      h('td', { text: r.class }),
      h('td', { class: 'num', text: r.index }),
      h('td', { class: 'num dim', text: r.serial }));
  }

  function renderGameEventRow(r) {
    return h('tr', null,
      h('td', { class: 'num', text: n(r.tick) }),
      h('td', { text: r.name }),
      h('td', { class: 'wrap dim', text: (r.keys || []).map(k => `${k.name}=${k.value}`).join('  ') }));
  }

  function renderCombatLogRow(r) {
    return h('tr', null,
      h('td', { class: 'num', text: n(r.tick) }),
      h('td', { text: r.type }),
      h('td', { class: 'clip', text: r.attacker }),
      h('td', { class: 'clip', text: r.target }),
      h('td', { class: 'clip dim', text: r.inflictor }),
      h('td', { class: 'num', text: r.value }),
      h('td', { class: 'num dim', text: r.health }),
      h('td', { class: 'num dim', text: r.timestamp.toFixed(2) }));
  }

  function renderStringTableUpdateRow(r) {
    return h('tr', null,
      h('td', { class: 'num', text: n(r.tick) }),
      h('td', { text: r.table }),
      h('td', { class: 'num', text: r.count }));
  }

  function renderTableEntryRow(r) {
    return h('tr', null,
      h('td', { class: 'num', text: r.index }),
      h('td', { class: 'clip', text: r.key }));
  }

  // ---- detail panel --------------------------------------------------------------------

  function kv(pairs, opts = {}) {
    const grid = h('div', { class: 'kv' });
    for (const [k, v, extra] of pairs) {
      grid.append(h('div', { class: 'k', text: k }));
      const val = h('div', { class: 'v' + (extra && extra.changed ? ' changed' : '') }, String(v));
      if (extra && extra.type) val.append(h('span', { class: 'type', text: extra.type }));
      grid.append(val);
    }
    return grid;
  }

  function detail(...children) {
    const body = clear($('detail-body'));
    body.append(...children);
    return body;
  }

  async function showEntityDetail(row) {
    const head = h('h2', null, `${row.class} `, h('span', { class: 'muted', text: `#${row.index}` }));
    const facts = kv([
      ['tick', n(row.tick)],
      ['operation', row.opName],
      ['index', row.index],
      ['serial', row.serial],
      ['class id', row.classId],
      ['changed fields', row.fieldCount],
    ]);
    const fieldsWrap = h('div');
    const stateWrap = h('div');
    const inspectBtn = h('button', { type: 'button', text: `inspect state at tick ${n(row.tick)}` });
    const prevBtn = h('button', { type: 'button', text: 'state before this op' , title: `state at tick ${Math.max(0, row.tick - 1)}` });
    const actions = h('div', { class: 'actions' }, inspectBtn, prevBtn);

    detail(head, facts, h('h3', { text: 'changed fields' }), fieldsWrap, actions, stateWrap);

    let fields = row.fields || [];
    if (row.fieldCount > fields.length) {
      try {
        const full = await getJSON(`/api/entities/${row.id}`);
        fields = full.fields || [];
      } catch (err) {
        fieldsWrap.append(h('p', { class: 'danger', text: err.message }));
      }
    }
    if (fields.length) fieldsWrap.append(h('ul', { class: 'fields' }, fields.map(f => h('li', { text: f }))));
    else if (!state.summary.file.fieldsRecorded) fieldsWrap.append(h('p', { class: 'note', text: 'not available: manta reports which entity changed, not which fields; inspect the state at this tick and the tick before to compare' }));
    else fieldsWrap.append(h('p', { class: 'note', text: 'none (leave/delete operations carry no fields)' }));

    const changed = new Set(fields);
    const inspect = async tick => {
      clear(stateWrap).append(h('p', { class: 'note', text: `re-parsing to tick ${n(tick)}…` }));
      inspectBtn.disabled = prevBtn.disabled = true;
      try {
        const st = await getJSON(`/api/entity-state?index=${row.index}&tick=${tick}`);
        renderEntityState(stateWrap, st, changed);
      } catch (err) {
        clear(stateWrap).append(h('p', { class: 'danger', text: err.message }));
      } finally {
        inspectBtn.disabled = prevBtn.disabled = false;
      }
    };
    inspectBtn.addEventListener('click', () => inspect(row.tick));
    prevBtn.addEventListener('click', () => inspect(Math.max(0, row.tick - 1)));

    if (consumeDeepLinkInspect()) inspect(row.tick);
  }

  function renderEntityState(wrap, st, changed) {
    clear(wrap);
    wrap.append(h('h3', { text: `state at tick ${n(st.requestedTick)}` }));
    wrap.append(h('p', { class: 'note', text: `last applied packet tick ${n(st.stateTick)}` }));
    if (st.missing) {
      wrap.append(h('p', { class: 'danger', text: 'entity does not exist at this tick (not yet created, or already deleted)' }));
      return;
    }
    wrap.append(kv([['class', st.class], ['serial', st.serial], ['fields', st.fields.length]]));
    const search = h('input', { type: 'search', placeholder: 'filter fields…' });
    const grid = h('div');
    const draw = () => {
      const q = search.value.toLowerCase();
      const rows = st.fields.filter(f => !q || f.name.toLowerCase().includes(q) || f.value.toLowerCase().includes(q));
      clear(grid).append(kv(rows.map(f => [f.name, f.value, { type: f.type, changed: changed.has(f.name) }])));
      if (!rows.length) grid.append(h('p', { class: 'note', text: 'no matching fields' }));
    };
    search.addEventListener('input', draw);
    wrap.append(h('div', { style: 'margin-top: 10px' }, search, grid));
    draw();
    if (changed.size) wrap.append(h('p', { class: 'note', text: 'highlighted values were changed by the selected operation' }));
  }

  function showGameEventDetail(row) {
    detail(
      h('h2', { text: row.name }),
      kv([['tick', n(row.tick)]]),
      h('h3', { text: 'keys' }),
      kv((row.keys || []).map(k => [k.name, k.value])));
  }

  async function showCombatLogDetail(row) {
    const raw = h('div');
    detail(
      h('h2', { text: row.type }),
      kv([
        ['tick', n(row.tick)],
        ['timestamp', row.timestamp],
        ['attacker', row.attacker],
        ['target', row.target],
        ['inflictor', row.inflictor],
        ['value', row.value],
        ['health', row.health],
      ]),
      h('h3', { text: 'full message' }),
      raw);
    try {
      const full = await getJSON(`/api/combat-log/${row.id}`);
      const msg = full.message || {};
      raw.append(kv(Object.entries(msg).map(([k, v]) => [k, typeof v === 'object' ? JSON.stringify(v) : v])));
    } catch (err) {
      raw.append(h('p', { class: 'danger', text: err.message }));
    }
  }

  function showStringTableUpdateDetail(row) {
    detail(
      h('h2', { text: row.table }),
      kv([['tick', n(row.tick)], ['entries changed', row.count]]),
      h('p', { class: 'note', text: 'manta applies the entries internally and exposes keys by index only; see the String table contents tab for the final keys' }));
  }

  function showTableEntryDetail(row) {
    detail(
      h('h2', { text: state.table }),
      kv([['index', row.index], ['key', row.key]]),
      h('p', { class: 'note', text: 'values are not exposed by manta; only keys are available by index' }));
  }

  // ---- wiring ---------------------------------------------------------------------------

  function wire() {
    $('filter-bar').addEventListener('submit', e => { e.preventDefault(); applyFilters(); });
    $('f-reset').addEventListener('click', resetFilters);
    $('f-table').addEventListener('change', applyFilters);
    $('f-limit').addEventListener('change', applyFilters);
    $('pg-prev').addEventListener('click', () => { state.offset = Math.max(0, state.offset - state.limit); load(); });
    $('pg-next').addEventListener('click', () => { state.offset += state.limit; load(); });
    $('picker-search').addEventListener('input', e => { state.pickerFilter = e.target.value; renderPicker(); });
    $('picker-all').addEventListener('click', () => setAllPicks(true));
    $('picker-none').addEventListener('click', () => setAllPicks(false));
  }

  async function init() {
    wire();
    setStatus('loading summary…');
    try {
      state.summary = await getJSON('/api/summary');
    } catch (err) {
      setStatus('failed to load summary: ' + err.message, true);
      return;
    }
    state.deepLink = parseDeepLink();
    if (state.deepLink) {
      state.stream = state.deepLink.stream;
      applyDeepLinkOffset();
    }
    renderHeader();
    renderTabs();
    renderPicker();
    renderFilterBar();
    load();
  }

  init();
})();
