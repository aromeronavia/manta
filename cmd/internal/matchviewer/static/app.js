/* manta-map viewer: replays sampled match data on a minimap. Consumes the
   document produced by extract.go, either from window.MATCH (static export)
   or api/match (served). */
(() => {
  'use strict';

  const CDN = 'https://cdn.cloudflare.steamstatic.com/apps/dota2/images';
  const MAP_CENTER = 16384;
  const CLOCK_UNKNOWN = -32768;
  const ABSENT = -1;
  const TEAM_NAME = { 2: 'Radiant', 3: 'Dire' };
  const TEAM_COLOR = { 2: '#23B200', 3: '#B21000' };
  const CHART_COLOR = { 2: '#23B200', 3: '#D93D2C' };
  const DEATH_MARK_SECONDS = 20;
  const TELEPORT_UNITS = 2500; // a jump this large between samples is a teleport, not movement
  const NOTABLE_EVENTS = new Set([
    'FIRSTBLOOD', 'TOWER_KILL', 'TOWER_DENY', 'BARRACKS_KILL', 'ROSHAN_KILL', 'AEGIS', 'AEGIS_STOLEN',
    'DENIED_AEGIS', 'COURIER_LOST', 'BUYBACK', 'PAUSED', 'UNPAUSED', 'GLYPH_USED',
    'HERO_DENY', 'RAPIER', 'CANT_USE_ACTION_ITEM', 'MINIBOSS_KILL', 'TORMENTOR_KILL', 'DISCONNECT', 'RECONNECT',
  ]);

  let M = null;
  const state = {
    tick: 0,
    playing: false,
    speed: 4,
    trailSeconds: 15,
    layers: { heroes: true, names: true, trails: true, kills: true, buildings: true, couriers: true, roshan: true, wards: true, alerts: true },
    selected: null,
    panel: 'score',
    hover: null,
    chartHover: null,
    lastPanelIdx: -1,
    feedCount: 0,
    dragging: null,
    farmPlayer: null,
    farmHeat: true,
    view: { zoom: 1, cx: 0.5, cy: 0.5, follow: false },
    sideCollapsed: false,
  };
  const els = {};
  const images = new Map();
  let minimapImg = null;
  let series = null;
  let feedEntries = [];
  let roshanDeaths = [];
  let loadouts = new Map(); // player id -> ability, cooldown and charge events

  // ---- helpers -----------------------------------------------------------------

  const $ = id => document.getElementById(id);

  function h(tag, attrs, ...children) {
    const el = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v == null) continue;
        if (k === 'class') el.className = v;
        else if (k === 'text') el.textContent = v;
        else if (k === 'style') el.style.cssText = v;
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

  const nf = new Intl.NumberFormat('en-US');
  const fmtNum = x => nf.format(x);
  const fmtK = x => (Math.abs(x) >= 1000 ? (x / 1000).toFixed(Math.abs(x) >= 10000 ? 0 : 1) + 'k' : String(x));

  function fmtClock(s) {
    if (s == null) return '–:––';
    const neg = s < 0;
    s = Math.abs(Math.floor(s));
    const m = Math.floor(s / 60);
    const sec = String(s % 60).padStart(2, '0');
    return `${neg ? '−' : ''}${m}:${sec}`;
  }

  function heroShort(npc) { return (npc || '').replace(/^npc_dota_hero_/, ''); }
  function heroIconURL(npc) { return `${CDN}/dota_react/heroes/icons/${heroShort(npc)}.png`; }
  function heroName(npc) {
    if (!npc) return 'unknown hero';
    return heroShort(npc).split('_').map(p => p ? p[0].toUpperCase() + p.slice(1) : p).join(' ');
  }
  function unitName(npc) {
    if (!npc) return 'unknown';
    if (npc.startsWith('npc_dota_hero_')) return heroName(npc);
    if (npc.startsWith('npc_dota_creep_')) return 'creeps';
    if (npc.startsWith('npc_dota_neutral_')) return 'neutrals';
    if (npc === 'npc_dota_roshan') return 'Roshan';
    if (/tower/.test(npc)) return 'a tower';
    if (/fort/.test(npc)) return 'the Ancient';
    if (/fountain/.test(npc)) return 'the fountain';
    return npc.replace(/^(npc_)?dota_/, '').replace(/_/g, ' ');
  }
  function eventName(type) {
    return type.toLowerCase().replace(/_/g, ' ').replace(/^./, c => c.toUpperCase());
  }
  function itemIconURL(name) {
    if (!name) return null;
    if (name.startsWith('item_recipe_')) return `${CDN}/dota_react/items/recipe.png`;
    return `${CDN}/dota_react/items/${name.replace(/^item_/, '')}.png`;
  }
  function itemLabel(name) {
    return (name || '').replace(/^item_(recipe_)?/, '').replace(/_/g, ' ');
  }

  // Lazily loads an image; returns it once complete, null before/failed.
  function img(url) {
    if (!url) return null;
    let entry = images.get(url);
    if (!entry) {
      entry = { img: new Image(), ok: false, failed: false };
      entry.img.onload = () => { entry.ok = true; };
      entry.img.onerror = () => { entry.failed = true; };
      entry.img.src = url;
      images.set(url, entry);
    }
    return entry.ok ? entry.img : null;
  }

  // ---- sample lookup -----------------------------------------------------------

  // Index of the last sample at or before tick, plus the fraction towards the next.
  function idxFor(tick) {
    const t = M.ticks;
    let lo = 0, hi = t.length - 1;
    if (tick <= t[0]) return { i: 0, f: 0 };
    if (tick >= t[hi]) return { i: hi, f: 0 };
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (t[mid] <= tick) lo = mid; else hi = mid;
    }
    const span = t[hi] - t[lo];
    return { i: lo, f: span > 0 ? (tick - t[lo]) / span : 0 };
  }

  // Interpolated position from x/y series; null when absent. Snaps across teleports.
  function posAt(xs, ys, idx) {
    const x0 = xs[idx.i], y0 = ys[idx.i];
    if (x0 === ABSENT || x0 == null) return null;
    const j = Math.min(idx.i + 1, xs.length - 1);
    const x1 = xs[j], y1 = ys[j];
    if (x1 === ABSENT || x1 == null || idx.f === 0) return { x: x0, y: y0 };
    if (Math.hypot(x1 - x0, y1 - y0) > TELEPORT_UNITS) return { x: x0, y: y0 };
    return { x: x0 + (x1 - x0) * idx.f, y: y0 + (y1 - y0) * idx.f };
  }

  function clockAt(tick) {
    const idx = idxFor(tick);
    const c0 = M.clock[idx.i];
    if (c0 === CLOCK_UNKNOWN) return null;
    const j = Math.min(idx.i + 1, M.clock.length - 1);
    const c1 = M.clock[j];
    if (c1 === CLOCK_UNKNOWN) return c0;
    return c0 + (c1 - c0) * idx.f;
  }

  function tickForClock(clock) {
    for (let i = 0; i < M.clock.length; i++) {
      if (M.clock[i] !== CLOCK_UNKNOWN && M.clock[i] >= clock) {
        if (i === 0) return M.ticks[0];
        const c0 = M.clock[i - 1], c1 = M.clock[i];
        if (c0 === CLOCK_UNKNOWN || c1 === c0) return M.ticks[i];
        return M.ticks[i - 1] + (M.ticks[i] - M.ticks[i - 1]) * ((clock - c0) / (c1 - c0));
      }
    }
    return M.ticks[M.ticks.length - 1];
  }

  const firstTick = () => M.ticks[0];
  const lastTick = () => M.ticks[M.ticks.length - 1];
  const tickRate = () => M.match.tickRate || 30;

  // ---- derived data ------------------------------------------------------------

  function deriveSeries() {
    const n = M.ticks.length;
    const gold = new Array(n), xp = new Array(n);
    for (let i = 0; i < n; i++) {
      let g = 0, x = 0, any = false;
      for (const p of M.players) {
        const nw = p.netWorth[i], px = p.xp[i];
        if (nw == null || nw === ABSENT) continue;
        any = true;
        const sign = p.team === 2 ? 1 : -1;
        g += sign * nw;
        x += sign * (px === ABSENT ? 0 : px);
      }
      gold[i] = any ? g : null;
      xp[i] = any ? x : null;
    }
    series = { gold, xp };

    roshanDeaths = [];
    if (M.roshan) {
      for (let i = 1; i < n; i++) {
        if (M.roshan.alive[i - 1] === 1 && M.roshan.alive[i] !== 1) roshanDeaths.push(M.ticks[i]);
      }
    }

    loadouts = new Map();
    for (const p of M.players) loadouts.set(p.id, { abilities: [], cooldowns: [], charges: [] });
    for (const e of M.abilities || []) { const L = loadouts.get(e.player); if (L) L.abilities.push(e); }
    for (const c of M.cooldowns || []) { const L = loadouts.get(c.player); if (L) L.cooldowns.push(c); }
    for (const c of M.charges || []) { const L = loadouts.get(c.player); if (L) L.charges.push(c); }

    feedEntries = [];
    for (const k of M.kills) feedEntries.push({ kind: 'kill', tick: k.tick, clock: k.clock, k });
    for (const c of M.chat) feedEntries.push({ kind: 'chat', tick: c.tick, clock: c.clock, c });
    for (const e of M.events) if (NOTABLE_EVENTS.has(e.type)) feedEntries.push({ kind: 'event', tick: e.tick, clock: e.clock, e });
    feedEntries.sort((a, b) => a.tick - b.tick);
  }

  const playerById = id => M.players.find(p => p.id === id) || null;

  // ---- header ------------------------------------------------------------------

  function renderHeader() {
    const m = M.match;
    $('match-title').textContent = m.fileName || '';
    const parts = [];
    if (m.id && m.id !== '0') parts.push(h('span', null, 'match ', h('b', { text: m.id })));
    if (m.winner === 2 || m.winner === 3) parts.push(h('span', null, 'winner ', h('b', { class: m.winner === 2 ? 'team-radiant' : 'team-dire', text: TEAM_NAME[m.winner] })));
    const lastClock = clockAt(lastTick());
    if (lastClock != null) parts.push(h('span', null, 'duration ', h('b', { text: fmtClock(lastClock) })));
    parts.push(h('span', null, 'map ', h('b', { text: m.map.id })));
    if (m.gameBuild) parts.push(h('span', null, 'build ', h('b', { text: m.gameBuild })));
    if (!m.map.image) parts.push(h('span', { class: 'muted', text: '(no minimap image: schematic map)' }));
    clear($('match-summary')).append(...parts.flatMap((p, i) => i ? [' · ', p] : [p]));
  }

  // ---- map ---------------------------------------------------------------------

  // The canvas fills the map pane. The map is a square of side mapSize × zoom
  // centred on (cx, cy) in normalised map coordinates: letterboxed at 1×,
  // covering the canvas once zoomed in.
  let mapSize = 0, mapW = 0, mapH = 0;
  const ZOOM_MIN = 1, ZOOM_MAX = 8, FOLLOW_ZOOM = 3, ZOOM_STEP = 1.5;

  function resizeMap() {
    const pane = $('map-pane');
    const w = Math.max(200, pane.clientWidth - 20), hgt = Math.max(200, pane.clientHeight - 20);
    const dpr = window.devicePixelRatio || 1;
    const c = els.map;
    if (mapW !== w || mapH !== hgt || c.width !== Math.round(w * dpr)) {
      mapW = w; mapH = hgt; mapSize = Math.min(w, hgt);
      c.width = Math.round(w * dpr);
      c.height = Math.round(hgt * dpr);
      c.style.width = w + 'px'; c.style.height = hgt + 'px';
      clampView();
    }
  }

  // World units to the unit square: (0,0) top-left, (1,1) bottom-right.
  function norm(x, y) {
    const s = M.match.map.size;
    return [(x - MAP_CENTER) / s + 0.5, 0.5 - (y - MAP_CENTER) / s];
  }
  // World units to canvas pixels through the camera.
  function project(x, y) {
    const [u, v] = norm(x, y);
    const k = state.view.zoom * mapSize;
    return [(u - state.view.cx) * k + mapW / 2, (v - state.view.cy) * k + mapH / 2];
  }
  // World units to pixels of the unzoomed map square, for cached layers.
  function baseProject(x, y) {
    const [u, v] = norm(x, y);
    return [u * mapSize, v * mapSize];
  }
  // Where the map square sits on the canvas: left, top, side.
  function mapRect() {
    const k = state.view.zoom * mapSize;
    return [mapW / 2 - state.view.cx * k, mapH / 2 - state.view.cy * k, k];
  }
  // Keeps the camera on the map: centred along an axis the map does not fill,
  // otherwise never looking past an edge.
  function clampView() {
    const v = state.view;
    v.zoom = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, v.zoom));
    const k = v.zoom * mapSize;
    v.cx = k <= mapW ? 0.5 : Math.min(1 - mapW / (2 * k), Math.max(mapW / (2 * k), v.cx));
    v.cy = k <= mapH ? 0.5 : Math.min(1 - mapH / (2 * k), Math.max(mapH / (2 * k), v.cy));
  }
  // Zooms by factor, keeping the map point under canvas pixel (mx, my) still.
  function zoomAt(factor, mx = mapW / 2, my = mapH / 2) {
    const v = state.view;
    const k0 = v.zoom * mapSize;
    const u = v.cx + (mx - mapW / 2) / k0, w = v.cy + (my - mapH / 2) / k0;
    v.zoom = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, v.zoom * factor));
    const k1 = v.zoom * mapSize;
    v.cx = u - (mx - mapW / 2) / k1;
    v.cy = w - (my - mapH / 2) / k1;
    clampView();
  }
  function resetView() {
    state.view.zoom = 1; state.view.cx = state.view.cy = 0.5; state.view.follow = false;
    updateMapControls();
  }
  // Locks the camera on the selected hero, zooming in if the map is still whole.
  function setFollow(on) {
    state.view.follow = on && state.selected != null;
    if (state.view.follow && state.view.zoom < FOLLOW_ZOOM) state.view.zoom = FOLLOW_ZOOM;
    updateMapControls();
  }
  function followSelected(idx) {
    if (!state.view.follow) return;
    const p = playerById(state.selected);
    if (!p) { setFollow(false); return; }
    const pos = posAt(p.x, p.y, idx);
    if (!pos) return;
    const [u, v] = norm(pos.x, pos.y);
    const view = state.view;
    const dx = u - view.cx, dy = v - view.cy;
    // Ease toward the hero; snap after a seek or teleport.
    const a = Math.hypot(dx, dy) * view.zoom > 0.5 ? 1 : 0.2;
    view.cx += dx * a; view.cy += dy * a;
    clampView();
  }
  function updateMapControls() {
    const follow = $('zoom-follow');
    follow.classList.toggle('active', state.view.follow);
    follow.disabled = state.selected == null;
    follow.title = state.selected == null ? 'select a hero to follow it' : (state.view.follow ? 'stop following (F)' : 'follow the selected hero (F)');
  }

  function drawSchematic(ctx) {
    ctx.fillStyle = '#12261a';
    ctx.fillRect(0, 0, mapSize, mapSize);
    // River from the top-left to the bottom-right corner.
    ctx.save();
    ctx.strokeStyle = '#1f4a5c';
    ctx.lineWidth = mapSize * 0.05;
    ctx.lineCap = 'round';
    ctx.beginPath();
    ctx.moveTo(mapSize * 0.02, mapSize * 0.02);
    ctx.lineTo(mapSize * 0.98, mapSize * 0.98);
    ctx.stroke();
    ctx.restore();
    // Bases.
    ctx.fillStyle = 'rgba(35, 178, 0, 0.12)';
    ctx.beginPath(); ctx.arc(mapSize * 0.12, mapSize * 0.88, mapSize * 0.14, 0, Math.PI * 2); ctx.fill();
    ctx.fillStyle = 'rgba(178, 16, 0, 0.14)';
    ctx.beginPath(); ctx.arc(mapSize * 0.88, mapSize * 0.12, mapSize * 0.14, 0, Math.PI * 2); ctx.fill();
    ctx.strokeStyle = 'rgba(255,255,255,0.05)';
    ctx.lineWidth = 1;
    for (let i = 1; i < 8; i++) {
      const p = mapSize * i / 8;
      ctx.beginPath(); ctx.moveTo(p, 0); ctx.lineTo(p, mapSize); ctx.stroke();
      ctx.beginPath(); ctx.moveTo(0, p); ctx.lineTo(mapSize, p); ctx.stroke();
    }
  }

  function drawMap(now = performance.now()) {
    resizeMap();
    const tick = state.tick;
    const idx = idxFor(tick);
    followSelected(idx);
    const c = els.map;
    const ctx = c.getContext('2d');
    const dpr = window.devicePixelRatio || 1;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.fillStyle = '#0b0f14';
    ctx.fillRect(0, 0, mapW, mapH);

    const [ox, oy, k] = mapRect();
    if (minimapImg && minimapImg.complete && minimapImg.naturalWidth) {
      ctx.drawImage(minimapImg, ox, oy, k, k);
    } else {
      ctx.save();
      ctx.translate(ox, oy); ctx.scale(k / mapSize, k / mapSize);
      drawSchematic(ctx);
      ctx.restore();
    }
    const L = state.layers;

    if (L.wards) drawWards(ctx, tick);
    if (L.buildings) drawBuildings(ctx, tick);
    if (L.roshan) drawRoshan(ctx, idx);
    if (L.couriers) drawCouriers(ctx, idx);
    if (state.panel === 'farm' && state.farmHeat) drawFarmHeat(ctx, tick, idx);
    if (L.kills) drawDeathMarks(ctx, tick);
    if (L.trails) drawTrails(ctx, tick, idx);
    if (L.heroes) drawHeroes(ctx, idx);
    if (L.alerts) drawItemAlerts(ctx, idx, now);
    drawMapTooltip(idx);
  }

  function drawWards(ctx, tick) {
    for (const w of M.wards) {
      if (w.placedTick > tick || (w.removedTick != null && w.removedTick <= tick)) continue;
      if (w.x === ABSENT) continue;
      const [px, py] = project(w.x, w.y);
      const owner = w.owner !== ABSENT ? playerById(w.owner) : null;
      ctx.lineWidth = 1.5;
      ctx.strokeStyle = owner ? owner.color : (TEAM_COLOR[w.team] || '#ccc');
      ctx.beginPath();
      if (w.kind === 'observer') {
        ctx.fillStyle = '#F3C623';
        ctx.arc(px, py, 4, 0, Math.PI * 2);
      } else {
        ctx.fillStyle = '#4FB3FF';
        ctx.moveTo(px, py - 5); ctx.lineTo(px + 5, py); ctx.lineTo(px, py + 5); ctx.lineTo(px - 5, py); ctx.closePath();
      }
      ctx.fill(); ctx.stroke();
    }
  }

  function drawBuildings(ctx, tick) {
    for (const b of M.buildings) {
      if (b.x === 0 && b.y === 0) continue;
      const [px, py] = project(b.x, b.y);
      const alive = b.destroyedTick == null || b.destroyedTick > tick;
      const color = TEAM_COLOR[b.team] || '#aaa';
      ctx.lineWidth = 1.5;
      ctx.strokeStyle = alive ? '#0b0f14' : '#6b7280';
      ctx.fillStyle = alive ? color : 'rgba(107,114,128,0.25)';
      ctx.beginPath();
      switch (b.kind) {
        case 'tower': ctx.rect(px - 4, py - 4, 8, 8); break;
        case 'rax': ctx.rect(px - 5, py - 3.5, 10, 7); break;
        case 'fort': ctx.moveTo(px, py - 9); ctx.lineTo(px + 9, py); ctx.lineTo(px, py + 9); ctx.lineTo(px - 9, py); ctx.closePath(); break;
        default: ctx.arc(px, py, 5, 0, Math.PI * 2);
      }
      ctx.fill(); ctx.stroke();
      if (!alive) {
        ctx.strokeStyle = '#9ca3af';
        ctx.beginPath(); ctx.moveTo(px - 4, py - 4); ctx.lineTo(px + 4, py + 4); ctx.stroke();
      }
    }
  }

  function drawRoshan(ctx, idx) {
    const r = M.roshan;
    if (!r || r.alive[idx.i] !== 1) return;
    const p = posAt(r.x, r.y, idx);
    if (!p) return;
    const [px, py] = project(p.x, p.y);
    ctx.beginPath(); ctx.arc(px, py, 9, 0, Math.PI * 2);
    ctx.fillStyle = '#8B5A2B'; ctx.fill();
    ctx.lineWidth = 2; ctx.strokeStyle = '#F3C623'; ctx.stroke();
    ctx.fillStyle = '#fff'; ctx.font = 'bold 10px ' + getComputedStyle(document.body).fontFamily;
    ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
    ctx.fillText('R', px, py + 0.5);
  }

  function drawCouriers(ctx, idx) {
    for (const c of M.couriers) {
      if (c.alive[idx.i] !== 1) continue;
      const p = posAt(c.x, c.y, idx);
      if (!p) continue;
      const [px, py] = project(p.x, p.y);
      const owner = c.owner !== ABSENT ? playerById(c.owner) : null;
      ctx.beginPath(); ctx.arc(px, py, 3.5, 0, Math.PI * 2);
      ctx.fillStyle = '#f5f5f5'; ctx.fill();
      ctx.lineWidth = 1.5; ctx.strokeStyle = owner ? owner.color : (TEAM_COLOR[c.team] || '#ccc'); ctx.stroke();
    }
  }

  function drawDeathMarks(ctx, tick) {
    const window_ = DEATH_MARK_SECONDS * tickRate();
    for (const k of M.kills) {
      if (k.tick > tick || k.tick < tick - window_ || k.x === ABSENT) continue;
      const age = (tick - k.tick) / window_;
      const alpha = 1 - age * 0.85;
      const victim = playerById(k.victim);
      const [px, py] = project(k.x, k.y);
      ctx.save();
      ctx.globalAlpha = alpha;
      ctx.lineCap = 'round';
      ctx.lineWidth = 4; ctx.strokeStyle = '#0b0f14';
      ctx.beginPath(); ctx.moveTo(px - 6, py - 6); ctx.lineTo(px + 6, py + 6); ctx.moveTo(px + 6, py - 6); ctx.lineTo(px - 6, py + 6); ctx.stroke();
      ctx.lineWidth = 2; ctx.strokeStyle = victim ? victim.color : '#fff';
      ctx.beginPath(); ctx.moveTo(px - 6, py - 6); ctx.lineTo(px + 6, py + 6); ctx.moveTo(px + 6, py - 6); ctx.lineTo(px - 6, py + 6); ctx.stroke();
      ctx.restore();
    }
  }

  function drawTrails(ctx, tick, idx) {
    const span = state.trailSeconds * tickRate();
    const start = idxFor(Math.max(firstTick(), tick - span)).i;
    ctx.lineCap = 'round'; ctx.lineJoin = 'round';
    for (const p of M.players) {
      if (p.alive[idx.i] !== 1) continue;
      const pts = [];
      let prev = null;
      for (let i = start; i <= idx.i; i++) {
        const x = p.x[i], y = p.y[i];
        if (x === ABSENT || p.alive[i] !== 1) { prev = null; pts.push(null); continue; }
        if (prev && Math.hypot(x - prev[0], y - prev[1]) > TELEPORT_UNITS) pts.push(null);
        prev = [x, y];
        pts.push(prev);
      }
      const cur = posAt(p.x, p.y, idx);
      if (cur) {
        if (prev && Math.hypot(cur.x - prev[0], cur.y - prev[1]) > TELEPORT_UNITS) pts.push(null);
        pts.push([cur.x, cur.y]);
      }
      const emphasized = state.selected === null || state.selected === p.id;
      ctx.strokeStyle = p.color;
      ctx.lineWidth = emphasized ? 2 : 1;
      let seg = [];
      const flush = () => {
        if (seg.length < 2) { seg = []; return; }
        for (let i = 1; i < seg.length; i++) {
          ctx.globalAlpha = (emphasized ? 0.75 : 0.25) * (0.15 + 0.85 * i / seg.length);
          ctx.beginPath();
          ctx.moveTo(...project(seg[i - 1][0], seg[i - 1][1]));
          ctx.lineTo(...project(seg[i][0], seg[i][1]));
          ctx.stroke();
        }
        seg = [];
      };
      for (const pt of pts) { if (pt) seg.push(pt); else flush(); }
      flush();
      ctx.globalAlpha = 1;
    }
  }

  function heroInitials(npc) {
    const parts = heroShort(npc).split('_');
    return parts.length > 1 ? (parts[0][0] + parts[1][0]).toUpperCase() : heroShort(npc).slice(0, 2).toUpperCase();
  }

  function drawHeroes(ctx, idx) {
    const r = 12;
    const font = getComputedStyle(document.body).fontFamily;
    for (const p of M.players) {
      if (p.alive[idx.i] !== 1) continue;
      const pos = posAt(p.x, p.y, idx);
      if (!pos) continue;
      const [px, py] = project(pos.x, pos.y);
      const selected = state.selected === p.id;
      const icon = img(heroIconURL(p.hero));

      ctx.save();
      ctx.beginPath(); ctx.arc(px, py, r, 0, Math.PI * 2); ctx.closePath();
      ctx.fillStyle = p.color; ctx.fill();
      if (icon) {
        ctx.clip();
        ctx.drawImage(icon, px - r, py - r, r * 2, r * 2);
      } else {
        ctx.fillStyle = '#0b0f14'; ctx.font = `bold 10px ${font}`; ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
        ctx.fillText(heroInitials(p.hero), px, py + 0.5);
      }
      ctx.restore();

      ctx.beginPath(); ctx.arc(px, py, r, 0, Math.PI * 2);
      ctx.lineWidth = selected ? 3 : 2; ctx.strokeStyle = p.color; ctx.stroke();
      if (selected) { ctx.beginPath(); ctx.arc(px, py, r + 3, 0, Math.PI * 2); ctx.lineWidth = 1.5; ctx.strokeStyle = '#fff'; ctx.stroke(); }

      // Health and mana bars, as under the in-game portraits.
      const hp = p.hp[idx.i], mana = p.mana ? p.mana[idx.i] : ABSENT;
      const w = r * 2, hgt = 3, x0 = px - r;
      let y0 = py + r + 2;
      if (hp != null && hp !== ABSENT) {
        ctx.fillStyle = 'rgba(0,0,0,0.7)'; ctx.fillRect(x0, y0, w, hgt);
        ctx.fillStyle = hp > 50 ? '#3fb950' : hp > 25 ? '#d29922' : '#f85149';
        ctx.fillRect(x0, y0, w * hp / 100, hgt);
        y0 += hgt + 1;
      }
      if (mana != null && mana !== ABSENT) {
        ctx.fillStyle = 'rgba(0,0,0,0.7)'; ctx.fillRect(x0, y0, w, hgt);
        ctx.fillStyle = '#3b8bff';
        ctx.fillRect(x0, y0, w * mana / 100, hgt);
        y0 += hgt + 1;
      }

      if (state.layers.names) {
        ctx.font = `600 10px ${font}`; ctx.textAlign = 'center'; ctx.textBaseline = 'top';
        ctx.lineWidth = 3; ctx.strokeStyle = 'rgba(0,0,0,0.8)'; ctx.fillStyle = '#eef2f7';
        const label = heroName(p.hero);
        ctx.strokeText(label, px, y0 + 1); ctx.fillText(label, px, y0 + 1);
      }
    }
  }

  function heroAtPoint(mx, my, idx) {
    let best = null, bestD = 16;
    for (const p of M.players) {
      if (p.alive[idx.i] !== 1) continue;
      const pos = posAt(p.x, p.y, idx);
      if (!pos) continue;
      const [px, py] = project(pos.x, pos.y);
      const d = Math.hypot(px - mx, py - my);
      if (d < bestD) { best = p; bestD = d; }
    }
    return best;
  }

  function drawMapTooltip(idx) {
    const tip = $('map-tooltip');
    if (!state.hover) { tip.hidden = true; return; }
    const p = heroAtPoint(state.hover.x, state.hover.y, idx);
    if (!p) { tip.hidden = true; return; }
    const i = idx.i;
    clear(tip).append(
      h('div', null, h('b', { text: heroName(p.hero) }), ' ', h('span', { class: 'sub', text: p.name })),
      h('div', { class: 'sub', text: `lvl ${p.level[i]} · ${p.hp[i]}% hp${p.mana && p.mana[i] >= 0 ? ` · ${p.mana[i]}% mana` : ''} · ${p.kills[i]}/${p.deaths[i]}/${p.assists[i]} · ${fmtNum(p.netWorth[i])} gold` }));
    tip.hidden = false;
    tip.style.left = (state.hover.x + 14) + 'px';
    tip.style.top = (state.hover.y + 14) + 'px';
  }

  // ---- timeline ----------------------------------------------------------------

  function drawTimeline() {
    const c = els.timeline;
    const dpr = window.devicePixelRatio || 1;
    const w = c.clientWidth, hgt = c.clientHeight;
    if (c.width !== Math.round(w * dpr)) { c.width = Math.round(w * dpr); c.height = Math.round(hgt * dpr); }
    const ctx = c.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, hgt);
    const t0 = firstTick(), t1 = lastTick();
    const xOf = t => (t - t0) / (t1 - t0) * w;

    ctx.fillStyle = '#1c2330';
    ctx.fillRect(0, hgt / 2 - 4, w, 8);

    // Ten-minute labels and the horn.
    const lastClock = clockAt(t1);
    ctx.fillStyle = '#8b96a8'; ctx.font = '10px ' + getComputedStyle(document.body).fontFamily; ctx.textAlign = 'center'; ctx.textBaseline = 'top';
    if (lastClock != null) {
      for (let m = 0; m <= lastClock / 60; m += 10) {
        const x = xOf(tickForClock(m * 60));
        ctx.fillStyle = m === 0 ? '#eef2f7' : '#3b4656';
        ctx.fillRect(x, hgt / 2 - 8, 1, 16);
        ctx.fillStyle = '#8b96a8';
        ctx.fillText(`${m}:00`, x, hgt / 2 + 9);
      }
    }
    for (const k of M.kills) {
      const victim = playerById(k.victim);
      ctx.fillStyle = victim ? (victim.team === 2 ? CHART_COLOR[3] : CHART_COLOR[2]) : '#ccc';
      ctx.fillRect(xOf(k.tick), 2, 1.5, 9);
    }
    ctx.fillStyle = '#F3C623';
    for (const t of roshanDeaths) ctx.fillRect(xOf(t) - 1.5, 1, 3, 5);

    const x = xOf(state.tick);
    ctx.fillStyle = '#5cc8ff';
    ctx.fillRect(x - 1, 0, 2, hgt);
    ctx.beginPath(); ctx.arc(x, hgt / 2, 5, 0, Math.PI * 2); ctx.fill();
  }

  function seekToTimelineX(clientX) {
    const rect = els.timeline.getBoundingClientRect();
    const f = Math.min(1, Math.max(0, (clientX - rect.left) / rect.width));
    setTick(firstTick() + f * (lastTick() - firstTick()));
  }

  // ---- charts ------------------------------------------------------------------

  function chartGeometry(canvas) {
    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth, hgt = canvas.clientHeight;
    if (canvas.width !== Math.round(w * dpr)) { canvas.width = Math.round(w * dpr); canvas.height = Math.round(hgt * dpr); }
    const pad = { l: 44, r: 10, t: 14, b: 18 };
    return { w, h: hgt, pad, pw: w - pad.l - pad.r, ph: hgt - pad.t - pad.b, dpr };
  }

  function chartDomain() {
    let c0 = null, c1 = null;
    for (let i = 0; i < M.clock.length; i++) {
      if (M.clock[i] === CLOCK_UNKNOWN) continue;
      if (c0 === null) c0 = M.clock[i];
      c1 = M.clock[i];
    }
    return c0 === null ? null : { c0, c1 };
  }

  function niceMax(v) {
    if (v <= 0) return 1000;
    const p = Math.pow(10, Math.floor(Math.log10(v)));
    for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
    return 10 * p;
  }

  function drawChart(canvas, values, unit) {
    const g = chartGeometry(canvas);
    const ctx = canvas.getContext('2d');
    ctx.setTransform(g.dpr, 0, 0, g.dpr, 0, 0);
    ctx.clearRect(0, 0, g.w, g.h);
    const dom = chartDomain();
    if (!dom) return;
    const font = getComputedStyle(document.body).fontFamily;

    let maxAbs = 0;
    for (let i = 0; i < values.length; i++) if (values[i] != null && M.clock[i] !== CLOCK_UNKNOWN) maxAbs = Math.max(maxAbs, Math.abs(values[i]));
    const yMax = niceMax(maxAbs);
    const xOf = c => g.pad.l + (c - dom.c0) / (dom.c1 - dom.c0 || 1) * g.pw;
    const yOf = v => g.pad.t + g.ph / 2 - v / yMax * (g.ph / 2);
    const y0 = yOf(0);

    // Grid: recessive, zero line neutral.
    ctx.lineWidth = 1;
    ctx.strokeStyle = '#242c3a';
    for (const v of [yMax, -yMax, yMax / 2, -yMax / 2]) { ctx.beginPath(); ctx.moveTo(g.pad.l, yOf(v)); ctx.lineTo(g.w - g.pad.r, yOf(v)); ctx.stroke(); }
    ctx.strokeStyle = '#5b6474';
    ctx.beginPath(); ctx.moveTo(g.pad.l, y0); ctx.lineTo(g.w - g.pad.r, y0); ctx.stroke();

    ctx.fillStyle = '#8b96a8'; ctx.font = `10px ${font}`; ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
    ctx.fillText('+' + fmtK(yMax), g.pad.l - 6, yOf(yMax));
    ctx.fillText('0', g.pad.l - 6, y0);
    ctx.fillText('−' + fmtK(yMax), g.pad.l - 6, yOf(-yMax));
    ctx.textAlign = 'center'; ctx.textBaseline = 'top';
    const step = dom.c1 - dom.c0 > 50 * 60 ? 20 : 10;
    for (let m = Math.ceil(dom.c0 / 60 / step) * step; m * 60 <= dom.c1; m += step) {
      ctx.fillText(`${m}:00`, xOf(m * 60), g.h - g.pad.b + 4);
    }
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    ctx.fillText('Radiant ahead', g.pad.l + 4, g.pad.t - 12);
    ctx.textBaseline = 'bottom';
    ctx.fillText('Dire ahead', g.pad.l + 4, g.h - g.pad.b - 2);

    // Line and fills, split at the zero line so each side wears its team hue.
    const path = new Path2D();
    let started = false;
    for (let i = 0; i < values.length; i++) {
      if (values[i] == null || M.clock[i] === CLOCK_UNKNOWN) continue;
      const x = xOf(M.clock[i]), y = yOf(values[i]);
      if (!started) { path.moveTo(x, y); started = true; } else path.lineTo(x, y);
    }
    const fillPath = new Path2D(path);
    fillPath.lineTo(xOf(dom.c1), y0); fillPath.lineTo(xOf(dom.c0), y0); fillPath.closePath();
    for (const [team, clipTop, clipH] of [[2, g.pad.t - 2, y0 - g.pad.t + 2], [3, y0, g.h - g.pad.b - y0 + 2]]) {
      ctx.save();
      ctx.beginPath(); ctx.rect(g.pad.l, clipTop, g.pw, clipH); ctx.clip();
      ctx.globalAlpha = 0.22; ctx.fillStyle = CHART_COLOR[team]; ctx.fill(fillPath);
      ctx.globalAlpha = 1; ctx.lineWidth = 2; ctx.lineJoin = 'round'; ctx.strokeStyle = CHART_COLOR[team]; ctx.stroke(path);
      ctx.restore();
    }

    // Kill ticks along the bottom axis, coloured by the team that scored.
    for (const k of M.kills) {
      if (k.clock === CLOCK_UNKNOWN) continue;
      const victim = playerById(k.victim);
      ctx.fillStyle = victim ? CHART_COLOR[victim.team === 2 ? 3 : 2] : '#ccc';
      ctx.fillRect(xOf(k.clock), g.h - g.pad.b - 4, 1, 4);
    }

    // Playhead.
    const now = clockAt(state.tick);
    if (now != null && now >= dom.c0 && now <= dom.c1) {
      ctx.strokeStyle = '#5cc8ff'; ctx.lineWidth = 1.5;
      ctx.beginPath(); ctx.moveTo(xOf(now), g.pad.t - 2); ctx.lineTo(xOf(now), g.h - g.pad.b); ctx.stroke();
    }

    // Hover crosshair.
    if (state.chartHover && state.chartHover.canvas === canvas) {
      const c = state.chartHover.clock;
      const i = nearestKnownIndex(c);
      if (i >= 0 && values[i] != null) {
        const x = xOf(M.clock[i]), y = yOf(values[i]);
        ctx.strokeStyle = 'rgba(255,255,255,0.35)'; ctx.lineWidth = 1;
        ctx.beginPath(); ctx.moveTo(x, g.pad.t); ctx.lineTo(x, g.h - g.pad.b); ctx.stroke();
        ctx.beginPath(); ctx.arc(x, y, 4, 0, Math.PI * 2);
        ctx.fillStyle = values[i] >= 0 ? CHART_COLOR[2] : CHART_COLOR[3]; ctx.fill();
        ctx.lineWidth = 2; ctx.strokeStyle = '#0f1216'; ctx.stroke();
        const tip = $('chart-tooltip');
        const lead = values[i] === 0 ? 'even' : `${values[i] > 0 ? 'Radiant' : 'Dire'} +${fmtNum(Math.abs(values[i]))} ${unit}`;
        clear(tip).append(h('div', null, h('b', { text: fmtClock(M.clock[i]) }), ' ', h('span', { class: 'sub', text: lead })));
        tip.hidden = false;
        const rect = canvas.getBoundingClientRect(), host = $('side').getBoundingClientRect();
        tip.style.left = Math.min(rect.left - host.left + x + 12, host.width - 170) + 'px';
        tip.style.top = (rect.top - host.top + y - 30) + 'px';
      }
    }
  }

  function nearestKnownIndex(clock) {
    let best = -1, bestD = Infinity;
    for (let i = 0; i < M.clock.length; i++) {
      if (M.clock[i] === CLOCK_UNKNOWN) continue;
      const d = Math.abs(M.clock[i] - clock);
      if (d < bestD) { best = i; bestD = d; }
    }
    return best;
  }

  function drawCharts() {
    if (state.sideCollapsed) return;
    if (state.panel !== 'score') return;
    drawChart(els.chartGold, series.gold, 'gold');
    drawChart(els.chartXp, series.xp, 'XP');
    if (!state.chartHover) $('chart-tooltip').hidden = true;
  }

  function chartClockAt(canvas, clientX) {
    const g = chartGeometry(canvas);
    const dom = chartDomain();
    if (!dom) return null;
    const rect = canvas.getBoundingClientRect();
    const f = Math.min(1, Math.max(0, (clientX - rect.left - g.pad.l) / g.pw));
    return dom.c0 + f * (dom.c1 - dom.c0);
  }

  // ---- scoreboard --------------------------------------------------------------

  const scoreRows = new Map();

  function buildScoreboard() {
    const panel = clear($('score-tables'));
    for (const team of [2, 3]) {
      const table = h('table', { class: 'score' });
      table.append(h('caption', null, h('span', { class: team === 2 ? 'team-radiant' : 'team-dire', text: TEAM_NAME[team] }), h('span', { class: 'muted', id: `team-nw-${team}` })));
      table.append(h('thead', null, h('tr', null, ['hero', 'lvl', 'K/D/A', 'net worth', 'items'].map((t, i) => h('th', { class: i > 0 && i < 4 ? 'num' : '', text: t })))));
      const tbody = h('tbody');
      for (const p of M.players.filter(p => p.team === team)) {
        const cells = {
          level: h('td', { class: 'num' }),
          kda: h('td', { class: 'num' }),
          nw: h('td', { class: 'num' }),
          items: h('td', null, h('div', { class: 'items' })),
          dead: h('span', { class: 'dead-tag' }),
        };
        const heroImg = h('img', { class: 'hero-icon', src: heroIconURL(p.hero), alt: '' });
        heroImg.addEventListener('error', () => { heroImg.replaceWith(h('span', { class: 'hero-icon' })); });
        const tr = h('tr', { class: 'player', onclick: () => { state.selected = state.selected === p.id ? null : p.id; refreshSelection(); } },
          h('td', null, h('div', { class: 'hero-cell' },
            h('span', { class: 'color-chip', style: `background:${p.color}` }),
            heroImg,
            h('div', { class: 'names' }, h('span', null, heroName(p.hero), cells.dead), h('span', { class: 'player', text: p.name, title: p.name })))),
          cells.level, cells.kda, cells.nw, cells.items);
        tbody.append(tr);
        scoreRows.set(p.id, { tr, cells, itemCells: [], lastItems: null });
        const wrap = cells.items.firstChild;
        for (let s = 0; s < 11; s++) {
          if (s === 6 || s === 9) wrap.append(h('span', { class: 'gap' }));
          const cell = h('img', { alt: '' });
          cell.addEventListener('error', () => { cell.dataset.failed = '1'; cell.style.visibility = 'hidden'; });
          wrap.append(cell);
          scoreRows.get(p.id).itemCells.push(cell);
        }
      }
      table.append(tbody);
      panel.append(table);
    }
  }

  function updateScoreboard(i) {
    const teamNW = { 2: 0, 3: 0 };
    for (const p of M.players) {
      const row = scoreRows.get(p.id);
      if (!row) continue;
      const lvl = p.level[i], k = p.kills[i], d = p.deaths[i], a = p.assists[i], nw = p.netWorth[i];
      row.cells.level.textContent = lvl === ABSENT ? '–' : lvl;
      row.cells.kda.textContent = k === ABSENT ? '–' : `${k}/${d}/${a}`;
      row.cells.nw.textContent = nw === ABSENT ? '–' : fmtNum(nw);
      if (nw !== ABSENT) teamNW[p.team] += nw;
      const dead = p.alive[i] === 0;
      row.tr.classList.toggle('dead', dead);
      row.cells.dead.textContent = dead ? 'dead' : '';
      row.tr.classList.toggle('selected', state.selected === p.id);
      const items = p.items[i];
      const key = items ? items.join(',') : '';
      if (key !== row.lastItems) {
        row.lastItems = key;
        row.itemCells.forEach((cell, s) => {
          const id = items ? items[s] : ABSENT;
          const name = id != null && id !== ABSENT ? M.itemNames[id] : null;
          const url = itemIconURL(name);
          if (!url) { cell.style.visibility = 'hidden'; cell.removeAttribute('src'); cell.title = ''; return; }
          if (cell.dataset.url !== url) { cell.dataset.url = url; cell.dataset.failed = ''; cell.src = url; }
          cell.style.visibility = cell.dataset.failed ? 'hidden' : '';
          cell.title = itemLabel(name);
        });
      }
    }
    for (const team of [2, 3]) {
      const el = $(`team-nw-${team}`);
      if (el) el.textContent = `${fmtNum(teamNW[team])} gold`;
    }
  }

  function refreshSelection() {
    for (const p of M.players) {
      const row = scoreRows.get(p.id);
      if (row) row.tr.classList.toggle('selected', state.selected === p.id);
    }
    if (state.selected == null) state.view.follow = false;
    updateMapControls();
  }

  // ---- feed --------------------------------------------------------------------

  function feedItem(entry) {
    const t = h('span', { class: 't', text: entry.clock === CLOCK_UNKNOWN ? '' : fmtClock(entry.clock) });
    if (entry.kind === 'kill') {
      const k = entry.k;
      const victim = playerById(k.victim), killer = k.killer !== ABSENT ? playerById(k.killer) : null;
      const who = killer
        ? h('span', { class: 'who', style: `color:${killer.color}`, text: heroName(killer.hero) })
        : h('span', { class: 'who muted', text: unitName(k.killerName) });
      const assists = k.assists.length ? h('span', { class: 'muted', text: ` +${k.assists.length} assist${k.assists.length > 1 ? 's' : ''}` }) : null;
      return h('li', { class: 'kill' }, t, h('span', null, who, h('span', { class: 'arrow', text: '▸' }),
        h('span', { class: 'who', style: victim ? `color:${victim.color}` : '', text: victim ? heroName(victim.hero) : '?' }), assists));
    }
    if (entry.kind === 'chat') {
      const c = entry.c;
      const p = c.player !== ABSENT ? playerById(c.player) : null;
      const name = p ? p.name : (c.name || 'spectator');
      return h('li', { class: 'chat' }, t, h('span', null,
        h('span', { class: 'channel', text: /team/i.test(c.channel) ? '[team]' : '' }),
        h('span', { class: 'who', style: p ? `color:${p.color}` : '', text: name }), ': ', c.text));
    }
    const e = entry.e;
    const names = e.players.map(id => playerById(id)).filter(Boolean).map(p => heroName(p.hero)).join(', ');
    return h('li', { class: 'event' }, t, h('span', null, eventName(e.type), names ? ` — ${names}` : ''));
  }

  function updateFeed(force) {
    const list = $('feed');
    let count = 0;
    while (count < feedEntries.length && feedEntries[count].tick <= state.tick) count++;
    if (force || count < state.feedCount) {
      clear(list);
      state.feedCount = 0;
    }
    if (count === state.feedCount) return;
    const frag = document.createDocumentFragment();
    for (let i = state.feedCount; i < count; i++) frag.append(feedItem(feedEntries[i]));
    list.append(frag);
    state.feedCount = count;
    if (state.panel === 'feed') list.parentElement.scrollTop = list.parentElement.scrollHeight;
  }

  // ---- farming -----------------------------------------------------------------

  const FARM_KIND_COLOR = { lane: '#c98500', neutral: '#199e70', ancient: '#3987e5', deny: '#d95926', roshan: '#F3C623' };
  const CS_KINDS = [['lane', 'Lane creeps'], ['neutral', 'Neutrals'], ['ancient', 'Ancients'], ['deny', 'Denies']];
  const GOLD_SOURCES = [
    ['passive', 'Passive income', '#008300', p => p.incomeGold],
    ['creep', 'Creeps', '#c98500', p => p.creepGold],
    ['neutral', 'Neutrals', '#3987e5', p => p.neutralGold],
    ['hero', 'Heroes', '#d95926', p => p.heroGold],
    ['building', 'Buildings', '#199e70', p => p.buildingGold],
    ['other', 'Other', '#9085e9', null],
  ];
  const REGIONS = [
    ['top', 'Top lane', '#3987e5'], ['mid', 'Mid lane', '#d95926'], ['bot', 'Bot lane', '#199e70'],
    ['own', 'Own jungle', '#c98500'], ['enemy', 'Enemy jungle', '#9085e9'], ['base', 'Base', '#6b7280'],
  ];
  const CONSUMABLES = new Set([
    'item_tango', 'item_tango_single', 'item_clarity', 'item_flask', 'item_enchanted_mango', 'item_tpscroll',
    'item_ward_observer', 'item_ward_sentry', 'item_ward_dispenser', 'item_dust', 'item_smoke_of_deceit',
    'item_faerie_fire', 'item_blood_grenade', 'item_bottle', 'item_courier', 'item_flying_courier',
    'item_famango', 'item_great_famango', 'item_greater_famango', 'item_cheese', 'item_aegis',
    'item_tome_of_knowledge', 'item_infused_raindrop', 'item_refresher_shard',
  ]);
  const FARM_WINDOW_SECONDS = 30;

  const farmCache = new Map();
  let heatCache = { key: '', canvas: null };

  // Lane paths in normalised map coordinates (0..1, y up), inset from the
  // edges: top runs up the left edge then along the top, bottom along the
  // bottom then up the right edge, mid along the diagonal.
  const LANE_INSET = 0.12;
  const LANE_PATHS = {
    top: [[LANE_INSET, LANE_INSET], [LANE_INSET, 1 - LANE_INSET], [1 - LANE_INSET, 1 - LANE_INSET]],
    mid: [[LANE_INSET, LANE_INSET], [1 - LANE_INSET, 1 - LANE_INSET]],
    bot: [[LANE_INSET, LANE_INSET], [1 - LANE_INSET, LANE_INSET], [1 - LANE_INSET, 1 - LANE_INSET]],
  };

  function distToPath(u, v, path) {
    let best = Infinity;
    for (let k = 1; k < path.length; k++) {
      const [ax, ay] = path[k - 1], [bx, by] = path[k];
      const dx = bx - ax, dy = by - ay;
      const t = Math.max(0, Math.min(1, ((u - ax) * dx + (v - ay) * dy) / (dx * dx + dy * dy)));
      best = Math.min(best, Math.hypot(u - (ax + t * dx), v - (ay + t * dy)));
    }
    return best;
  }

  // Classifies where a creep kill happened. Neutral camps are jungle by
  // definition, split by which side of the river they sit on; lane creep
  // kills go to the nearest lane path, or the base when inside one.
  function regionOf(e, team) {
    if (e.x === ABSENT) return 'unknown';
    const s = M.match.map.size;
    const u = (e.x - MAP_CENTER) / s + 0.5, v = (e.y - MAP_CENTER) / s + 0.5;
    const radiantSide = u + v < 1;
    const own = (team === 2) === radiantSide;
    if (e.kind === 'neutral' || e.kind === 'ancient' || e.kind === 'roshan') return own ? 'own' : 'enemy';
    if (Math.hypot(u - 0.1, v - 0.1) < 0.13 || Math.hypot(u - 0.9, v - 0.9) < 0.13) return 'base';
    let best = 'mid', bestD = Infinity;
    for (const [lane, path] of Object.entries(LANE_PATHS)) {
      const dd = distToPath(u, v, path);
      if (dd < bestD) { best = lane; bestD = dd; }
    }
    return best;
  }

  function farmGoldAt(p, i) {
    const c = p.creepGold[i];
    if (c == null || c === ABSENT) return -1;
    const nn = p.neutralGold[i];
    return c + (nn == null || nn === ABSENT ? 0 : nn);
  }

  // Per-player derived farming data, computed once.
  function farmDerived(pid) {
    if (farmCache.has(pid)) return farmCache.get(pid);
    const p = playerById(pid);
    const n = M.ticks.length;
    const events = M.farmEvents.filter(e => e.player === pid).map(e => ({ ...e, region: regionOf(e, p.team) }));
    const purchases = M.purchases.filter(u => u.player === pid && !CONSUMABLES.has(u.item) && !u.item.startsWith('item_recipe_'));

    // Prefix counts per kind and region over the tick-sorted events.
    const prefix = {};
    for (const [k] of CS_KINDS) prefix[k] = new Int32Array(events.length + 1);
    prefix.roshan = new Int32Array(events.length + 1);
    for (const [r] of REGIONS) prefix['r:' + r] = new Int32Array(events.length + 1);
    prefix['r:unknown'] = new Int32Array(events.length + 1);
    for (let k = 0; k < events.length; k++) {
      for (const key of Object.keys(prefix)) prefix[key][k + 1] = prefix[key][k];
      prefix[events[k].kind][k + 1]++;
      prefix['r:' + events[k].region][k + 1]++;
    }

    // Per-minute creep score bins.
    const bins = [];
    for (const e of events) {
      if (e.clock === CLOCK_UNKNOWN) continue;
      const m = Math.max(0, Math.floor(e.clock / 60));
      while (bins.length <= m) bins.push({ lane: 0, neutral: 0, ancient: 0, deny: 0, roshan: 0 });
      bins[m][e.kind]++;
    }

    // Cumulative alive / farming / dead sample counts from the horn.
    const hornIdx = idxFor(tickForClock(0)).i;
    const aliveCum = new Int32Array(n), farmCum = new Int32Array(n), deadCum = new Int32Array(n);
    const window_ = FARM_WINDOW_SECONDS * tickRate();
    for (let i = 0; i < n; i++) {
      let a = 0, f = 0, d = 0;
      if (i >= hornIdx) {
        if (p.alive[i] === 1) {
          a = 1;
          const g = farmGoldAt(p, i), g0 = farmGoldAt(p, idxFor(M.ticks[i] - window_).i);
          if (g >= 0 && g0 >= 0 && g > g0) f = 1;
        } else if (p.alive[i] === 0) d = 1;
      }
      aliveCum[i] = (i ? aliveCum[i - 1] : 0) + a;
      farmCum[i] = (i ? farmCum[i - 1] : 0) + f;
      deadCum[i] = (i ? deadCum[i - 1] : 0) + d;
    }

    const d = { p, events, purchases, prefix, bins, hornIdx, aliveCum, farmCum, deadCum };
    farmCache.set(pid, d);
    return d;
  }

  // Number of events at or before tick (events are tick-sorted).
  function eventsUpTo(events, tick) {
    let lo = 0, hi = events.length;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (events[mid].tick <= tick) lo = mid + 1; else hi = mid; }
    return lo;
  }

  function farmMetrics(p, i) {
    const clock = M.clock[i];
    const minutes = clock !== CLOCK_UNKNOWN && clock > 0 ? clock / 60 : 0;
    const val = (arr) => (arr[i] == null || arr[i] === ABSENT ? null : arr[i]);
    const earned = val(p.earnedGold), xp = val(p.xp);
    return {
      gpm: minutes > 0 && earned != null ? earned / minutes : null,
      xpm: minutes > 0 && xp != null ? xp / minutes : null,
      lh: val(p.lastHits), denies: val(p.denies), nw: val(p.netWorth), stacks: val(p.stacks), earned,
    };
  }

  function uptimeAt(d, i) {
    const alive = d.aliveCum[i], farm = d.farmCum[i], dead = d.deadCum[i];
    return { uptime: alive > 0 ? farm / alive : null, dead: alive + dead > 0 ? dead / (alive + dead) : null };
  }

  // 1-based rank of pid among players by a metric (higher is better).
  function rankOf(pid, i, metric) {
    const vals = M.players.map(p => ({ id: p.id, v: metric(p, i) })).filter(x => x.v != null).sort((a, b) => b.v - a.v);
    const k = vals.findIndex(x => x.id === pid);
    return k < 0 ? null : { rank: k + 1, of: vals.length };
  }

  // The Farming and Itemization tabs share one selected hero.
  function setFarmPlayer(pid) {
    state.farmPlayer = pid;
    state.selected = pid;
    refreshSelection();
    for (const b of document.querySelectorAll('.hero-picker button')) b.classList.toggle('active', parseInt(b.dataset.id, 10) === pid);
    const i = idxFor(state.tick).i;
    updateFarmPanel(i, true);
    updateItemPanel(i);
  }

  function buildFarmPicker() {
    for (const id of ['farm-heroes', 'item-heroes']) {
      const wrap = clear($(id));
      for (const p of M.players) {
        const im = h('img', { src: heroIconURL(p.hero), alt: '', title: `${heroName(p.hero)} · ${p.name}` });
        im.addEventListener('error', () => { im.replaceWith(h('span', { class: 'hero-icon', text: heroInitials(p.hero) })); });
        wrap.append(h('button', { type: 'button', 'data-id': p.id, title: `${heroName(p.hero)} · ${p.name}`, onclick: () => setFarmPlayer(p.id) },
          im, h('span', { class: 'bar', style: `background:${p.color}` })));
      }
    }
    if (state.farmPlayer == null && M.players.length) {
      state.farmPlayer = state.selected != null ? state.selected : M.players[0].id;
    }
    for (const b of document.querySelectorAll('.hero-picker button')) b.classList.toggle('active', parseInt(b.dataset.id, 10) === state.farmPlayer);
    legend($('legend-cs'), CS_KINDS.map(([k, label]) => [label, FARM_KIND_COLOR[k]]));
    legend($('legend-gpm'), [['GPM', '#c98500'], ['XPM', '#3987e5']]);
  }

  function updateItemPanel(i) {
    if (state.panel !== 'item' || state.farmPlayer == null) return;
    const d = farmDerived(state.farmPlayer);
    clear($('item-title')).append(h('span', { text: heroName(d.p.hero) }), h('span', { class: 'muted', text: `${d.p.name} · ${TEAM_NAME[d.p.team]} · at ${fmtClock(clockAt(state.tick))}` }));
    renderReference(d, i, M.ticks[i]);
  }

  function legend(el, items) {
    clear(el).append(...items.map(([label, color]) => h('span', { style: `--c:${color}`, text: label })));
  }

  const fmtPct = x => (x == null ? '–' : Math.round(x * 100) + '%');
  const fmtRate = x => (x == null ? '–' : fmtNum(Math.round(x)));

  function tile(label, value, rank) {
    const r = rank ? h('span', { class: 'r' + (rank.rank === 1 ? ' top' : ''), text: `#${rank.rank}/${rank.of}` }) : null;
    return h('div', { class: 'tile' }, h('div', { class: 'v' }, value, r), h('div', { class: 'l', text: label }));
  }

  function updateFarmPanel(i, force) {
    if (state.panel !== 'farm' || state.farmPlayer == null) return;
    const d = farmDerived(state.farmPlayer);
    const p = d.p;
    const tick = M.ticks[i];
    const m = farmMetrics(p, i);
    const up = uptimeAt(d, i);
    const upTo = eventsUpTo(d.events, tick);
    const neutrals = d.prefix.neutral[upTo] + d.prefix.ancient[upTo];

    clear($('farm-title')).append(h('span', { text: heroName(p.hero) }), h('span', { class: 'muted', text: `${p.name} · ${TEAM_NAME[p.team]} · at ${fmtClock(clockAt(state.tick))}` }));

    let lhLabel = 'last hits';
    const idx10 = M.clock[i] !== CLOCK_UNKNOWN && M.clock[i] >= 600 ? idxFor(tickForClock(600)).i : -1;
    if (idx10 >= 0 && p.lastHits[idx10] >= 0) lhLabel = `last hits · ${p.lastHits[idx10]} @10`;

    clear($('farm-tiles')).append(
      tile('gold per minute', fmtRate(m.gpm), rankOf(p.id, i, (q, j) => farmMetrics(q, j).gpm)),
      tile('XP per minute', fmtRate(m.xpm), rankOf(p.id, i, (q, j) => farmMetrics(q, j).xpm)),
      tile(lhLabel, m.lh == null ? '–' : fmtNum(m.lh), rankOf(p.id, i, (q, j) => farmMetrics(q, j).lh)),
      tile('denies', m.denies == null ? '–' : fmtNum(m.denies), null),
      tile('neutral kills', fmtNum(neutrals), rankOf(p.id, i, (q, j) => { const e = farmDerived(q.id); const k = eventsUpTo(e.events, M.ticks[j]); return e.prefix.neutral[k] + e.prefix.ancient[k]; })),
      tile('camps stacked', m.stacks == null ? '–' : fmtNum(m.stacks), null),
      tile('farm uptime', fmtPct(up.uptime), rankOf(p.id, i, (q, j) => uptimeAt(farmDerived(q.id), j).uptime)),
      tile('time dead', fmtPct(up.dead), null));

    renderSplit(d, upTo);
    drawFarmCharts(i);
    if (force || !$('farm-items').childElementCount) renderItemStrip(d);
    updateItemStrip(d, tick);
    renderCompare(i);
  }

  function renderSplit(d, upTo) {
    const bar = clear($('farm-split'));
    const total = upTo - d.prefix['r:unknown'][upTo];
    const items = [];
    for (const [key, label, color] of REGIONS) {
      const count = d.prefix['r:' + key][upTo];
      if (!count) continue;
      const share = count / Math.max(1, total);
      bar.append(h('div', { style: `flex:${count};background:${color}`, title: `${label}: ${count} (${Math.round(share * 100)}%)` }));
      items.push([`${label} ${Math.round(share * 100)}%`, color]);
    }
    if (!items.length) bar.append(h('div', { class: 'muted small', style: 'padding: 0 6px; line-height: 14px', text: 'no creep kills yet' }));
    legend($('farm-split-legend'), items);
  }

  // Common frame for the farming charts: x is the game clock from the horn.
  function farmChartFrame(canvas, yMax, yLabel) {
    const g = chartGeometry(canvas);
    const ctx = canvas.getContext('2d');
    ctx.setTransform(g.dpr, 0, 0, g.dpr, 0, 0);
    ctx.clearRect(0, 0, g.w, g.h);
    const dom = chartDomain();
    if (!dom) return null;
    const c0 = Math.max(0, dom.c0), c1 = Math.max(c0 + 60, dom.c1);
    const font = getComputedStyle(document.body).fontFamily;
    const xOf = c => g.pad.l + (c - c0) / (c1 - c0) * g.pw;
    const yOf = v => g.pad.t + g.ph - v / yMax * g.ph;
    ctx.lineWidth = 1; ctx.strokeStyle = '#242c3a';
    for (const f of [0.25, 0.5, 0.75, 1]) { ctx.beginPath(); ctx.moveTo(g.pad.l, yOf(yMax * f)); ctx.lineTo(g.w - g.pad.r, yOf(yMax * f)); ctx.stroke(); }
    ctx.strokeStyle = '#5b6474'; ctx.beginPath(); ctx.moveTo(g.pad.l, yOf(0)); ctx.lineTo(g.w - g.pad.r, yOf(0)); ctx.stroke();
    ctx.fillStyle = '#8b96a8'; ctx.font = `10px ${font}`; ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
    ctx.fillText(yLabel(yMax), g.pad.l - 6, yOf(yMax)); ctx.fillText(yLabel(yMax / 2), g.pad.l - 6, yOf(yMax / 2)); ctx.fillText('0', g.pad.l - 6, yOf(0));
    ctx.textAlign = 'center'; ctx.textBaseline = 'top';
    const step = c1 - c0 > 50 * 60 ? 20 : 10;
    for (let mm = 0; mm * 60 <= c1; mm += step) ctx.fillText(`${mm}:00`, xOf(mm * 60), g.h - g.pad.b + 4);
    const now = clockAt(state.tick);
    const playhead = () => {
      if (now == null || now < c0 || now > c1) return;
      ctx.strokeStyle = '#5cc8ff'; ctx.lineWidth = 1.5; ctx.beginPath(); ctx.moveTo(xOf(now), g.pad.t - 2); ctx.lineTo(xOf(now), g.h - g.pad.b); ctx.stroke();
    };
    return { g, ctx, c0, c1, xOf, yOf, font, playhead };
  }

  function farmTooltip(canvas, x, y, lines) {
    const tip = $('chart-tooltip');
    clear(tip).append(...lines.map((l, i) => h('div', { class: i ? 'sub' : '' }, i ? l : h('b', { text: l }))));
    tip.hidden = false;
    const rect = canvas.getBoundingClientRect(), host = $('side').getBoundingClientRect();
    tip.style.left = Math.min(rect.left - host.left + x + 12, host.width - 190) + 'px';
    tip.style.top = (rect.top - host.top + y - 10 + $('side').scrollTop) + 'px';
  }

  function drawFarmCharts(i) {
    if (state.sideCollapsed) return;
    if (state.panel !== 'farm' || state.farmPlayer == null) return;
    const d = farmDerived(state.farmPlayer);
    drawCSChart($('chart-cs'), d);
    drawRateChart($('chart-gpm'), d);
    drawGoldSourceChart($('chart-gold-src'), d);
    if (!state.chartHover) $('chart-tooltip').hidden = true;
  }

  function drawCSChart(canvas, d) {
    let maxCount = 1;
    for (const b of d.bins) maxCount = Math.max(maxCount, b.lane + b.neutral + b.ancient + b.deny);
    const yMax = Math.ceil(maxCount / 5) * 5;
    const f = farmChartFrame(canvas, yMax, v => String(Math.round(v)));
    if (!f) return;
    const { ctx, xOf, yOf } = f;
    const minutes = Math.floor(f.c1 / 60) + 1;
    const bw = Math.max(1, (xOf(60) - xOf(0)) - 2);
    const hover = state.chartHover && state.chartHover.canvas === canvas ? Math.floor(state.chartHover.clock / 60) : -1;
    for (let mm = 0; mm < Math.min(minutes, d.bins.length); mm++) {
      const b = d.bins[mm];
      let y = yOf(0);
      const x = xOf(mm * 60) + 1;
      for (const [k] of CS_KINDS) {
        if (!b[k]) continue;
        const top = yOf((yOf(0) - y) / (yOf(0) - yOf(1)) + b[k]);
        ctx.fillStyle = FARM_KIND_COLOR[k];
        ctx.globalAlpha = hover >= 0 && hover !== mm ? 0.55 : 1;
        ctx.fillRect(x, top, bw, y - top - 1);
        y = top;
      }
    }
    ctx.globalAlpha = 1;
    f.playhead();
    if (hover >= 0 && hover < d.bins.length) {
      const b = d.bins[hover];
      farmTooltip(canvas, xOf(hover * 60 + 30), f.g.pad.t, [`${hover}:00 – ${hover + 1}:00`, `lane ${b.lane} · neutral ${b.neutral} · ancient ${b.ancient} · denies ${b.deny}`]);
    }
  }

  function drawRateChart(canvas, d) {
    const p = d.p;
    const pts = [];
    for (let i = 0; i < M.ticks.length; i++) {
      const c = M.clock[i];
      if (c === CLOCK_UNKNOWN || c < 60) continue;
      const m = farmMetrics(p, i);
      if (m.gpm == null && m.xpm == null) continue;
      pts.push({ c, gpm: m.gpm, xpm: m.xpm, i });
    }
    let maxV = 100;
    for (const q of pts) maxV = Math.max(maxV, q.gpm || 0, q.xpm || 0);
    const yMax = niceMax(maxV);
    const f = farmChartFrame(canvas, yMax, fmtK);
    if (!f) return;
    const { ctx, xOf, yOf } = f;
    for (const [key, color] of [['gpm', '#c98500'], ['xpm', '#3987e5']]) {
      ctx.beginPath();
      let started = false;
      for (const q of pts) { if (q[key] == null) continue; const x = xOf(q.c), y = yOf(q[key]); if (!started) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y); }
      ctx.lineWidth = 2; ctx.lineJoin = 'round'; ctx.strokeStyle = color; ctx.stroke();
      const last = [...pts].reverse().find(q => q[key] != null);
      if (last) { ctx.fillStyle = '#d7dde6'; ctx.font = `600 10px ${f.font}`; ctx.textAlign = 'right'; ctx.textBaseline = 'bottom'; ctx.fillText(key.toUpperCase(), xOf(last.c), yOf(last[key]) - 3); }
    }
    f.playhead();
    if (state.chartHover && state.chartHover.canvas === canvas && pts.length) {
      const c = state.chartHover.clock;
      let best = pts[0];
      for (const q of pts) if (Math.abs(q.c - c) < Math.abs(best.c - c)) best = q;
      ctx.strokeStyle = 'rgba(255,255,255,0.35)'; ctx.lineWidth = 1; ctx.beginPath(); ctx.moveTo(xOf(best.c), f.g.pad.t); ctx.lineTo(xOf(best.c), f.g.h - f.g.pad.b); ctx.stroke();
      farmTooltip(canvas, xOf(best.c), f.g.pad.t, [fmtClock(best.c), `GPM ${fmtRate(best.gpm)} · XPM ${fmtRate(best.xpm)}`]);
    }
  }

  function goldSources(p, i) {
    const out = {};
    let known = 0;
    for (const [key, , , get] of GOLD_SOURCES) {
      if (!get) continue;
      const v = get(p)[i];
      out[key] = v == null || v === ABSENT ? null : v;
      if (out[key] != null) known += out[key];
    }
    const earned = p.earnedGold[i];
    out.other = earned == null || earned === ABSENT ? null : Math.max(0, earned - known);
    return out;
  }

  function drawGoldSourceChart(canvas, d) {
    const p = d.p;
    const n = M.ticks.length;
    let maxV = 100, last = null;
    for (let i = n - 1; i >= 0; i--) { const e = p.earnedGold[i]; if (e != null && e !== ABSENT) { maxV = Math.max(maxV, e); last = i; break; } }
    const yMax = niceMax(maxV);
    const f = farmChartFrame(canvas, yMax, fmtK);
    if (!f) return;
    const { ctx, xOf, yOf } = f;
    const active = GOLD_SOURCES.filter(([key]) => { const s = goldSources(p, last == null ? n - 1 : last); return s[key] != null && (key !== 'other' || s.other > 0); });
    const samples = [];
    for (let i = 0; i < n; i++) { const c = M.clock[i]; if (c === CLOCK_UNKNOWN || c < 0) continue; samples.push({ c, src: goldSources(p, i), i }); }
    let base = samples.map(() => 0);
    for (const [key, , color] of active) {
      const path = new Path2D();
      const tops = samples.map((s, k) => base[k] + (s.src[key] || 0));
      path.moveTo(xOf(samples[0].c), yOf(base[0]));
      samples.forEach((s, k) => path.lineTo(xOf(s.c), yOf(tops[k])));
      for (let k = samples.length - 1; k >= 0; k--) path.lineTo(xOf(samples[k].c), yOf(base[k]));
      path.closePath();
      ctx.fillStyle = color; ctx.globalAlpha = 0.85; ctx.fill(path);
      ctx.globalAlpha = 1; ctx.strokeStyle = '#0f1216'; ctx.lineWidth = 1;
      ctx.beginPath(); samples.forEach((s, k) => k ? ctx.lineTo(xOf(s.c), yOf(tops[k])) : ctx.moveTo(xOf(s.c), yOf(tops[k]))); ctx.stroke();
      base = tops;
    }
    legend($('legend-gold-src'), active.map(([, label, color]) => [label, color]));
    f.playhead();
    if (state.chartHover && state.chartHover.canvas === canvas && samples.length) {
      const c = state.chartHover.clock;
      let best = samples[0];
      for (const s of samples) if (Math.abs(s.c - c) < Math.abs(best.c - c)) best = s;
      ctx.strokeStyle = 'rgba(255,255,255,0.35)'; ctx.lineWidth = 1; ctx.beginPath(); ctx.moveTo(xOf(best.c), f.g.pad.t); ctx.lineTo(xOf(best.c), f.g.h - f.g.pad.b); ctx.stroke();
      farmTooltip(canvas, xOf(best.c), f.g.pad.t, [fmtClock(best.c), ...active.map(([key, label]) => `${label} ${fmtNum(best.src[key] || 0)}`)]);
    }
  }

  function renderItemStrip(d) {
    const strip = clear($('farm-items'));
    const dom = chartDomain();
    if (!dom || !d.purchases.length) { strip.append(h('span', { class: 'muted small', text: 'no item purchases recorded' })); return; }
    const c0 = 0, c1 = Math.max(60, dom.c1);
    const xPct = c => Math.min(100, Math.max(0, (c - c0) / (c1 - c0) * 100));
    const rowsEnd = [];
    const width = strip.clientWidth || 400;
    for (const u of d.purchases) {
      if (u.clock === CLOCK_UNKNOWN) continue;
      const x = xPct(u.clock) / 100 * width;
      let row = rowsEnd.findIndex(end => end + 24 <= x);
      if (row < 0) { row = rowsEnd.length; rowsEnd.push(0); }
      rowsEnd[row] = x;
      const im = h('img', { src: itemIconURL(u.item), alt: '', title: `${fmtClock(u.clock)} ${itemLabel(u.item)}`, style: `left:${xPct(u.clock)}%; top:${(row % 3) * 20}px` });
      im.dataset.tick = u.tick;
      im.addEventListener('error', () => { im.style.visibility = 'hidden'; });
      strip.append(im);
    }
    const step = c1 > 50 * 60 ? 20 : 10;
    for (let mm = 0; mm * 60 <= c1; mm += step) strip.append(h('span', { class: 'axis-label', style: `left:${xPct(mm * 60)}%`, text: `${mm}:00` }));
    strip.append(h('div', { class: 'playhead', id: 'farm-items-playhead' }));
  }

  function updateItemStrip(d, tick) {
    for (const im of $('farm-items').querySelectorAll('img')) im.classList.toggle('future', parseInt(im.dataset.tick, 10) > tick);
    const dom = chartDomain();
    const ph = $('farm-items-playhead');
    const now = clockAt(state.tick);
    if (ph && dom && now != null) ph.style.left = Math.min(100, Math.max(0, now / Math.max(60, dom.c1) * 100)) + '%';
    const list = clear($('farm-item-list'));
    const bought = d.purchases.filter(u => u.tick <= tick).slice(-12).reverse();
    for (const u of bought) list.append(h('li', null, h('span', { class: 't', text: fmtClock(u.clock) }), h('span', { text: itemLabel(u.item) })));
  }

  function renderCompare(i) {
    const table = clear($('farm-compare'));
    table.append(h('thead', null, h('tr', null,
      h('th', { text: 'hero' }), h('th', { class: 'num', text: 'GPM' }), h('th', { class: 'num', text: 'XPM' }),
      h('th', { class: 'num', text: 'LH' }), h('th', { class: 'num', text: 'DN' }), h('th', { class: 'num', text: 'net worth' }), h('th', { class: 'num', text: 'farm %' }))));
    const rows = M.players.map(p => ({ p, m: farmMetrics(p, i), up: uptimeAt(farmDerived(p.id), i) }));
    rows.sort((a, b) => (b.m.gpm || 0) - (a.m.gpm || 0));
    const tbody = h('tbody');
    for (const { p, m, up } of rows) {
      tbody.append(h('tr', { class: 'player' + (p.id === state.farmPlayer ? ' me' : ''), onclick: () => setFarmPlayer(p.id) },
        h('td', null, h('div', { class: 'hero-cell' }, h('span', { class: 'color-chip', style: `background:${p.color}` }), h('span', { text: heroName(p.hero) }))),
        h('td', { class: 'num', text: fmtRate(m.gpm) }), h('td', { class: 'num', text: fmtRate(m.xpm) }),
        h('td', { class: 'num', text: m.lh == null ? '–' : m.lh }), h('td', { class: 'num', text: m.denies == null ? '–' : m.denies }),
        h('td', { class: 'num', text: m.nw == null ? '–' : fmtNum(m.nw) }), h('td', { class: 'num', text: fmtPct(up.uptime) })));
    }
    table.append(tbody);
  }

  // ---- OpenDota reference ----------------------------------------------------------

  // Game phases and cost filters exactly as OpenDota's itemPopularity buckets
  // them (svc/util/queries.ts): start <= 0:00 and <= 600 gold, early < 10:00
  // and >= 500, mid < 25:00 and >= 1000, late >= 25:00 and >= 2000.
  const PHASES = [
    ['start', 'Start', 'before 0:00 · items up to 600 gold', c => c <= 0, cost => cost <= 600],
    ['early', 'Early', '0:00 – 10:00 · 500+ gold', c => c > 0 && c < 600, cost => cost >= 500],
    ['mid', 'Mid', '10:00 – 25:00 · 1000+ gold', c => c >= 600 && c < 1500, cost => cost >= 1000],
    ['late', 'Late', 'after 25:00 · 2000+ gold', c => c >= 1500, cost => cost >= 2000],
  ];
  // Timing scenario thresholds (svc/util/scenariosUtil.ts): a purchase is
  // filed under the first threshold at or after its time.
  const TIMING_THRESHOLDS = [7.5, 10, 12, 15, 20, 25, 30].map(m => m * 60);
  const BENCH_STATS = [
    ['gold_per_min', 'GPM', (p, i, mins) => farmMetrics(p, i).gpm, v => fmtRate(v)],
    ['xp_per_min', 'XPM', (p, i, mins) => farmMetrics(p, i).xpm, v => fmtRate(v)],
    ['last_hits_per_min', 'last hits / min', (p, i, mins) => { const lh = farmMetrics(p, i).lh; return lh == null || mins <= 0 ? null : lh / mins; }, v => v.toFixed(1)],
    ['denies_per_min', 'denies / min', (p, i, mins) => { const dn = farmMetrics(p, i).denies; return dn == null || mins <= 0 ? null : dn / mins; }, v => v.toFixed(2)],
  ];
  const PRO_TOP_N = 8;

  function pollReference() {
    fetch('api/reference').then(r => r.json()).then(ref => {
      if (ref.pending) { setTimeout(pollReference, 4000); return; }
      M.reference = ref.disabled ? { disabled: true } : ref;
      updateItemPanel(idxFor(state.tick).i);
    }).catch(() => setTimeout(pollReference, 8000));
  }

  const shortItem = name => (name || '').replace(/^item_/, '');
  function itemDisplay(short) {
    const it = M.reference && M.reference.items && M.reference.items[short];
    return it && it.name ? it.name : itemLabel('item_' + short);
  }

  // Percentile of value on a benchmark curve, interpolated between points.
  function percentileOf(curve, value) {
    if (!curve || !curve.length || value == null) return null;
    const pts = [...curve].sort((a, b) => a.percentile - b.percentile);
    if (value <= pts[0].value) return pts[0].percentile * value / Math.max(1e-9, pts[0].value);
    for (let k = 1; k < pts.length; k++) {
      if (value <= pts[k].value) {
        const a = pts[k - 1], b = pts[k];
        return a.percentile + (b.percentile - a.percentile) * (value - a.value) / Math.max(1e-9, b.value - a.value);
      }
    }
    return Math.min(0.999, pts[pts.length - 1].percentile + 0.005);
  }
  const ordinal = n => { const s = ['th', 'st', 'nd', 'rd'], v = n % 100; return n + (s[(v - 20) % 10] || s[v] || s[0]); };

  function renderReference(d, i, tick) {
    const p = d.p;
    const note = $('farm-ref-note');
    const ref = M.reference;
    const bench = clear($('farm-bench')), phases = clear($('farm-phases')), timings = clear($('farm-timings'));
    if (!ref) { note.textContent = 'loading reference data…'; return; }
    if (ref.disabled) { note.textContent = 'disabled (run without -opendota=false)'; return; }
    const hr = ref.heroes && ref.heroes[String(p.heroId)];
    if (!hr) { note.textContent = `no OpenDota data for hero ${p.heroId}`; return; }
    note.textContent = `${heroName(p.hero)} in recent matches · fetched ${ref.fetchedAt.slice(0, 10)}${ref.errors && ref.errors.length ? ` · ${ref.errors.length} request(s) failed` : ''}`;

    // Benchmarks: the player's rate at the playhead against the hero's percentile curve.
    if (hr.benchmarks) {
      const clock = M.clock[i];
      const mins = clock !== CLOCK_UNKNOWN && clock > 0 ? clock / 60 : 0;
      const grid = h('div', { class: 'bench' });
      for (const [key, label, get, fmt] of BENCH_STATS) {
        const curve = hr.benchmarks[key];
        const value = get(p, i, mins);
        if (!curve || value == null) continue;
        const pct = percentileOf(curve, value);
        const median = curve.find(c => Math.abs(c.percentile - 0.5) < 1e-6);
        const bar = h('div', { class: 'bar' }, h('span', { class: 'median', style: 'left:50%', title: median ? `median ${fmt(median.value)}` : '' }), h('span', { class: 'me', style: `left:${Math.round(pct * 100)}%` }));
        grid.append(h('span', { class: 'l', text: label }), bar,
          h('span', { class: 'v' }, fmt(value), h('span', { class: 'muted', text: `${ordinal(Math.round(pct * 100))} pct${median ? ` · median ${fmt(median.value)}` : ''}` })));
      }
      bench.append(grid, h('p', { class: 'muted small', style: 'margin:4px 0 0', text: 'Percentile of this hero\'s per-minute rates across recent OpenDota matches; the marker is the current value, the line the median.' }));
    }

    // Build vs pro: per phase, the popular items and whether this player bought them then.
    if (hr.popularity) {
      const games = Math.max(1, ...Object.values(hr.popularity.start || {}), ...Object.values(hr.popularity.early || {}));
      const costOf = name => { const it = ref.items && ref.items[name]; return it && it.cost != null ? it.cost : null; };
      for (const [key, label, range, inPhase, costOK] of PHASES) {
        const pro = Object.entries(hr.popularity[key] || {}).filter(([n]) => !n.startsWith('#') && !n.startsWith('recipe_')).sort((a, b) => b[1] - a[1]);
        const top = pro.slice(0, PRO_TOP_N);
        const topCount = top.length ? top[0][1] : 1;
        const mine = M.purchases.filter(u => u.player === p.id && u.clock !== CLOCK_UNKNOWN && inPhase(u.clock) && !u.item.startsWith('item_recipe_'))
          .filter(u => { const c = costOf(shortItem(u.item)); return c == null || costOK(c); });
        const mineSet = new Map();
        for (const u of mine) if (!mineSet.has(shortItem(u.item))) mineSet.set(shortItem(u.item), u);
        let weightHit = 0, weightAll = 0;
        for (const [name, n] of top) { weightAll += n; if (mineSet.has(name)) weightHit += n; }
        const col = h('div', { class: 'phase' });
        col.append(h('h4', null, label, h('span', { class: 'muted', text: range })));
        if (top.length) col.append(h('div', { class: 'match' }, 'matches ', h('b', { text: `${Math.round(100 * weightHit / Math.max(1, weightAll))}%` }), ` of the popular build (~${games} pro games)`));
        const list = h('ul');
        for (const [name, n] of top) {
          const u = mineSet.get(name);
          const rel = Math.round(100 * n / topCount);
          const li = h('li', {
            class: (u ? 'bought' : '') + (u && u.tick > tick ? ' future' : ''),
            style: `background: linear-gradient(90deg, rgba(92,200,255,0.12) ${rel}%, transparent ${rel}%)`,
            title: `${itemDisplay(name)} · ${n} buys in ~${games} pro games (${(n / games).toFixed(1)} per game)${u ? ` · you: ${fmtClock(u.clock)}` : ''}`,
          },
            h('span', { class: 'tick', text: u ? '✓' : '' }), h('img', { src: itemIconURL('item_' + name), alt: '' }),
            h('span', { class: 'clip', text: itemDisplay(name) }), h('span', { class: 'pct', text: `${(n / games).toFixed(1)}/game` }));
          li.querySelector('img').addEventListener('error', e => { e.target.style.visibility = 'hidden'; });
          list.append(li);
        }
        col.append(list);
        const also = [...mineSet.values()].filter(u => !top.some(([name]) => name === shortItem(u.item)));
        if (also.length) {
          const chips = h('div', { class: 'chips' });
          for (const u of also) {
            const im = h('img', { src: itemIconURL(u.item), alt: '', title: `${itemDisplay(shortItem(u.item))} · ${fmtClock(u.clock)}`, class: u.tick > tick ? 'future' : '' });
            im.addEventListener('error', () => { im.style.visibility = 'hidden'; });
            chips.append(im);
          }
          col.append(h('div', { class: 'also' }, 'also bought', chips));
        }
        phases.append(col);
      }
    }

    // Core item timings: where this purchase falls among games on this hero.
    if (hr.timings) {
      const shown = [];
      for (const u of d.purchases) {
        const name = shortItem(u.item);
        const buckets = hr.timings[name];
        if (!buckets || shown.includes(name) || u.clock === CLOCK_UNKNOWN) continue;
        shown.push(name);
        const total = buckets.reduce((a, b) => a + b.games, 0);
        if (!total) continue;
        let later = 0, mineBucket = null;
        for (const b of buckets) {
          const k = TIMING_THRESHOLDS.indexOf(b.time);
          const lower = k > 0 ? TIMING_THRESHOLDS[k - 1] : 0;
          if (u.clock <= lower) later += b.games;
          else if (u.clock <= b.time) { later += b.games / 2; mineBucket = b; }
        }
        const earlierThan = Math.round(100 * later / total);
        const maxGames = Math.max(...buckets.map(b => b.games));
        const wrap = h('div', { class: 'buckets' });
        for (const b of buckets) {
          const wr = b.games ? Math.round(100 * b.wins / b.games) : 0;
          wrap.append(h('div', { class: 'bucket' + (b === mineBucket ? ' mine' : ''), title: `bought by ${fmtClock(b.time)}: ${b.games} games, ${wr}% won` },
            h('div', { class: 'fill', style: `height:${Math.max(2, Math.round(100 * b.games / maxGames))}%` }),
            h('span', { class: 'lbl', text: `${b.time / 60}′ ${wr}%` })));
        }
        const im = h('img', { src: itemIconURL(u.item), alt: '' });
        im.addEventListener('error', () => { im.style.visibility = 'hidden'; });
        const verdict = mineBucket
          ? h('div', { class: 'verdict' }, 'earlier than ', h('b', { text: `${earlierThan}%` }), ` of ${total} games · win rate when bought by ${fmtClock(mineBucket.time)}: `, h('b', { text: `${Math.round(100 * mineBucket.wins / Math.max(1, mineBucket.games))}%` }))
          : h('div', { class: 'verdict' }, u.clock > TIMING_THRESHOLDS[TIMING_THRESHOLDS.length - 1] ? `bought after 30:00, later than all ${total} tracked games` : `earlier than ${earlierThan}% of ${total} games`);
        timings.append(h('div', { class: 'timing' + (u.tick > tick ? ' future' : '') }, im,
          h('div', null, h('div', { class: 'head' }, h('b', { text: itemDisplay(name) }), h('span', { class: 'muted', text: `you: ${fmtClock(u.clock)}` })),
            h('div', { class: 'buckets-wrap' }, wrap), verdict)));
      }
      if (!shown.length) timings.append(h('p', { class: 'muted small', text: 'no core items (1400+ gold) bought yet, or no timing data for them' }));
    } else {
      timings.append(h('p', { class: 'muted small', text: 'timing scenarios not fetched (run with -timings)' }));
    }
  }

  // Heatmap of the selected hero's creep kills so far, plus a dot per kill.
  function drawFarmHeat(ctx, tick, idx) {
    if (state.farmPlayer == null) return;
    const d = farmDerived(state.farmPlayer);
    const dpr = window.devicePixelRatio || 1;
    const key = `${state.farmPlayer}:${idx.i}:${mapSize}`;
    if (heatCache.key !== key) {
      const c = document.createElement('canvas');
      c.width = c.height = Math.round(mapSize * dpr);
      const g = c.getContext('2d');
      g.setTransform(dpr, 0, 0, dpr, 0, 0);
      const r = Math.max(10, mapSize * 0.035);
      for (const e of d.events) {
        if (e.tick > tick) break;
        if (e.x === ABSENT) continue;
        const [px, py] = baseProject(e.x, e.y);
        const grad = g.createRadialGradient(px, py, 0, px, py, r);
        grad.addColorStop(0, 'rgba(255, 150, 40, 0.28)');
        grad.addColorStop(1, 'rgba(255, 150, 40, 0)');
        g.fillStyle = grad;
        g.fillRect(px - r, py - r, 2 * r, 2 * r);
      }
      heatCache = { key, canvas: c };
    }
    const [ox, oy, k] = mapRect();
    ctx.drawImage(heatCache.canvas, ox, oy, k, k);
    for (const e of d.events) {
      if (e.tick > tick) break;
      if (e.x === ABSENT) continue;
      const [px, py] = project(e.x, e.y);
      ctx.beginPath(); ctx.arc(px, py, 2.5, 0, Math.PI * 2);
      ctx.fillStyle = FARM_KIND_COLOR[e.kind] || '#fff'; ctx.fill();
      ctx.lineWidth = 1; ctx.strokeStyle = 'rgba(0,0,0,0.6)'; ctx.stroke();
    }
  }

  // ---- item purchase alerts ------------------------------------------------------

  const ITEM_ALERT_MS = 4500;       // real time a toast stays up
  const KILL_ALERT_MS = 5500;
  const ITEM_ALERT_MAX = 6;
  const KILL_ALERT_MAX = 3;
  const ITEM_ALERT_MIN_COST = 500;  // when OpenDota item costs are known
  const ITEM_ALERT_SEEK_SECONDS = 60; // a jump larger than this is a seek, not playback
  let alerts = [];
  let killAlerts = [];
  let lastAlertTick = null;

  function notablePurchase(u) {
    if (CONSUMABLES.has(u.item) || u.item.startsWith('item_recipe_')) return false;
    const it = M.reference && M.reference.items && M.reference.items[shortItem(u.item)];
    return !it || it.cost == null || it.cost >= ITEM_ALERT_MIN_COST;
  }

  function clearAlerts() {
    for (const a of alerts) a.el.remove();
    for (const a of killAlerts) a.el.remove();
    alerts = [];
    killAlerts = [];
  }

  // A kill notice in the style of the in-game banner: killer, skull, victim.
  function pushKillAlert(k, now) {
    const victim = playerById(k.victim);
    if (!victim) return;
    const killer = k.killer !== ABSENT ? playerById(k.killer) : null;
    const side = (p, name, teamColor, cls) => {
      const wrap = h('div', { class: `side ${cls}`, style: `--team:${teamColor}` });
      if (p) {
        const im = h('img', { src: heroLandscapeURL(p.hero), alt: '' });
        im.addEventListener('error', () => { im.replaceWith(h('div', { class: 'unit', text: heroInitials(p.hero) })); });
        wrap.append(im, h('span', { class: 'name', text: heroName(p.hero) }));
      } else {
        wrap.append(h('div', { class: 'unit', text: name }), h('span', { class: 'name', text: name }));
      }
      return wrap;
    };
    const killerTeam = killer ? TEAM_COLOR[killer.team] : (victim.team === 2 ? TEAM_COLOR[3] : TEAM_COLOR[2]);
    const isFirst = M.kills.length && M.kills[0] === k;
    const metaParts = [];
    if (isFirst) metaParts.push('FIRST BLOOD');
    if (k.assists && k.assists.length) metaParts.push(`+${k.assists.length} assist${k.assists.length > 1 ? 's' : ''}`);
    metaParts.push(fmtClock(k.clock));
    const el = h('div', { class: 'kill-alert' + (isFirst ? ' first' : '') },
      side(killer, unitName(k.killerName), killerTeam, 'killer'),
      h('span', { class: 'skull', text: '☠' }),
      side(victim, null, TEAM_COLOR[victim.team], 'victim'),
      h('span', { class: 'meta', text: metaParts.join(' · ') }));
    $('kill-alerts').append(el);
    killAlerts.push({ k, el, until: now + KILL_ALERT_MS });
    while (killAlerts.length > KILL_ALERT_MAX) killAlerts.shift().el.remove();
  }

  function pushAlert(u, now) {
    const p = playerById(u.player);
    if (!p) return;
    const hero = h('img', { class: 'hero', src: heroIconURL(p.hero), alt: '' });
    hero.addEventListener('error', () => { hero.style.visibility = 'hidden'; });
    const item = h('img', { class: 'item', src: itemIconURL(u.item), alt: '' });
    item.addEventListener('error', () => { item.style.visibility = 'hidden'; });
    const el = h('div', { class: 'alert', style: `border-left-color:${p.color}` }, hero,
      h('span', { class: 'text' }, h('span', { class: 'who', style: `color:${p.color}`, text: heroName(p.hero) }),
        h('span', { class: 'what', text: `acquired ${itemDisplay(shortItem(u.item))} · ${fmtClock(u.clock)}` })), item);
    const wrap = $('alerts');
    wrap.prepend(el);
    alerts.push({ u, el, player: u.player, until: now + ITEM_ALERT_MS });
    while (alerts.length > ITEM_ALERT_MAX) alerts.shift().el.remove();
  }

  // Fires toasts for purchases the playhead has just crossed; a seek resets.
  function updateItemAlerts(now) {
    if (!state.layers.alerts) { if (alerts.length) clearAlerts(); lastAlertTick = state.tick; return; }
    const seekTicks = ITEM_ALERT_SEEK_SECONDS * tickRate();
    if (lastAlertTick == null || state.tick < lastAlertTick || state.tick - lastAlertTick > seekTicks) {
      clearAlerts();
      lastAlertTick = state.tick;
    }
    if (state.tick > lastAlertTick) {
      for (const u of M.purchases) {
        if (u.tick > lastAlertTick && u.tick <= state.tick && notablePurchase(u)) pushAlert(u, now);
      }
      for (const k of M.kills) {
        if (k.tick > lastAlertTick && k.tick <= state.tick) pushKillAlert(k, now);
      }
      lastAlertTick = state.tick;
    }
    for (const list of [alerts, killAlerts]) {
      for (const a of list) {
        if (a.until - now < 300) a.el.classList.add('fading');
        if (a.until <= now) a.el.remove();
      }
    }
    alerts = alerts.filter(a => a.until > now);
    killAlerts = killAlerts.filter(a => a.until > now);
  }

  // Draws the acquired item over the hero's avatar while its toast is up.
  function drawItemAlerts(ctx, idx, now) {
    for (const a of alerts) {
      const p = playerById(a.player);
      if (!p || p.alive[idx.i] !== 1) continue;
      const pos = posAt(p.x, p.y, idx);
      if (!pos) continue;
      const [px, py] = project(pos.x, pos.y);
      const icon = img(itemIconURL(a.u.item));
      const remaining = Math.max(0, Math.min(1, (a.until - now) / ITEM_ALERT_MS));
      const lift = 30 + (1 - remaining) * 6;
      ctx.save();
      ctx.globalAlpha = Math.min(1, remaining * 3);
      ctx.fillStyle = '#0b0f14';
      ctx.fillRect(px - 12, py - lift - 8, 24, 16);
      if (icon) ctx.drawImage(icon, px - 11, py - lift - 7, 22, 14);
      ctx.strokeStyle = p.color; ctx.lineWidth = 1; ctx.strokeRect(px - 12, py - lift - 8, 24, 16);
      ctx.restore();
    }
  }

  // ---- playback ----------------------------------------------------------------

  function setTick(t) {
    state.tick = Math.min(lastTick(), Math.max(firstTick(), t));
  }

  function setPlaying(on) {
    state.playing = on;
    $('btn-play').textContent = on ? '❚❚' : '▶';
  }

  let lastFrame = 0;
  function frame(now) {
    const dt = lastFrame ? Math.min(0.25, (now - lastFrame) / 1000) : 0;
    lastFrame = now;
    if (state.playing) {
      setTick(state.tick + dt * state.speed * tickRate());
      if (state.tick >= lastTick()) setPlaying(false);
    }

    updateItemAlerts(now);
    drawMap(now);
    drawTimeline();

    const idx = idxFor(state.tick);
    $('clock').textContent = fmtClock(clockAt(state.tick));
    $('tick').textContent = `tick ${fmtNum(Math.round(state.tick))}`;
    updateHUD(idx);
    updateHeroCard(idx);
    if (idx.i !== state.lastPanelIdx) {
      state.lastPanelIdx = idx.i;
      updateScoreboard(idx.i);
      updateFeed(false);
      drawCharts();
      updateFarmPanel(idx.i);
      updateItemPanel(idx.i);
    } else if (state.chartHover) {
      drawCharts();
      if (state.panel === 'farm') drawFarmCharts(idx.i);
    }
    requestAnimationFrame(frame);
  }

  // ---- top-of-map HUD ------------------------------------------------------------

  const TIME_OF_DAY_CYCLE = 65535;
  const HUD_NATURAL_WIDTH = 780;
  const hudLast = { radiant: null, dire: null, clock: null, night: null, turn: null, scale: null };
  const hudPortraits = new Map();
  let hudNaturalWidth = 0; // layout width of the HUD before scaling

  function heroLandscapeURL(npc) { return `${CDN}/dota_react/heroes/${heroShort(npc)}.png`; }

  // Portraits flank the clock as in the game's top bar: Radiant left, Dire
  // right, each in team slot order with colour strip, health, mana, level and
  // respawn countdown.
  function buildHUDHeroes() {
    for (const team of [2, 3]) {
      const wrap = clear($(team === 2 ? 'hud-heroes-radiant' : 'hud-heroes-dire'));
      const players = M.players.filter(p => p.team === team).sort((a, b) => a.slot - b.slot);
      for (const p of players) {
        const im = h('img', { src: heroLandscapeURL(p.hero), alt: '', draggable: 'false' });
        im.addEventListener('error', () => { im.replaceWith(h('div', { class: 'fallback', text: heroInitials(p.hero) })); });
        const el = h('div', { class: 'portrait', title: `${heroName(p.hero)} · ${p.name}`, onclick: () => { state.selected = state.selected === p.id ? null : p.id; refreshSelection(); } },
          h('span', { class: 'strip', style: `background:${p.color}` }), im,
          h('span', { class: 'lvl' }), h('span', { class: 'respawn' }),
          h('div', { class: 'bar hp' }, h('i')), h('div', { class: 'bar mp' }, h('i')),
          h('div', { class: 'stats' }, h('span', { class: 'lh', title: 'last hits / denies' }), h('span', { class: 'nw', title: 'net worth and its rank' }), h('span', { class: 'kda', title: 'kills / deaths / assists' })));
        wrap.append(el);
        hudPortraits.set(p.id, {
          el, lvl: el.querySelector('.lvl'), respawn: el.querySelector('.respawn'), hp: el.querySelector('.bar.hp i'), mp: el.querySelector('.bar.mp i'),
          lh: el.querySelector('.lh'), nw: el.querySelector('.nw'), kda: el.querySelector('.kda'), last: {},
        });
      }
    }
    hudNaturalWidth = $('hud').offsetWidth;
  }

  // Players ranked by net worth at a sample, 1 being the richest.
  let rankCache = { i: -1, ranks: new Map() };
  function netWorthRanks(i) {
    if (rankCache.i === i) return rankCache.ranks;
    const sorted = M.players.filter(p => p.netWorth[i] != null && p.netWorth[i] !== ABSENT).sort((a, b) => b.netWorth[i] - a.netWorth[i]);
    const ranks = new Map();
    sorted.forEach((p, r) => ranks.set(p.id, r + 1));
    rankCache = { i, ranks };
    return ranks;
  }

  function updateHUDHeroes(i) {
    const ranks = netWorthRanks(i);
    for (const p of M.players) {
      const hp = hudPortraits.get(p.id);
      if (!hp) continue;
      const dead = p.alive[i] === 0;
      const level = p.level[i], health = p.hp[i], mana = p.mana ? p.mana[i] : ABSENT, respawn = p.respawn ? p.respawn[i] : ABSENT;
      const selected = state.selected === p.id;
      const L = hp.last;
      if (L.dead !== dead) { L.dead = dead; hp.el.classList.toggle('dead', dead); }
      if (L.selected !== selected) { L.selected = selected; hp.el.classList.toggle('selected', selected); }
      if (L.level !== level) { L.level = level; hp.lvl.textContent = level != null && level !== ABSENT ? level : ''; }
      const healthW = dead ? 0 : (health == null || health === ABSENT ? 100 : health);
      if (L.health !== healthW) { L.health = healthW; hp.hp.style.width = healthW + '%'; }
      const manaW = dead ? 0 : (mana == null || mana === ABSENT ? 0 : mana);
      if (L.mana !== manaW) { L.mana = manaW; hp.mp.style.width = manaW + '%'; }
      const rs = dead && respawn != null && respawn > 0 ? String(respawn) : (dead ? '☠' : '');
      if (L.respawn !== rs) { L.respawn = rs; hp.respawn.textContent = rs; }

      const stat = v => (v == null || v === ABSENT ? '–' : v);
      const lh = p.lastHits ? `${stat(p.lastHits[i])}/${stat(p.denies[i])}` : '';
      if (L.lh !== lh) { L.lh = lh; hp.lh.textContent = lh; }
      const nw = p.netWorth[i], rank = ranks.get(p.id);
      const nwTxt = nw == null || nw === ABSENT ? '' : `${fmtK(nw)} #${rank}`;
      if (L.nw !== nwTxt) { L.nw = nwTxt; clear(hp.nw); if (nwTxt) hp.nw.append(fmtK(nw), h('b', { text: `#${rank}` })); }
      const kda = p.kills[i] == null || p.kills[i] === ABSENT ? '' : `${p.kills[i]}/${p.deaths[i]}/${p.assists[i]}`;
      if (L.kda !== kda) { L.kda = kda; hp.kda.textContent = kda; }
    }
  }

  // Day/night at a sample: from the game rules' time-of-day counter when the
  // replay networks it (day is the middle half of the cycle), otherwise from
  // the clock's five-minute alternation starting with day at the horn.
  function dayNightAt(i) {
    const tod = M.timeOfDay ? M.timeOfDay[i] : ABSENT;
    if (tod != null && tod !== ABSENT) {
      const f = tod / TIME_OF_DAY_CYCLE;
      return { night: f < 0.25 || f >= 0.75, turn: f };
    }
    const clock = M.clock[i];
    if (clock === CLOCK_UNKNOWN || clock < 0) return { night: false, turn: 0.5 };
    const block = Math.floor(clock / 300);
    const within = (clock % 300) / 300;
    const night = block % 2 === 1;
    return { night, turn: night ? 0.75 + within * 0.5 - (within >= 0.5 ? 1 : 0) : 0.25 + within * 0.5 };
  }

  function updateHUD(idx) {
    const i = idx.i;
    const kills = { 2: 0, 3: 0 };
    for (const p of M.players) { const k = p.kills[i]; if (k != null && k !== ABSENT) kills[p.team] += k; }
    if (kills[2] !== hudLast.radiant) { hudLast.radiant = kills[2]; $('hud-radiant').textContent = kills[2]; }
    if (kills[3] !== hudLast.dire) { hudLast.dire = kills[3]; $('hud-dire').textContent = kills[3]; }
    const clock = fmtClock(clockAt(state.tick));
    if (clock !== hudLast.clock) { hudLast.clock = clock; $('hud-clock').textContent = clock; }
    const dn = dayNightAt(i);
    if (dn.night !== hudLast.night) {
      hudLast.night = dn.night;
      $('hud-daynight').classList.toggle('night', dn.night);
      $('hud-glyph').textContent = dn.night ? '☾' : '☀';
    }
    const turn = Math.round(((dn.turn % 1) + 1) % 1 * 100);
    if (turn !== hudLast.turn) { hudLast.turn = turn; $('hud-dial').style.setProperty('--turn', `${turn}%`); }
    updateHUDHeroes(i);
    const scale = Math.min(1, Math.round(100 * (mapW - 16) / (hudNaturalWidth || HUD_NATURAL_WIDTH)) / 100);
    if (scale !== hudLast.scale) { hudLast.scale = scale; $('hud').style.setProperty('--hud-scale', scale); }
  }

  // ---- hero card ---------------------------------------------------------------

  // The selected hero's abilities and inventory with live cooldowns and
  // charges, read from the sparse loadout events: the state at a tick is the
  // last event per slot at or before it.
  const card = { player: null, abilityKey: null, abilities: [], items: [], last: {} };
  const ITEM_GRID = { extra: [10, 9], main: [0, 1, 2, 3, 4, 5], back: [6, 7, 8] }; // indexes into items[]

  function abilityIconURL(name) { return `${CDN}/dota_react/abilities/${name}.png`; }
  function abilityLabel(name, hero) {
    const prefix = heroShort(hero) + '_';
    return (name.startsWith(prefix) ? name.slice(prefix.length) : name).replace(/_/g, ' ');
  }
  function abilitiesAt(L, tick) {
    const slots = new Map();
    for (const e of L.abilities) {
      if (e.tick > tick) break;
      if (e.name === ABSENT) slots.delete(e.slot); else slots.set(e.slot, e);
    }
    return [...slots.values()].sort((a, b) => a.slot - b.slot).map(e => ({ slot: e.slot, name: M.abilityNames[e.name], level: e.level }));
  }
  function cooldownsAt(L, tick, kind) {
    const out = new Map();
    for (const c of L.cooldowns) { if (c.tick > tick) break; if (c.kind === kind) out.set(c.slot, c); }
    return out;
  }
  function chargesAt(L, tick) {
    const out = new Map();
    for (const c of L.charges) { if (c.tick > tick) break; out.set(c.slot, c.charges); }
    return out;
  }

  // A slot: icon with a text fallback, cooldown sweep and timer, charge badge.
  function makeSlot(extraClass) {
    const im = h('img', { alt: '', draggable: 'false' });
    const slot = {
      el: h('div', { class: `slot ${extraClass || ''}` }, im, h('span', { class: 'name' }), h('div', { class: 'cd' }), h('div', { class: 'cdt' }), h('span', { class: 'charges' })),
      img: im, url: undefined, cooling: null, txt: null, frac: null, charges: null,
    };
    slot.name = slot.el.querySelector('.name'); slot.cd = slot.el.querySelector('.cd'); slot.cdt = slot.el.querySelector('.cdt'); slot.badge = slot.el.querySelector('.charges');
    im.addEventListener('error', () => { im.style.visibility = 'hidden'; slot.name.hidden = false; });
    return slot;
  }
  function setSlotIcon(slot, url, label) {
    if (slot.url === url) return;
    slot.url = url;
    slot.el.classList.toggle('empty', !url);
    slot.el.title = label || '';
    slot.name.textContent = label || '';
    slot.name.hidden = !!url;
    if (url) { slot.img.style.visibility = ''; slot.img.src = url; } else { slot.img.removeAttribute('src'); slot.img.style.visibility = 'hidden'; }
  }
  // c is the slot's latest cooldown event; the sweep shows the share left.
  function setSlotCooldown(slot, c, clock, present) {
    let remaining = 0, frac = 0;
    if (present && c && clock != null && c.end !== CLOCK_UNKNOWN) {
      remaining = c.end - clock;
      if (remaining > 0) frac = c.len > 0 ? Math.min(1, remaining / c.len) : 1;
    }
    const cooling = remaining > 0.05;
    if (slot.cooling !== cooling) { slot.cooling = cooling; slot.el.classList.toggle('cooling', cooling); }
    if (!cooling) return;
    const txt = remaining >= 10 ? String(Math.ceil(remaining)) : remaining.toFixed(1);
    if (txt !== slot.txt) { slot.txt = txt; slot.cdt.textContent = txt; }
    const f = Math.round(frac * 50) / 50;
    if (f !== slot.frac) { slot.frac = f; slot.cd.style.setProperty('--frac', f); }
  }
  function setSlotCharges(slot, n) {
    const txt = n != null && n !== ABSENT ? String(n) : '';
    if (txt !== slot.charges) { slot.charges = txt; slot.badge.textContent = txt; }
  }

  function buildHeroCard() {
    card.items = [];
    for (const [grid, indexes] of Object.entries(ITEM_GRID)) {
      const wrap = clear($(`card-items-${grid}`));
      for (const s of indexes) {
        const slot = makeSlot(s === 10 ? 'neutral' : s === 9 ? 'tp' : '');
        slot.index = s;
        wrap.append(slot.el);
        card.items.push(slot);
      }
    }
    $('card-portrait').addEventListener('error', e => { e.target.style.visibility = 'hidden'; });
  }

  function updateHeroCard(idx) {
    const el = $('hero-card');
    const p = state.selected != null ? playerById(state.selected) : null;
    if (!p) {
      if (!el.hidden) { el.hidden = true; card.player = null; }
      return;
    }
    if (card.player !== p.id) {
      card.player = p.id; card.abilityKey = null; card.last = {};
      for (const slot of card.items) slot.url = undefined;
      const portrait = $('card-portrait');
      portrait.style.visibility = ''; portrait.src = heroLandscapeURL(p.hero);
      $('card-hero').textContent = heroName(p.hero);
      $('card-player').textContent = p.name;
      el.style.setProperty('--team', p.color);
      el.hidden = false;
    }
    const i = idx.i, clock = clockAt(state.tick), L = loadouts.get(p.id), last = card.last;
    const level = p.level[i], hp = p.hp[i], mana = p.mana ? p.mana[i] : ABSENT, dead = p.alive[i] === 0;
    const lvl = level != null && level !== ABSENT ? `Lv ${level}` : '';
    if (lvl !== last.lvl) { last.lvl = lvl; $('card-level').textContent = lvl; }
    if (dead !== last.dead) { last.dead = dead; el.classList.toggle('dead', dead); }
    const hpTxt = dead ? 'dead' : (hp == null || hp === ABSENT ? '' : `${hp}% hp`);
    if (hpTxt !== last.hp) { last.hp = hpTxt; $('card-hp').style.width = (dead || hp == null || hp === ABSENT ? 0 : hp) + '%'; $('card-hp-text').textContent = hpTxt; }
    const mpTxt = dead || mana == null || mana === ABSENT ? '' : `${mana}% mana`;
    if (mpTxt !== last.mp) { last.mp = mpTxt; $('card-mp').style.width = (mpTxt ? mana : 0) + '%'; $('card-mp-text').textContent = mpTxt; }

    const abilities = L ? abilitiesAt(L, state.tick) : [];
    const key = abilities.map(a => `${a.slot}:${a.name}`).join('|');
    if (key !== card.abilityKey) {
      card.abilityKey = key;
      const wrap = clear($('card-abilities'));
      card.abilities = abilities.map(a => {
        const slot = makeSlot('');
        slot.slot = a.slot; slot.level = null;
        slot.pips = h('div', { class: 'pips' });
        setSlotIcon(slot, abilityIconURL(a.name), abilityLabel(a.name, p.hero));
        wrap.append(h('div', { class: 'ability' }, slot.el, slot.pips));
        return slot;
      });
      if (!abilities.length) wrap.append(h('div', { class: 'muted small none', text: L && L.abilities.length ? 'no abilities yet' : 'no ability data for this match' }));
    }
    const abilityCDs = L ? cooldownsAt(L, state.tick, 'ability') : new Map();
    for (const slot of card.abilities) {
      const a = abilities.find(x => x.slot === slot.slot);
      const level = a ? a.level : 0;
      if (level !== slot.level) {
        slot.level = level;
        slot.el.classList.toggle('unlearned', level === 0);
        clear(slot.pips);
        for (let k = 0; k < level; k++) slot.pips.append(h('i'));
      }
      setSlotCooldown(slot, abilityCDs.get(slot.slot), clock, level > 0);
    }

    const items = p.items[i] || [];
    const itemCDs = L ? cooldownsAt(L, state.tick, 'item') : new Map();
    const charges = L ? chargesAt(L, state.tick) : new Map();
    for (const slot of card.items) {
      const id = items[slot.index];
      const name = id != null && id !== ABSENT ? M.itemNames[id] : null;
      setSlotIcon(slot, itemIconURL(name), name ? itemLabel(name) : '');
      setSlotCooldown(slot, itemCDs.get(slot.index), clock, !!name);
      setSlotCharges(slot, name ? charges.get(slot.index) : null);
    }
  }

  // ---- sidebar -----------------------------------------------------------------

  const SIDE_KEY = 'manta-map.side';
  function setSideCollapsed(on) {
    state.sideCollapsed = on;
    $('layout').classList.toggle('side-collapsed', on);
    const b = $('side-toggle');
    b.textContent = on ? '«' : '»';
    b.title = on ? 'show the panel' : 'hide the panel';
    try { localStorage.setItem(SIDE_KEY, on ? 'collapsed' : 'open'); } catch (err) { /* storage unavailable */ }
    if (!on) requestAnimationFrame(() => { drawCharts(); if (state.panel === 'farm') drawFarmCharts(idxFor(state.tick).i); });
  }
  function readSideCollapsed() {
    try { return localStorage.getItem(SIDE_KEY) === 'collapsed'; } catch (err) { return false; }
  }

  function switchPanel(name) {
    state.panel = name;
    for (const b of document.querySelectorAll('#panel-tabs button[data-panel]')) b.classList.toggle('active', b.dataset.panel === name);
    for (const p of document.querySelectorAll('.panel')) p.hidden = p.id !== `panel-${name}`;
    if (name === 'score') drawCharts();
    if (name === 'feed') updateFeed(true);
    if (name === 'farm') updateFarmPanel(idxFor(state.tick).i, true);
    if (name === 'item') updateItemPanel(idxFor(state.tick).i);
  }

  // ---- wiring ------------------------------------------------------------------

  function wire() {
    els.map = $('map'); els.timeline = $('timeline'); els.chartGold = $('chart-gold'); els.chartXp = $('chart-xp');

    $('btn-play').addEventListener('click', () => setPlaying(!state.playing));
    $('btn-horn').addEventListener('click', () => setTick(tickForClock(0)));
    $('speed').addEventListener('change', e => { state.speed = parseFloat(e.target.value); });
    $('trail-seconds').addEventListener('change', e => { state.trailSeconds = parseFloat(e.target.value); });
    for (const cb of document.querySelectorAll('#layers input[type=checkbox]')) {
      cb.addEventListener('change', () => { state.layers[cb.dataset.layer] = cb.checked; });
    }
    for (const b of document.querySelectorAll('#panel-tabs button[data-panel]')) b.addEventListener('click', () => switchPanel(b.dataset.panel));

    const tl = els.timeline;
    tl.addEventListener('pointerdown', e => { state.dragging = 'timeline'; tl.setPointerCapture(e.pointerId); seekToTimelineX(e.clientX); });
    tl.addEventListener('pointermove', e => { if (state.dragging === 'timeline') seekToTimelineX(e.clientX); });
    tl.addEventListener('pointerup', () => { state.dragging = null; });

    // Map: click selects a hero (empty ground clears), drag pans, wheel zooms,
    // double-click follows.
    const map = els.map;
    let pan = null; // { id, x, y, moved } while the primary button is down
    const mapPoint = e => { const r = map.getBoundingClientRect(); return { x: e.clientX - r.left, y: e.clientY - r.top }; };
    map.addEventListener('pointerdown', e => {
      if (e.button !== 0) return;
      pan = { id: e.pointerId, x: e.clientX, y: e.clientY, moved: false };
      map.setPointerCapture(e.pointerId);
    });
    map.addEventListener('pointermove', e => {
      state.hover = mapPoint(e);
      if (!pan || pan.id !== e.pointerId) return;
      const dx = e.clientX - pan.x, dy = e.clientY - pan.y;
      if (!pan.moved) {
        if (Math.hypot(dx, dy) < 4) return;
        pan.moved = true;
        map.classList.add('panning');
        if (state.view.follow) setFollow(false);
      }
      const k = state.view.zoom * mapSize;
      state.view.cx -= dx / k; state.view.cy -= dy / k;
      clampView();
      pan.x = e.clientX; pan.y = e.clientY;
    });
    const endPan = e => {
      if (!pan || pan.id !== e.pointerId) return;
      const moved = pan.moved;
      pan = null;
      map.classList.remove('panning');
      if (moved || e.type !== 'pointerup') return;
      const { x, y } = mapPoint(e);
      const p = heroAtPoint(x, y, idxFor(state.tick));
      state.selected = p ? p.id : null;
      refreshSelection();
    };
    map.addEventListener('pointerup', endPan);
    map.addEventListener('pointercancel', endPan);
    map.addEventListener('pointerleave', () => { state.hover = null; });
    map.addEventListener('dblclick', e => {
      const { x, y } = mapPoint(e);
      const p = heroAtPoint(x, y, idxFor(state.tick));
      if (!p) return;
      state.selected = p.id;
      refreshSelection();
      setFollow(true);
    });
    map.addEventListener('wheel', e => {
      e.preventDefault();
      const dy = e.deltaMode === 1 ? e.deltaY * 33 : e.deltaY;
      const { x, y } = mapPoint(e);
      zoomAt(Math.pow(1.0015, -dy), x, y);
    }, { passive: false });

    $('zoom-in').addEventListener('click', () => zoomAt(ZOOM_STEP));
    $('zoom-out').addEventListener('click', () => zoomAt(1 / ZOOM_STEP));
    $('zoom-follow').addEventListener('click', () => setFollow(!state.view.follow));
    $('zoom-reset').addEventListener('click', resetView);
    $('card-close').addEventListener('click', () => { state.selected = null; refreshSelection(); });
    $('side-toggle').addEventListener('click', () => setSideCollapsed(!state.sideCollapsed));

    $('farm-heat').addEventListener('change', e => { state.farmHeat = e.target.checked; });
    for (const canvas of [els.chartGold, els.chartXp, $('chart-cs'), $('chart-gpm'), $('chart-gold-src')]) {
      canvas.addEventListener('pointermove', e => {
        const clock = chartClockAt(canvas, e.clientX);
        state.chartHover = clock == null ? null : { canvas, clock };
        if (state.dragging === 'chart') setTick(tickForClock(clock));
      });
      canvas.addEventListener('pointerleave', () => { state.chartHover = null; $('chart-tooltip').hidden = true; drawCharts(); if (state.panel === 'farm') drawFarmCharts(idxFor(state.tick).i); });
      canvas.addEventListener('pointerdown', e => { state.dragging = 'chart'; canvas.setPointerCapture(e.pointerId); const c = chartClockAt(canvas, e.clientX); if (c != null) setTick(tickForClock(c)); });
      canvas.addEventListener('pointerup', () => { state.dragging = null; });
    }

    window.addEventListener('keydown', e => {
      if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT') return;
      const step = (e.shiftKey ? 60 : 10) * tickRate();
      if (e.code === 'Space') { e.preventDefault(); setPlaying(!state.playing); }
      else if (e.code === 'ArrowRight') { e.preventDefault(); setTick(state.tick + step); }
      else if (e.code === 'ArrowLeft') { e.preventDefault(); setTick(state.tick - step); }
      else if (e.code === 'KeyF') { setFollow(!state.view.follow); }
      else if (e.code === 'Escape') { state.selected = null; refreshSelection(); }
      else if (e.code === 'Equal' || e.code === 'NumpadAdd') { zoomAt(ZOOM_STEP); }
      else if (e.code === 'Minus' || e.code === 'NumpadSubtract') { zoomAt(1 / ZOOM_STEP); }
      else if (e.code === 'Digit0') { resetView(); }
    });
    window.addEventListener('resize', () => { drawCharts(); });
  }

  async function init() {
    wire();
    const status = $('status');
    try {
      M = window.MATCH || await (await fetch('api/match')).json();
    } catch (err) {
      status.textContent = 'failed to load match data: ' + err.message;
      return;
    }
    if (!M.ticks || !M.ticks.length) { status.textContent = 'match data has no samples'; return; }
    if (!M.reference && !window.MATCH) pollReference();

    if (M.match.map.image) {
      minimapImg = new Image();
      minimapImg.src = M.match.map.image;
    }
    deriveSeries();
    renderHeader();
    buildScoreboard();
    buildFarmPicker();
    buildHUDHeroes();
    buildHeroCard();
    setSideCollapsed(readSideCollapsed());
    updateMapControls();
    setTick(tickForClock(0));
    applyHash();
    updateFeed(true);
    drawCharts();
    requestAnimationFrame(frame);
  }

  // #t=12:34&panel=graph&speed=8&play=1&select=3&follow=1 opens the viewer at a moment.
  function applyHash() {
    const params = new URLSearchParams(location.hash.replace(/^#/, ''));
    const t = params.get('t');
    if (t) {
      const m = /^(-?)(\d+):(\d{1,2})$/.exec(t);
      const seconds = m ? (m[1] ? -1 : 1) * (parseInt(m[2], 10) * 60 + parseInt(m[3], 10)) : parseFloat(t);
      if (!Number.isNaN(seconds)) setTick(tickForClock(seconds));
    }
    const panel = params.get('panel') === 'graph' ? 'score' : params.get('panel');
    if (panel && document.getElementById(`panel-${panel}`)) switchPanel(panel);
    const speed = parseFloat(params.get('speed'));
    if (speed > 0) { state.speed = speed; $('speed').value = String(speed); }
    const hero = params.get('hero');
    if (hero != null && playerById(parseInt(hero, 10))) setFarmPlayer(parseInt(hero, 10));
    const sel = params.get('select');
    if (sel != null && playerById(parseInt(sel, 10))) { state.selected = parseInt(sel, 10); refreshSelection(); }
    if (params.get('follow') === '1') setFollow(true);
    if (params.get('play') === '1') setPlaying(true);
  }

  init();
})();
