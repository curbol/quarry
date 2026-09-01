import { contentURL, thumbURL } from '/static/scene.js';
import { lazyWork, modelThumbs, forgetThumbs, ensureFont } from '/static/thumbs.js';
import { LIVE, wantedRange, visibleRange, needsRebuild, spacerRows, windowDelta } from '/static/gridwindow.js';
import { iconEl, protoClone } from '/static/icons.js';
import { nextTags } from '/static/tagedit.js';
import { startViewer } from '/static/viewer.js';

const PAGE = 200;

const els = {
  q: document.getElementById('q'),
  sort: document.getElementById('sort'),
  group: document.getElementById('group'),
  count: document.getElementById('count'),
  grid: document.getElementById('grid'),
  sentinel: document.getElementById('sentinel'),
  empty: document.getElementById('empty'),
  error: document.getElementById('load-error'),
  tagError: document.getElementById('tag-error'),
};

// gen is bumped by reset() so a page still in flight from the previous filter can
// tell that its results are stale and drop them.
// facetsMode is the grouping the dropdown counts were built under, not a bare
// "loaded" flag: the server counts cards and rows separately, because one result is
// not one thing, and a count carried over from the other mode advertises a number
// clicking it cannot return.
const state = { gen: 0, offset: 0, total: 0, loading: false, done: false, failed: false, facetsMode: null, items: [] };

// ---- data ----

// grouping is the single reading of the group checkbox, so the results, the facet
// counts and the tag counts cannot disagree about which mode the page is in — the
// same reason the server has one ungrouped().
function grouping() { return els.group.checked ? 'cards' : 'assets'; }

function query(extra = {}) {
  const p = new URLSearchParams();
  if (els.q.value.trim()) p.set('q', els.q.value.trim());
  // No boxes checked = no param = no filter; each checked value is appended so the
  // backend unions them. An empty value is the variant "(loose / unknown)" bucket.
  for (const f of FILTERS) for (const v of filters[f.id].getSelected()) p.append(f.id, v);
  for (const t of tagFilter.selected) p.append('tag', t);
  if (tagFilter.selected.size) p.set('tagmode', tagFilter.mode);
  if (tagFilter.selected.size && tagFilter.related) p.set('includeRelated', '1');
  p.set('sort', els.sort.value);
  if (!els.group.checked) p.set('group', '0');
  for (const [k, v] of Object.entries(extra)) p.set(k, v);
  return p;
}


// sentinelNear reports whether the load sentinel is within a screenful of the
// viewport bottom — i.e. more should load.
function sentinelNear() {
  const vh = window.innerHeight || document.documentElement.clientHeight;
  return els.sentinel.getBoundingClientRect().top <= vh + 600;
}

// fetchPage loads the next page. While one is in flight it hands back that same
// promise rather than a resolved one, so a caller that awaits it — navLightbox
// stepping past the last loaded item — waits for the page instead of racing it and
// finding the list unchanged. Without that, a second arrow press at the tail resolved
// instantly, found nothing new, and was silently dropped.
let inflight = null;
async function fetchPage() {
  if (state.done) return;
  if (state.loading) return inflight;
  const run = loadPage();
  inflight = run.finally(() => { if (inflight === run) inflight = null; });
  return inflight;
}

async function loadPage() {
  state.loading = true;
  // reset() clears the grid and releases the loading latch, so a page already in
  // flight would otherwise resume and append its now-stale items to the fresh grid
  // while the new request does the same: duplicate cards and a wrong offset.
  const gen = state.gen;
  const p = query({ offset: state.offset, limit: PAGE });
  try {
    const res = await fetch('/api/assets?' + p.toString());
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const data = await res.json();
    if (!Array.isArray(data.items)) throw new Error('malformed response');
    if (gen !== state.gen) return;
    const mode = grouping();
    if (state.facetsMode !== mode && data.facets) { populateFacets(data.facets); state.facetsMode = mode; }
    state.total = data.total;
    for (const a of data.items) state.items.push(a);
    if (!gridWindow.appended()) {
      for (const a of data.items) els.grid.appendChild(card(a));
    }
    state.offset += data.items.length;
    if (data.items.length === 0 || state.offset >= data.total) state.done = true;
    els.count.textContent = state.total + (state.total === 1 ? ' asset' : ' assets');
    els.empty.hidden = state.total !== 0;
    state.failed = false;
    els.error.hidden = true;
  } catch (e) {
    // Everything that consumes the response is inside the try, not just the fetch: a
    // body that parses but is missing a field would otherwise throw with the loading
    // latch still held, which stops the grid loading anything more for the rest of the
    // session. Release it so the next scroll retries, and say so on screen — silently
    // stopping is indistinguishable from having reached the end.
    if (gen !== state.gen) return;
    console.error('loading assets failed', e);
    state.failed = true;
    els.error.textContent = 'Could not load assets (' + (e && e.message ? e.message : 'unknown error') + '). Scroll or change a filter to retry.';
    els.error.hidden = false;
  }
  state.loading = false;
  // A page may not push the sentinel off-screen (big monitor, short page); the
  // IntersectionObserver won't re-fire while it stays visible, so keep filling.
  //
  // Not after a failure, though. The sentinel is still exactly where it was and
  // state.done is still false, so this would call straight back into a request that
  // fails again — and against a quarry that has stopped, fetch rejects in about a
  // millisecond. The latch is what makes the message above true: the retry comes from
  // a scroll, which is a person deciding to ask again.
  if (!state.done && !state.failed && sentinelNear()) fetchPage();
}

function reset() {
  state.gen++;
  state.offset = 0; state.total = 0; state.done = false; state.loading = false;
  state.failed = false;
  state.items = [];
  // Let go of the outgoing cards before dropping them: what is observing them, what is
  // still being rendered for them, and what would repaint them on a tag edit all
  // outlive the DOM otherwise.
  //
  // Card by card rather than by rebuilding the two observers wholesale. They are not
  // the grid's alone — the lightbox's related strip registers its thumbnails through
  // the same thumbContent — and a reset can arrive with one open, since editing a tag
  // under an active tag filter re-runs the query. Tearing the observers down then left
  // the strip's holders in the DOM watched by nobody and in no pending set, spinning
  // for a render nothing would ask for again.
  for (const el of els.grid.querySelectorAll('.card')) forgetCard(el);
  tagWatchers.clear();
  gridWindow.reset();
  els.grid.replaceChildren();
  // Cards appended straight into a fresh result set animate in; cards the window
  // rebuilds while scrolling do not. See the .initial rule in style.css.
  els.grid.classList.add('initial');
  fetchPage();
}

// gridWindow keeps the number of live cards bounded once a scroll session gets long.
//
// `content-visibility: auto` skips layout and paint for off-screen cards, but it does
// not remove them: each card is ~11 elements, so a long scroll through a large library
// climbs into the hundreds of thousands of nodes, every one of them re-checked for
// relevancy on each frame. Below the threshold nothing here runs at all and the grid
// behaves exactly as it always did — the recycling path exists only for the case that
// needs it.
//
// The grid is a uniform auto-fill grid, so an item's row is a division and the space a
// dropped row occupied can be held open by a full-width spacer of the same height. That
// keeps the scrollbar honest and the scroll position stable.
const gridWindow = {
  size: LIVE,
  start: 0,
  end: 0,
  active: false,
  top: null,
  bottom: null,

  reset() {
    this.start = 0;
    this.end = 0;
    this.active = false;
    this.top = this.bottom = null;
    this.geom = null;
  },

  // geom caches what metrics() measures. Reading it costs a querySelectorAll over
  // every live card plus getComputedStyle and two getBoundingClientRect, which forces
  // a synchronous layout; doing that on every scroll frame is most of the cost of
  // scrolling. Only a resize or a rebuild can change it.
  geom: null,

  // metrics reads the live geometry rather than assuming it: the column count depends
  // on the viewport width and the row height on the cards' own content.
  metrics() {
    if (this.geom) return this.geom;
    const cards = els.grid.querySelectorAll('.card');
    if (!cards.length) return null;
    const cs = getComputedStyle(els.grid);
    const cols = cs.gridTemplateColumns.split(' ').filter(Boolean).length;
    if (!cols) return null;
    const gap = parseFloat(cs.rowGap) || 0;
    const first = cards[0].getBoundingClientRect();
    const rows = Math.ceil(cards.length / cols);
    // Averaged across every live row rather than sampled from one card: a card's height
    // varies a little with how its name wraps, and one sample made the estimated total
    // height — and so the scrollbar — jump each time the window rebuilt on other cards.
    const last = cards[cards.length - 1].getBoundingClientRect();
    const rowH = rows > 1 ? (last.bottom - first.top + gap) / rows : first.height + gap;
    if (!rowH) return null;
    this.geom = { cols, rowH };
    return this.geom;
  },

  spacer(where) {
    const el = document.createElement('div');
    el.className = 'grid-spacer';
    el.dataset.where = where;
    return el;
  },

  // scrollTop is the viewport's offset into the grid itself, which is what the range
  // arithmetic is expressed in.
  scrollTop() { return window.scrollY - els.grid.offsetTop; },

  // sync rebuilds the live range when the viewport is close to running out of it.
  // Called after each page and on scroll/resize. The decision itself lives in
  // gridwindow.js; this supplies the measurements and does the DOM work.
  sync(force) {
    const total = state.items.length;
    if (!this.active && total <= this.size) return;
    const m = this.metrics();
    if (!m) return;
    const at = { scrollTop: this.scrollTop(), rowH: m.rowH, cols: m.cols, total };
    if (!force && this.active) {
      const view = visibleRange({ ...at, viewportH: window.innerHeight });
      if (!needsRebuild({ live: { start: this.start, end: this.end }, view, total })) return;
    }
    const want = wantedRange({ ...at, size: this.size });
    this.render(want.start, want.end, m);
  },

  cards(from, to) {
    const frag = document.createDocumentFragment();
    for (let i = from; i < to; i++) frag.appendChild(card(state.items[i]));
    return frag;
  },

  // render moves the live range to [start, end). The window slides by a fraction of
  // its own width — needsRebuild fires on the margin, not on movement — so most of
  // what is live stays live, and only the ends are rebuilt.
  //
  // Replacing all of it instead was two costs, not one. Every surviving card was
  // recreated, and every render already in flight was cancelled along with it: the
  // worker finished the job, the result no longer matched any live holder, and the
  // freshly built card asked for the identical render again. A model slow enough to
  // still be rendering when the window moved could be started and abandoned several
  // times before it ever landed.
  render(start, end, m) {
    const total = state.items.length;
    const rows = spacerRows({ start, end, total, cols: m.cols });
    const d = windowDelta({
      live: { start: this.start, end: this.end },
      want: { start, end },
      active: this.active,
    });

    if (d.splice) {
      for (let i = 0; i < d.dropTop; i++) this.drop(this.top.nextElementSibling);
      for (let i = 0; i < d.dropBottom; i++) this.drop(this.bottom.previousElementSibling);
      if (d.addBefore) this.top.after(this.cards(d.addBefore.start, d.addBefore.end));
      if (d.addAfter) this.bottom.before(this.cards(d.addAfter.start, d.addAfter.end));
    } else {
      // First activation, or a jump far enough that nothing live is still wanted. The
      // cards being replaced include the ones fetchPage appended before recycling
      // turned on, which have the same registrations to release.
      for (const el of els.grid.querySelectorAll('.card')) forgetCard(el);
      this.top = this.spacer('top');
      this.bottom = this.spacer('bottom');
      els.grid.replaceChildren(this.top, this.cards(start, end), this.bottom);
    }
    this.top.style.height = rows.before * m.rowH + 'px';
    this.bottom.style.height = rows.after * m.rowH + 'px';

    els.grid.classList.remove('initial');
    this.start = start;
    this.end = end;
    this.active = true;
    // Re-measured on the next read: the cards that back the measurement have changed.
    this.geom = null;
  },

  drop(el) {
    forgetCard(el);
    el.remove();
  },

  // append adds a freshly loaded page. Once recycling is on the page goes into the
  // model and the window decides what is live; before that it is appended directly, so
  // the common case never pays for any of this.
  appended() {
    if (!this.active && state.items.length <= this.size) return false;
    this.sync(true);
    return true;
  },
};

// Each type/vendor/variant filter is a checkbox dropdown: none checked = no filter
// (all), any checked = the union of those values. The empty-string value is a real
// facet bucket ("(loose / unknown)" for variants), so it's just another checkbox.
// dropdowns holds every one of them, tag filter included, so opening any one closes
// the rest.
const dropdowns = [];

// wireDropdown builds the button label and caret and hooks up open/close, registering
// the owner so the whole set stays mutually exclusive. The owner supplies setOpen,
// because the tag filter renders its contents lazily on open. It sets this.btn,
// this.pop and this.label on the owner.
function wireDropdown(owner, root) {
  owner.root = root;
  owner.btn = root.querySelector('.ms-btn');
  owner.pop = root.querySelector('.ms-pop');
  owner.label = document.createElement('span');
  owner.label.className = 'ms-btn-label';
  const caret = document.createElement('span');
  caret.className = 'ms-caret';
  caret.textContent = '▾';
  owner.btn.append(owner.label, caret);
  owner.btn.addEventListener('click', (e) => {
    e.stopPropagation();
    const open = owner.pop.hidden;
    for (const d of dropdowns) d.setOpen(false);
    owner.setOpen(open);
  });
  owner.pop.addEventListener('click', (e) => e.stopPropagation());
  dropdowns.push(owner);
}

// msOption is one checkbox row in a dropdown: the facet lists and the tag filter draw
// the same thing, down to the class names the stylesheet keys on, and differ only in a
// colour dot and where the count comes from. Built here so the two cannot drift apart.
function msOption({ checked, label, count, countTitle = '', before = null, onToggle }) {
  const row = document.createElement('label');
  row.className = 'ms-opt';
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.checked = checked;
  cb.addEventListener('change', () => onToggle(cb.checked));
  const text = document.createElement('span');
  text.className = 'ms-opt-label';
  text.textContent = label;
  const n = document.createElement('span');
  n.className = 'ms-opt-count';
  n.textContent = count;
  if (countTitle) n.title = countTitle;
  row.append(cb, ...(before ? [before] : []), text, n);
  return row;
}


class MultiSelect {
  constructor(root, allLabel, isVariant, onChange) {
    this.allLabel = allLabel;
    this.isVariant = isVariant;
    this.onChange = onChange;
    this.selected = new Set();
    wireDropdown(this, root);
    this.renderButton();
  }

  setOpen(open) { this.pop.hidden = !open; this.root.classList.toggle('open', open); }

  // display renders a facet value, giving the empty bucket a readable label.
  display(value) {
    if (value !== '') return value;
    return this.isVariant ? '(loose / unknown)' : '(none)';
  }

  setOptions(values) {
    this.pop.replaceChildren();
    for (const f of values) {
      this.pop.appendChild(msOption({
        checked: this.selected.has(f.value),
        label: this.display(f.value),
        count: f.count,
        onToggle: (on) => {
          if (on) this.selected.add(f.value); else this.selected.delete(f.value);
          this.renderButton();
          this.onChange();
        },
      }));
    }
    this.renderButton();
  }

  renderButton() {
    const n = this.selected.size;
    this.btn.classList.toggle('active', n > 0);
    if (n === 0) this.label.textContent = this.allLabel;
    else if (n === 1) this.label.textContent = this.display([...this.selected][0]);
    else this.label.textContent = n + ' selected';
  }

  getSelected() { return [...this.selected]; }
}

const FILTERS = [
  { id: 'type', all: 'all types', key: 'categories', isVariant: false },
  { id: 'vendor', all: 'all vendors', key: 'vendors', isVariant: false },
  { id: 'variant', all: 'all variants', key: 'variants', isVariant: true },
];
const filters = {};
for (const f of FILTERS) filters[f.id] = new MultiSelect(document.getElementById(f.id), f.all, f.isVariant, reset);
document.addEventListener('click', () => { for (const d of dropdowns) d.setOpen(false); });

function populateFacets(facets) {
  for (const f of FILTERS) filters[f.id].setOptions(facets[f.key]);
}

// ---- tags ----

const TAG_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20.6 13.4 12 22l-8-8V4h10l6.6 6.6a2 2 0 0 1 0 2.8z"/><circle cx="7.5" cy="7.5" r="1.2"/></svg>';
const MAX_SLIVERS = 6;

const tagState = { enabled: false, colors: new Map(), counts: new Map(), offIndex: new Map(), tags: [] };

function tagColor(id) { return tagState.colors.get(id) || '#9aa0aa'; }
function hex6(c) { return /^#[0-9a-fA-F]{6}$/.test(c) ? c : '#9aa0aa'; }

// applyTagCounts re-reads the palette under the current grouping. Called on every
// palette response and again when the grouping changes, since the same palette means
// different numbers on either side of it.
function applyTagCounts() {
  const cards = grouping() === 'cards';
  tagState.counts = new Map(tagState.tags.map((t) => [t.id, cards ? t.count : t.assets]));
  tagState.offIndex = new Map(tagState.tags.map((t) => [t.id, t.offIndex || 0]));
}

// applyPalette syncs the local palette from any tag API response, so a newly
// created or recolored tag is known before its slivers/chips render.
function applyPalette(p) {
  if (!p) return;
  tagState.enabled = !!p.enabled;
  const colors = new Map((p.tags || []).map((t) => [t.id, t.color]));
  // Assigning a tag returns the palette too, so a just-created tag's color is known
  // without a second request — but it leaves every color alone. Repainting anyway
  // walks the whole grid three times on the app's most frequent write.
  const repaint = colors.size !== tagState.colors.size
    || [...colors].some(([id, c]) => tagState.colors.get(id) !== c);
  tagState.colors = colors;
  // Both numbers come back for the reason the facets return two sets; which one the
  // chip shows has to follow the grouping the query will run under.
  tagState.tags = p.tags || [];
  applyTagCounts();
  document.body.classList.toggle('tags-on', tagState.enabled);
  tagFilter.root.hidden = !tagState.enabled;
  tagFilter.setOptions();
  if (repaint) restyleTags();
}

// loadPalette turns tagging on. A failure here used to be swallowed, which presented
// as tagging having switched itself off: no tag filter, no card tag buttons, no
// lightbox tag panel, and nothing said. That is the outcome the write path goes out of
// its way to avoid, and "tagging is never silently off" is the same promise the CLI
// keeps by always resolving a store. So it is reported and retried; only a server that
// genuinely has no store leaves it off, and that answer arrives as a successful
// response saying so.
async function loadPalette(attempt = 0) {
  try {
    const resp = await fetch('/api/tags');
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    applyPalette(await resp.json());
  } catch (e) {
    if (attempt < 2) {
      setTimeout(() => loadPalette(attempt + 1), 500 * (attempt + 1));
      return;
    }
    console.error('could not load the tag palette', e);
    // Its own element, not the grid's. Sharing one meant whichever wrote second won —
    // and the palette retries on a timer, so it always did — and then the next page
    // that loaded successfully hid it again. Tagging silently unavailable is the one
    // thing this message exists to prevent.
    els.tagError.textContent = 'Tagging is unavailable (' + (e && e.message ? e.message : 'unknown error') + '). Reload to try again.';
    els.tagError.hidden = false;
  }
}

// tagWatchers repaints the cards a tag edit affects, keyed on fingerprint — which is
// the asset's actual identity — rather than on the object the edit happened to arrive
// through. A card opened from the lightbox's "parts of this set" strip is a different
// object than the grid's card for the same asset, so hanging the repaint off the object
// left the grid showing tags that were no longer true.
const tagWatchers = new Map(); // fingerprint -> Set<{ asset, repaint }>

function watchTags(asset, repaint) {
  const entry = { asset, repaint };
  for (const fp of asset.fingerprints || []) {
    let set = tagWatchers.get(fp);
    if (!set) tagWatchers.set(fp, (set = new Set()));
    set.add(entry);
  }
  return entry;
}

// unwatchTags drops one card's registration. The map is keyed by fingerprint and
// outlives any single card, so a window that replaces part of its DOM without this
// keeps every card the session ever drew — and repaints them.
function unwatchTags(entry) {
  if (!entry) return;
  for (const fp of entry.asset.fingerprints || []) {
    const set = tagWatchers.get(fp);
    if (!set) continue;
    set.delete(entry);
    if (!set.size) tagWatchers.delete(fp);
  }
}

// applyTagChange folds one edit into every card that shares a fingerprint with it.
// What the edit does to a card is nextTags; this is which cards it reaches.
function applyTagChange(fingerprints, tag, on) {
  const done = new Set();
  for (const fp of fingerprints) {
    for (const e of tagWatchers.get(fp) || []) {
      if (done.has(e)) continue;
      done.add(e);
      e.asset.tags = nextTags({
        cardFingerprints: e.asset.fingerprints,
        cardTags: e.asset.tags,
        edited: fingerprints,
        tag,
        on,
      });
      e.repaint();
    }
  }
}

// apiAssign toggles a tag across a card's whole fingerprint set and returns the
// resulting union of tag ids for that set, or null on failure so a caller leaves the
// card's displayed tags untouched rather than wiping them.
async function apiAssign(fingerprints, tag, on) {
  let res;
  try {
    res = await fetch('/api/assign', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ fingerprints, tag, on }),
    });
  } catch {
    reportTagError(null);
    return null;
  }
  const data = await res.json().catch(() => null);
  if (!res.ok || !data) {
    reportTagError(data);
    return null;
  }
  applyPalette(data.palette);
  // Broadcast here rather than at each call site, so no caller can forget to and leave
  // a card on screen contradicting what was just written.
  applyTagChange(fingerprints, tag, on);
  // Under a tag filter the edit changes which cards match, not just how they look, and
  // the grid pages by offset into a set the server recomputes per request. Folding the
  // edit in place leaves the client's offset pointing one past where it was: the card
  // that shifted into that slot is never requested, and it is missing from the grid for
  // the rest of the session. Re-running the query is what keeps the offsets honest.
  if (tagFilter.selected.size) reset();
  return data.tags || [];
}

// apiTag edits the palette (create, rename, recolor, delete) and returns whether it
// took. An error body is JSON too, so feeding the response straight to applyPalette
// read a missing `enabled` as false and a missing `tags` as none: a save that failed
// would have presented as tagging switching itself off, in every open tab.
async function apiTag(method, body) {
  let res;
  try {
    res = await fetch('/api/tags', {
      method, headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  } catch (e) {
    // Reported, not swallowed. The store is the one thing quarry writes, so a write
    // that did not land has to say so: returning false alone left a delete or a rename
    // that never reached the server indistinguishable from a click the UI ignored,
    // while the same request failing with a 500 raised an alert.
    console.error('tag palette edit failed', e);
    reportTagError(null);
    return false;
  }
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    reportTagError(data);
    return false;
  }
  applyPalette(data);
  return true;
}

// reportTagError surfaces a rejected palette edit. The store is the one thing quarry
// writes, so a write that did not land has to say so rather than look like a no-op.
function reportTagError(data) {
  alert('Tag change failed.\n\n' + ((data && data.error) || 'The server rejected the change.'));
}

// restyleTags repaints every rendered sliver, chip, and filter dot after a recolor.
function restyleTags() {
  for (const s of document.querySelectorAll('.sliver[data-tag]')) s.style.background = tagColor(s.dataset.tag);
  for (const c of document.querySelectorAll('.tag-chip[data-tag]')) c.style.setProperty('--tc', tagColor(c.dataset.tag));
  for (const d of document.querySelectorAll('.tag-dot[data-tag]')) d.style.background = tagColor(d.dataset.tag);
}

// renderSlivers fills a card's tag strip: one colored segment per tag (no text) plus
// a +N overflow marker, hidden when the card has no tags.
function renderSlivers(bar, a) {
  bar.replaceChildren();
  const tags = a.tags || [];
  bar.hidden = tags.length === 0;
  const title = tags.join(', ');
  for (const t of tags.slice(0, MAX_SLIVERS)) {
    const s = document.createElement('span');
    s.className = 'sliver';
    s.dataset.tag = t;
    s.style.background = tagColor(t);
    s.title = title;
    bar.appendChild(s);
  }
  if (tags.length > MAX_SLIVERS) {
    const more = document.createElement('span');
    more.className = 'sliver-more';
    more.textContent = '+' + (tags.length - MAX_SLIVERS);
    more.title = title;
    bar.appendChild(more);
  }
}

// hasFingerprints reports whether a card can be tagged at all.
function hasFingerprints(a) { return Array.isArray(a.fingerprints) && a.fingerprints.length > 0; }

// ---- tag menu (assign / create-on-the-fly, from a card or the lightbox) ----

let tagMenu = null;
function closeTagMenu() { if (tagMenu) { tagMenu.remove(); tagMenu = null; } }

// openTagMenu shows a checkbox list of existing tags (toggling assigns/unassigns
// across the card's whole fingerprint set) plus a field to create-and-assign. It
// calls onChange after any change so the caller repaints its slivers/chips.
function openTagMenu(anchor, a, onChange) {
  closeTagMenu();
  const menu = document.createElement('div');
  menu.className = 'tag-menu';
  menu.addEventListener('click', (e) => e.stopPropagation());

  const input = document.createElement('input');
  input.className = 'tag-menu-new';
  input.type = 'text';
  input.placeholder = 'Search or add a tag…';

  const list = document.createElement('div');
  list.className = 'tag-menu-list';

  const rebuild = () => {
    list.replaceChildren();
    const have = new Set(a.tags || []);
    const q = input.value.trim().toLowerCase();
    const ids = [...tagState.colors.keys()]
      .filter((id) => !q || id.toLowerCase().includes(q))
      .sort((x, y) => x.localeCompare(y));
    if (ids.length === 0) {
      const hint = document.createElement('div');
      hint.className = 'tag-menu-empty';
      hint.textContent = q
        ? 'Enter to create "' + input.value.trim() + '"'
        : (tagState.colors.size ? 'No matches.' : 'No tags yet. Type to create one.');
      list.appendChild(hint);
    }
    for (const id of ids) {
      const row = document.createElement('label');
      row.className = 'tag-menu-opt';
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = have.has(id);
      cb.addEventListener('change', async () => {
        const t = await apiAssign(a.fingerprints, id, cb.checked);
        if (t === null) { cb.checked = !cb.checked; return; }
        a.tags = t;
        onChange();
      });
      const dot = document.createElement('span');
      dot.className = 'tag-dot';
      dot.style.background = tagColor(id);
      const lbl = document.createElement('span');
      lbl.className = 'tag-menu-label';
      lbl.textContent = id;
      row.append(cb, dot, lbl);
      list.appendChild(row);
    }
  };
  rebuild();

  input.addEventListener('input', rebuild);
  input.addEventListener('keydown', async (e) => {
    if (e.key !== 'Enter') return;
    const name = input.value.trim();
    if (!name) return;
    input.value = '';
    const t = await apiAssign(a.fingerprints, name, true);
    if (t !== null) a.tags = t;
    rebuild();
    onChange();
  });

  menu.append(input, list);
  document.body.appendChild(menu);
  const r = anchor.getBoundingClientRect();
  menu.style.top = Math.min(r.bottom + 4, window.innerHeight - menu.offsetHeight - 8) + 'px';
  menu.style.left = Math.min(r.left, window.innerWidth - menu.offsetWidth - 8) + 'px';
  tagMenu = menu;
  setTimeout(() => input.focus(), 0);
}

// ---- tag filter (header): select tags to filter by, with an AND/OR toggle and an
// inline manage mode to rename / recolor / delete tags library-wide. ----

const tagFilter = {
  root: document.getElementById('tagfilter'),
  selected: new Set(),
  mode: 'or',
  related: false,
  manage: false,
  init() {
    wireDropdown(this, this.root);
    this.renderButton();
  },
  setOpen(open) {
    this.pop.hidden = !open;
    this.root.classList.toggle('open', open);
    if (open) this.render();
  },
  renderButton() {
    const n = this.selected.size;
    this.btn.classList.toggle('active', n > 0);
    this.label.textContent = n === 0 ? 'tags' : (n === 1 ? [...this.selected][0] : n + ' tags');
  },
  // setOptions runs after any palette change: drop selections for deleted tags and
  // repaint the open popover.
  setOptions() {
    for (const id of [...this.selected]) if (!tagState.colors.has(id)) this.selected.delete(id);
    if (!this.pop.hidden) this.render();
    this.renderButton();
  },
  render() {
    this.pop.replaceChildren();
    const head = document.createElement('div');
    head.className = 'tag-pop-head';
    const modeBtn = document.createElement('button');
    modeBtn.type = 'button';
    modeBtn.className = 'tag-mode';
    const setModeLabel = () => {
      modeBtn.textContent = this.mode === 'and' ? 'ALL' : 'ANY';
      modeBtn.title = 'match ' + (this.mode === 'and' ? 'all selected tags (AND)' : 'any selected tag (OR)');
    };
    setModeLabel();
    modeBtn.addEventListener('click', () => {
      this.mode = this.mode === 'and' ? 'or' : 'and';
      setModeLabel();
      if (this.selected.size) reset();
    });
    const manageBtn = document.createElement('button');
    manageBtn.type = 'button';
    manageBtn.className = 'tag-manage';
    manageBtn.textContent = this.manage ? 'done' : 'manage';
    manageBtn.addEventListener('click', () => { this.manage = !this.manage; this.render(); });
    head.append(modeBtn, manageBtn);
    if (!this.manage) {
      const linked = document.createElement('label');
      linked.className = 'tag-linked';
      linked.title = 'also show assets linked to a match (companions, like a frame’s background)';
      const lcb = document.createElement('input');
      lcb.type = 'checkbox';
      lcb.checked = this.related;
      lcb.addEventListener('change', () => { this.related = lcb.checked; if (this.selected.size) reset(); });
      const lt = document.createElement('span');
      lt.textContent = 'linked';
      linked.append(lcb, lt);
      head.appendChild(linked);
    }
    this.pop.appendChild(head);

    const ids = [...tagState.colors.keys()].sort((x, y) => x.localeCompare(y));
    if (ids.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'tag-pop-empty';
      empty.textContent = 'No tags yet.';
      this.pop.appendChild(empty);
      return;
    }
    for (const id of ids) this.pop.appendChild(this.manage ? this.manageRow(id) : this.filterRow(id));
  },
  filterRow(id) {
    const dot = document.createElement('span');
    dot.className = 'tag-dot';
    dot.dataset.tag = id;
    dot.style.background = tagColor(id);
    // The store keeps assignments for content this library does not hold — a narrowed
    // --root, a disabled pack, another machine — and no filter can reach them. Saying
    // so here is what keeps a tag that is all off-index from reading as an empty one.
    const off = tagState.offIndex.get(id) || 0;
    return msOption({
      checked: this.selected.has(id),
      label: id,
      count: tagState.counts.get(id) || 0,
      countTitle: off ? off + ' more on content outside this library' : '',
      before: dot,
      onToggle: (on) => {
        if (on) this.selected.add(id); else this.selected.delete(id);
        this.renderButton();
        reset();
      },
    });
  },
  manageRow(id) {
    const row = document.createElement('div');
    row.className = 'tag-manage-row';
    const color = tagColorInput(id, 'tag-color');
    const name = document.createElement('input');
    name.type = 'text';
    name.className = 'tag-name';
    name.value = id;
    const commit = async () => {
      const v = name.value.trim();
      if (!v || v === id) return;
      if (await apiTag('PATCH', { id, newId: v })) reset();
      else name.value = id;
    };
    name.addEventListener('keydown', (e) => { if (e.key === 'Enter') { name.blur(); } });
    name.addEventListener('blur', commit);
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'tag-del';
    del.textContent = '🗑';
    del.title = 'delete tag';
    del.addEventListener('click', async () => {
      if (!confirm('Delete tag "' + id + '"? It will be removed from all assets.')) return;
      if (await apiTag('DELETE', { id })) reset();
    });
    row.append(color, name, del);
    return row;
  },
};
tagFilter.init();

document.addEventListener('click', closeTagMenu);

// ---- cards ----

const COPY_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';

// splitName splits a name into a head (fills line 1, ellipsized) and a tail (the
// distinguishing suffix + extension on line 2), cutting at a separator near the end
// so the tail is a clean chunk. Short names go entirely on line 1.
function splitName(name) {
  if (name.length <= 20) return [name, ''];
  let cut = name.length - 14;
  for (let i = cut; i >= cut - 9 && i > 4; i--) {
    if ('-_@.'.includes(name[i - 1])) { cut = i; break; }
  }
  return [name.slice(0, cut), name.slice(cut)];
}

// The page's inline glyphs are keyed by their own markup, which is a module constant
// here — the category icons keep their own cache keyed by category name.
const svgProtos = new Map();
const svgIcon = (markup) => protoClone(svgProtos, markup, markup);

// forgetCard is forgetThumbs plus the tag-repaint registration only a grid card has.
function forgetCard(el) {
  forgetThumbs(el);
  unwatchTags(el._tagWatch);
  el._tagWatch = null;
}

function card(a) {
  const el = document.createElement('div');
  el.className = 'card';
  el.tabIndex = 0;

  const thumb = document.createElement('div');
  thumb.className = 'thumb';
  thumb.appendChild(thumbContent(a));

  const ext = document.createElement('span');
  ext.className = 'ext-badge';
  ext.textContent = a.ext || a.category;
  const vendor = document.createElement('span');
  vendor.className = 'vendor-badge';
  vendor.textContent = a.vendor;
  thumb.append(ext, vendor);

  if (a.count > 1) {
    const cb = document.createElement('span');
    cb.className = 'count-badge';
    cb.textContent = '×' + a.count;
    cb.title = a.count + ' copies (variants / packs)';
    thumb.appendChild(cb);
  }

  // Copy icon (hover), top-right; ✓ feedback without replacing the icon glyph.
  const copy = document.createElement('button');
  copy.className = 'copy-icon';
  copy.appendChild(svgIcon(COPY_SVG));
  copy.title = 'copy path';
  copy.addEventListener('click', (e) => {
    e.stopPropagation();
    flashCopy(copy, a.copyPath);
  });
  thumb.appendChild(copy);

  // Tag affordances: a colored sliver strip (one per tag) along the bottom edge so
  // it never covers the preview, and a hover add button. Both are CSS-gated on
  // body.tags-on; the add button also hides when the asset has no fingerprint.
  const bar = document.createElement('div');
  bar.className = 'sliver-bar';
  renderSlivers(bar, a);
  thumb.appendChild(bar);
  el._tagWatch = watchTags(a, () => renderSlivers(bar, a));

  const tagBtn = document.createElement('button');
  tagBtn.type = 'button';
  tagBtn.className = 'tag-add';
  tagBtn.appendChild(svgIcon(TAG_SVG));
  tagBtn.title = 'tags';
  if (!hasFingerprints(a)) tagBtn.classList.add('nofp');
  tagBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    openTagMenu(tagBtn, a, () => renderSlivers(bar, a));
  });
  thumb.appendChild(tagBtn);

  const name = document.createElement('div');
  name.className = 'name';
  name.title = a.name; // full name on hover
  const [head, tail] = splitName(a.name);
  const l1 = document.createElement('span'); l1.className = 'l1'; l1.textContent = head;
  const l2 = document.createElement('span'); l2.className = 'l2'; l2.textContent = tail;
  name.append(l1, l2);

  el.append(thumb, name);
  el.addEventListener('click', () => openLightbox(a));
  el.addEventListener('keydown', (e) => { if (e.key === 'Enter') openLightbox(a); });
  return el;
}

function thumbContent(a) {
  // Rendered in the worker: models for the obvious reason, and images because every
  // image is ThumbImage regardless of size — a texture-heavy pack is mostly 2048² and
  // 4096² atlases, at ~67 MB of decoded bitmap each, so one screenful of 4K textures in
  // a 158px grid is a couple of gigabytes resident. content-visibility does not help,
  // because the decode is driven by the image loader rather than by paint.
  if (a.thumb === 'image' || a.thumb === 'glb' || a.thumb === 'fbx' || a.thumb === 'sidekick') {
    const holder = document.createElement('div');
    holder.className = 'thumb-3d';
    holder.appendChild(iconEl(a.category));
    modelThumbs.observe(holder, a);
    return holder;
  }
  if (a.thumb === 'preview') {
    const img = new Image();
    img.loading = 'lazy';
    img.src = thumbURL(a.id);
    img.onerror = () => img.replaceWith(iconEl(a.category));
    return img;
  }
  if (a.thumb === 'font') {
    const el = document.createElement('div');
    el.className = 'font-thumb';
    el.textContent = 'Ag';
    // Deferred like the model thumbnails: a FontFace load fetches the whole file, and
    // a page of a font pack would otherwise pull every one of them at card creation.
    lazyWork.when(el, () => {
      ensureFont(a, el).then((fam) => { el.style.fontFamily = `"${fam}", serif`; })
        // Falling back to the category icon is the right answer on screen, but silently
        // it is indistinguishable from a font that simply has no preview.
        .catch((e) => { console.warn('font preview failed', a.relPath, e); el.replaceWith(iconEl(a.category)); });
    });
    return el;
  }
  return iconEl(a.category);
}

// ---- font specimen (the lightbox's enlarged view of a typeface) ----

// fontSample renders a specimen (name, pangram, glyph set, size ramp) in the font.
function fontSample(a) {
  const wrap = document.createElement('div');
  wrap.className = 'font-sample';
  const name = document.createElement('div'); name.className = 'fs-name'; name.textContent = a.name.replace(/\.[^.]+$/, '');
  const pangram = document.createElement('div'); pangram.className = 'fs-pangram'; pangram.textContent = 'The quick brown fox jumps over the lazy dog';
  const glyphs = document.createElement('div'); glyphs.className = 'fs-glyphs';
  glyphs.textContent = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ\nabcdefghijklmnopqrstuvwxyz\n0123456789 !?&@#$%()';
  const ramp = document.createElement('div'); ramp.className = 'fs-ramp';
  for (const px of [30, 22, 16]) {
    const line = document.createElement('div'); line.style.fontSize = px + 'px';
    line.textContent = 'The quick brown fox jumps over the lazy dog';
    ramp.appendChild(line);
  }
  wrap.append(name, pangram, glyphs, ramp);
  ensureFont(a).then((fam) => { wrap.style.fontFamily = `"${fam}"`; })
    .catch(() => { const p = document.createElement('p'); p.className = 'fs-fail'; p.textContent = 'Could not load this font.'; wrap.prepend(p); });
  return wrap;
}

// COPY_FEEDBACK_MS is how long a copy button shows it worked. One constant because
// the icon buttons and the labelled ones are the same affordance and drifted apart
// when each carried its own timeout.
const COPY_FEEDBACK_MS = 1200;

// flashCopy copies text and marks btn as done for a moment. Buttons whose label is
// text (rather than an icon) also say "copied ✓" and get their label back after.
function flashCopy(btn, text, { label = false } = {}) {
  const flash = (ok) => {
    // Stashed on the button rather than captured per click. A second click inside the
    // feedback window used to capture "copied ✓" as the text to restore, and its later
    // timer won — so the button kept that label for the rest of the session.
    if (label) {
      if (btn.dataset.label === undefined) btn.dataset.label = btn.textContent;
      btn.textContent = ok ? 'copied ✓' : 'copy failed';
    }
    btn.classList.add(ok ? 'done' : 'failed');
    clearTimeout(btn._flashTimer);
    btn._flashTimer = setTimeout(() => {
      if (label) btn.textContent = btn.dataset.label;
      btn.classList.remove('done', 'failed');
    }, COPY_FEEDBACK_MS);
  };
  writeClipboard(text).then(flash, () => flash(false));
}

// writeClipboard falls back to a selection copy where the async clipboard API is
// unavailable. navigator.clipboard exists only in a secure context, and serving on a
// routable address — which --addr exists to allow — is plain http, so on every machine
// but the one running quarry the property is simply undefined. Copy-path is a third of
// what this UI is for, and reading it off a missing object threw inside the click
// handler, leaving every copy button silently dead.
function writeClipboard(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.cssText = 'position:fixed;top:-1000px;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand('copy') ? resolve(true) : reject(new Error('copy rejected'));
    } catch (e) {
      reject(e);
    } finally {
      ta.remove();
    }
  });
}

// tagColorInput is the swatch that edits a tag's color. Reverting to the stored color
// when the save is refused is the part worth having in one place: a swatch left showing
// a color the store rejected is the UI quietly disagreeing with the file.
function tagColorInput(id, className) {
  const color = document.createElement('input');
  color.type = 'color';
  color.className = className;
  color.value = hex6(tagColor(id));
  color.addEventListener('change', async () => {
    if (!await apiTag('PATCH', { id, color: color.value })) color.value = hex6(tagColor(id));
  });
  return color;
}


// ---- shared three.js helpers ----

// Synty source FBX reference a shared texture atlas that isn't adjacent to the
// file, so their materials load near-black and the loader chases missing textures.
// A LoadingManager stubs out every sub-resource (only the model URL is served),
// and FBX meshes get a neutral clay material so the geometry is always visible.
// GLB/glTF carry embedded textures, so they keep their real materials.

// ---- lightbox ----

const lb = {
  root: document.getElementById('lightbox'),
  view: document.getElementById('lb-view'),
  name: document.getElementById('lb-name'),
  fields: document.getElementById('lb-fields'),
  tags: document.getElementById('lb-tags'),
  related: document.getElementById('lb-related'),
  character: document.getElementById('lb-character'),
  copies: document.getElementById('lb-copies'),
  prev: document.getElementById('lb-prev'),
  next: document.getElementById('lb-next'),
  index: -1,
};
let activeViewer = null;

// lbGen counts lightbox openings. Anything async started for one asset checks it
// before touching the panel, so a slow response cannot land on whatever the user
// navigated to in the meantime.
let lbGen = 0;
// lbReturnFocus is the element focus came from, restored on close so keyboard
// navigation resumes where it left off instead of at the top of the document.
let lbReturnFocus = null;

// updateLbNav enables/disables the prev/next arrows for the current position in the
// loaded result set. Next stays enabled at the tail while more pages can still load.
function updateLbNav() {
  lb.prev.disabled = lb.index <= 0;
  lb.next.disabled = lb.index < 0 || (lb.index >= state.items.length - 1 && state.done);
}

// navLightbox steps to an adjacent asset in the filtered result set, loading the next
// page first when stepping past the last loaded item.
async function navLightbox(delta) {
  if (lb.root.hidden || lb.index < 0) return;
  let i = lb.index + delta;
  if (i < 0) return;
  if (i >= state.items.length) {
    if (state.done) return;
    await fetchPage();
    if (i >= state.items.length) return;
  }
  openLightbox(state.items[i]);
}

function openLightbox(a) {
  if (activeViewer) { activeViewer.stop(); activeViewer = null; } // tear down when navigating
  // The tag menu closes over the asset it was opened for, so arrowing to another card
  // while it is up would edit this card's tags under the next card's name.
  closeTagMenu();
  const gen = ++lbGen;
  lb.index = state.items.indexOf(a);
  updateLbNav();
  lb.name.textContent = a.name;
  // The metadata carries the shared file properties; every location (one or many)
  // lives in the copies list below, so there's a single copy system either way.
  const bitmap = /^(png|jpe?g|gif|webp|bmp)$/i.test(a.ext || '');
  const hasDims = a.width > 0 && a.height > 0;
  // A card groups by name and size, so its copies can come from different vendors;
  // the panel names every one rather than only the representative's.
  const vendors = [...new Set(copiesOf(a).map((c) => c.vendor).filter(Boolean))];
  const fields = [['Vendor', vendors.join(', ') || '—'], ['Category', a.category], ['Format', a.ext || '—'], ['Size', humanSize(a.size)]];
  if (hasDims) fields.push(['Dimensions', `${a.width} × ${a.height}`]);
  else if (bitmap) fields.push(['Dimensions', '…']);
  // Built as nodes like every other renderer here. The values are library-derived —
  // a vendor is a directory name — and this was the one place in the page where any of
  // that reached markup, so an escaper had to be right rather than merely present.
  lb.fields.replaceChildren(...fields.flatMap(([k, v]) => {
    const dt = document.createElement('dt');
    dt.textContent = k;
    const dd = document.createElement('dd');
    dd.dataset.field = k;
    dd.textContent = v;
    return [dt, dd];
  }));
  // The index carries dimensions for images it could measure; probe the bytes only
  // as a fallback (a copy dropped from the index, or a format the scanner skipped).
  if (!hasDims && bitmap) {
    const probe = new Image();
    // The field is found by querying the DOM, which by then may belong to a different
    // asset: a probe that outlives its own lightbox has to stay quiet, not write one
    // asset's pixel size into another's panel.
    const dd = () => (gen === lbGen ? lb.fields.querySelector('dd[data-field="Dimensions"]') : null);
    probe.onload = () => { const el = dd(); if (el) el.textContent = `${probe.naturalWidth} × ${probe.naturalHeight}`; };
    probe.onerror = () => { const el = dd(); if (el) el.textContent = '—'; };
    probe.src = contentURL(a.id);
  }
  lb.character.replaceChildren(); // the viewer fills this for clip-only animations
  renderLbTags(a);
  renderLbRelated(a, gen);
  renderCopies(a);

  lb.view.replaceChildren();
  if (a.thumb === 'glb' || a.thumb === 'fbx' || a.thumb === 'sidekick') {
    activeViewer = startViewer(lb.view, a, { character: lb.character });
  } else if (bitmap) {
    // The expanded view shows the full-resolution image, not the small Unity
    // preview.png a unitypackage entry may also carry; fall back to that preview,
    // then the category icon.
    const img = new Image();
    img.onerror = () => {
      if (a.thumb === 'preview') { img.onerror = () => img.replaceWith(iconEl(a.category)); img.src = thumbURL(a.id); }
      else img.replaceWith(iconEl(a.category));
    };
    img.src = contentURL(a.id);
    lb.view.appendChild(img);
  } else if (a.thumb === 'preview') {
    const img = new Image(); img.src = thumbURL(a.id); lb.view.appendChild(img);
  } else if (a.thumb === 'font') {
    lb.view.appendChild(fontSample(a));
  } else {
    lb.view.appendChild(iconEl(a.category));
  }
  lb.root.hidden = false;
  // Cards are focusable and open on Enter, so a card left focused behind the modal
  // would re-open the lightbox on every Enter — building a second viewer each time —
  // and Tab would walk the grid underneath.
  if (!els.grid.inert) lbReturnFocus = document.activeElement;
  els.grid.inert = true;
  document.getElementById('lb-close').focus();
}

// renderLbTags shows the card's tags as colored chips (each recolorable and
// removable) with an add control, all targeting the card's whole fingerprint set.
// Hidden entirely when tagging is disabled.
function renderLbTags(a) {
  lb.tags.replaceChildren();
  lb.tags.hidden = !tagState.enabled;
  if (!tagState.enabled) return;

  const head = document.createElement('div');
  head.className = 'lb-tags-head';
  const heading = document.createElement('span');
  heading.textContent = 'Tags';
  const add = document.createElement('button');
  add.type = 'button';
  add.className = 'lb-tag-add';
  add.textContent = '+ add';
  if (hasFingerprints(a)) {
    add.addEventListener('click', (e) => {
      e.stopPropagation();
      openTagMenu(add, a, () => renderLbTags(a));
    });
  } else {
    add.disabled = true;
    add.title = 'this asset has no content fingerprint, so it cannot be tagged';
  }
  head.append(heading, add);

  const chips = document.createElement('div');
  chips.className = 'tag-chips';
  for (const id of (a.tags || [])) chips.appendChild(lbTagChip(a, id));

  lb.tags.append(head, chips);
}

function lbTagChip(a, id) {
  const chip = document.createElement('span');
  chip.className = 'tag-chip';
  chip.dataset.tag = id;
  chip.style.setProperty('--tc', tagColor(id));

  const color = tagColorInput(id, 'tag-chip-color');
  color.title = 'change color';
  color.addEventListener('click', (e) => e.stopPropagation());

  const label = document.createElement('span');
  label.className = 'tag-chip-label';
  label.textContent = id;

  const x = document.createElement('button');
  x.type = 'button';
  x.className = 'tag-chip-x';
  x.textContent = '×';
  x.title = 'remove tag';
  x.addEventListener('click', async (e) => {
    e.stopPropagation();
    const t = await apiAssign(a.fingerprints, id, false);
    if (t === null) return;
    a.tags = t;
    renderLbTags(a);
  });

  chip.append(color, label, x);
  return chip;
}

// copiesOf is the card's occurrences, falling back to the representative's own
// fields for a payload that carries no copies list.
function copiesOf(a) {
  return (a.copies && a.copies.length)
    ? a.copies
    : [{ variant: a.variant, vendor: a.vendor, pack: a.pack, copyPath: a.copyPath }];
}

// renderCopies lists where the file lives — one row for a unique file, or every
// occurrence for a file shipped across variants/packs — each with its own copy
// button, plus a copy-all when there's more than one. One system, one or many.
function renderCopies(a) {
  lb.copies.replaceChildren();
  const copies = copiesOf(a);
  const many = copies.length > 1;

  const head = document.createElement('div');
  head.className = 'lb-copies-head';
  const heading = document.createElement('span');
  heading.textContent = many ? copies.length + ' copies' : 'Location';
  head.appendChild(heading);
  if (many) {
    const all = document.createElement('button');
    all.className = 'lb-copyall';
    all.textContent = 'copy all';
    all.addEventListener('click', () => flashCopy(all, copies.map((c) => c.copyPath).join('\n'), { label: true }));
    head.appendChild(all);
  }
  lb.copies.appendChild(head);

  for (const c of copies) {
    const row = document.createElement('div');
    row.className = 'lb-copyrow';
    const label = document.createElement('span');
    label.className = 'lb-copyrow-label';
    label.textContent = [c.variant || 'loose', c.pack || c.vendor].filter(Boolean).join(' · ');
    const code = document.createElement('code');
    code.textContent = c.copyPath;
    const btn = document.createElement('button');
    btn.className = 'lb-copyicon';
    btn.title = 'copy path';
    btn.appendChild(svgIcon(COPY_SVG));
    btn.addEventListener('click', () => flashCopy(btn, c.copyPath));
    row.append(label, code, btn);
    lb.copies.appendChild(row);
  }
}

// renderLbRelated shows the card's linked companions ("parts of this set") as small
// clickable thumbnails that open that asset. It fetches the related cards on demand
// (they can live anywhere in the library) and stays hidden when there are none or
// tagging is off. Clearing on entry only protects the call doing the clearing, so the
// generation is checked after the fetch too: navigating mid-flight would otherwise
// append one asset's companions under the next asset's heading.
async function renderLbRelated(a, gen) {
  const box = lb.related;
  forgetThumbs(box);
  box.replaceChildren();
  box.hidden = true;
  if (!tagState.enabled || !hasFingerprints(a)) return;
  let items;
  try {
    const qs = a.fingerprints.map((fp) => 'fingerprint=' + encodeURIComponent(fp)).join('&');
    // res.ok checked rather than trusting the body: an error response is JSON too, so
    // .items is simply undefined and `|| []` turns a failure into "this asset has no
    // companions" — the strip is absent either way, and nothing anywhere says which.
    const res = await fetch('/api/related?' + qs);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    items = (await res.json()).items || [];
  } catch (e) { console.error('loading linked companions failed', e); return; }
  if (!items.length || lb.root.hidden || gen !== lbGen) return;

  const head = document.createElement('div');
  head.className = 'lb-related-head';
  const heading = document.createElement('span');
  heading.textContent = items.length > 1 ? `Parts of this set (${items.length})` : 'Part of this set';
  head.appendChild(heading);

  const strip = document.createElement('div');
  strip.className = 'lb-related-strip';
  for (const it of items) strip.appendChild(relatedThumb(it));

  box.append(head, strip);
  box.hidden = false;
}

function relatedThumb(it) {
  const el = document.createElement('button');
  el.type = 'button';
  el.className = 'lb-related-item';
  el.title = it.name;
  const th = document.createElement('div');
  th.className = 'lb-related-thumb';
  th.appendChild(thumbContent(it));
  const name = document.createElement('span');
  name.className = 'lb-related-name';
  name.textContent = it.name;
  el.append(th, name);
  el.addEventListener('click', () => openLightbox(it));
  return el;
}

function closeLightbox() {
  lb.root.hidden = true;
  // Anchored to a control inside the lightbox, so without this it is left floating
  // over the grid with nothing to dismiss it but a stray click.
  closeTagMenu();
  if (activeViewer) { activeViewer.stop(); activeViewer = null; }
  // The related strip and the character picker register thumbnails with the same two
  // observers the grid uses, and both hold their targets: arrowing through a linked
  // set would otherwise strand one strip's worth of nodes per step.
  forgetThumbs(lb.root);
  lb.view.replaceChildren();
  lb.character.replaceChildren();
  lb.related.replaceChildren();
  lb.related.hidden = true;
  // The grid was made inert on open so Enter could not re-trigger the card behind the
  // modal; hand focus back to where it came from.
  els.grid.inert = false;
  if (lbReturnFocus && lbReturnFocus.isConnected) lbReturnFocus.focus();
  lbReturnFocus = null;
}


document.getElementById('lb-close').addEventListener('click', closeLightbox);
lb.root.querySelector('.lb-backdrop').addEventListener('click', closeLightbox);
lb.prev.addEventListener('click', () => navLightbox(-1));
lb.next.addEventListener('click', () => navLightbox(1));
document.addEventListener('keydown', (e) => {
  if (lb.root.hidden) return;
  // The field guard comes first, Escape included: inside the character search Escape
  // closes the dropdown (handled there, which stops propagation), and inside the tag
  // input it cancels the edit. Closing the whole lightbox from under either is not what
  // the key meant there.
  const t = e.target;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
  if (e.key === 'Escape') { closeLightbox(); return; }
  if (e.key === 'ArrowLeft') { e.preventDefault(); navLightbox(-1); }
  else if (e.key === 'ArrowRight') { e.preventDefault(); navLightbox(1); }
});

// ---- misc ----

function humanSize(n) {
  if (!n) return '—';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}

let debounce;
els.q.addEventListener('input', () => { clearTimeout(debounce); debounce = setTimeout(reset, 220); });
els.sort.addEventListener('change', reset);
// Grouping changes what a result is, so every number beside a filter has to be asked
// for again: the facets come back with the next page, the tag counts are already in
// hand under both modes.
els.group.addEventListener('change', () => { applyTagCounts(); tagFilter.setOptions(); reset(); });

new IntersectionObserver((entries) => {
  if (entries.some((e) => e.isIntersecting)) fetchPage();
}, { rootMargin: '600px' }).observe(els.sentinel);

// Scrolling and resizing both move which cards should be live. Coalesced into a frame
// so a fling costs one check per paint rather than one per scroll event, and both are
// passive so neither can hold up the scroll itself.
let windowTick = 0;
let windowForce = false;
const syncGridWindow = (force) => {
  windowForce = windowForce || !!force;
  if (windowTick) return;
  windowTick = requestAnimationFrame(() => {
    windowTick = 0;
    const f = windowForce;
    windowForce = false;
    gridWindow.sync(f);
  });
};
addEventListener('scroll', () => {
  syncGridWindow(false);
  // The sentinel stays intersecting after a failed page, so the IntersectionObserver
  // never fires again. A scroll is the gesture the error message asks for.
  if (state.failed) { state.failed = false; fetchPage(); }
}, { passive: true });
// A resize changes the column count and the row height, so the cached geometry is
// stale and the spacers have to be remeasured outright. Coalesced like scroll: dragging
// a window edge emits a continuous stream of these, and each one forces a layout read.
addEventListener('resize', () => {
  gridWindow.geom = null;
  syncGridWindow(true);
}, { passive: true });

loadPalette();
fetchPage();
