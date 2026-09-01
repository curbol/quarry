// Package gridwindow holds the arithmetic behind the grid's card recycling, separated
// from the DOM it is driven by.
//
// The grid keeps a bounded number of live cards over a result set that can run to
// ~150k, holding the space the dropped rows occupied open with spacers so the
// scrollbar stays honest. Which range is live, and when that range is rebuilt, are
// pure functions of the scroll position and a handful of measurements — so they live
// here, where they can be checked without a browser. Getting the rebuild condition
// subtly wrong is invisible in the UI (the grid still works; it just rebuilds every
// row instead of every few hundred), which is exactly the kind of bug that needs a
// test rather than an eye.

// LIVE is how many cards stay in the DOM. Comfortably more than a screenful at any
// window size, so ordinary scrolling never outruns the rebuild.
export const LIVE = 1500;

// SLACK is how much live margin the viewport must keep on each side before a rebuild
// is due, measured in items.
export const SLACK = 400;

// wantedRange is the item range that should be live for a scroll position: a
// fixed-width window centred on the viewport, snapped to whole rows so the spacers
// line up with the grid.
//
// Centred, rather than biased down the list in the direction most scrolling goes,
// because needsRebuild demands the same margin on both sides. Holding a third of the
// window above the viewport and two thirds below leaves only a fraction of the
// downward play in the upward direction — a rebuild every dozen-odd rows scrolling
// back up against every hundred going down, which costs more over any round trip than
// the bias saves going down.
export function wantedRange({ scrollTop, rowH, cols, total, size = LIVE }) {
  const firstVisibleRow = Math.max(0, Math.floor(scrollTop / rowH));
  const rowsLive = Math.ceil(size / cols);
  const startRow = Math.max(0, firstVisibleRow - Math.floor(rowsLive / 2));
  const start = startRow * cols;
  return { start, end: Math.min(total, start + rowsLive * cols) };
}

// visibleRange is the item range the viewport actually covers, snapped to whole rows.
// wantedRange says where the live range should sit; this says what the user is looking
// at, and the two answer different questions.
export function visibleRange({ scrollTop, viewportH, rowH, cols, total }) {
  const firstRow = Math.max(0, Math.floor(scrollTop / rowH));
  const lastRow = Math.max(firstRow, Math.floor((scrollTop + viewportH) / rowH));
  return {
    start: Math.min(total, firstRow * cols),
    end: Math.min(total, (lastRow + 1) * cols),
  };
}

// needsRebuild reports whether the live range is about to run out under the viewport.
//
// The test is how much live margin is left on each side, never how far the wanted
// range has moved. Those are not the same, and the difference is the whole point:
// wantedRange is a fixed-width window that slides with the viewport, so away from the
// ends its start moves by one column every time a row scrolls past and its end moves
// with it. Asking whether it still fits inside the live range is then only ever true
// at exact equality, which rebuilt every card on every row of scroll in either
// direction — in the mechanism that exists to keep a 150k-card grid responsive.
//
// The margin is genuinely unavailable at the ends of the list, where there is nothing
// further to hold, so each side counts as satisfied once the live range reaches it.
export function needsRebuild({ live, view, total, slack = SLACK }) {
  const above = live.start <= 0 || view.start - live.start >= slack;
  const below = live.end >= total || live.end - view.end >= slack;
  return !(above && below);
}

// spacerRows is how many whole rows a spacer stands in for: the rows before the live
// range, and the rows after it.
export function spacerRows({ start, end, total, cols }) {
  return {
    before: Math.ceil(start / cols),
    after: Math.ceil(Math.max(0, total - end) / cols),
  };
}

// windowDelta turns "the live range is `live`, the wanted range is `want`" into the
// edits that move one to the other: how many cards to drop off each end, and which
// sub-ranges to add back. `splice` is false when the two ranges do not overlap at all,
// where replacing the whole window is both simpler and no more work.
//
// Here rather than in the render method it drives, for the reason the rest of this file
// is here: an off-by-one inserts a duplicated row or leaves a gap in the middle of a
// 150k-item grid, and the whole-window fallback hides it again on the next big jump.
// The caller does the DOM; this decides what the DOM should be.
export function windowDelta({ live, want, active }) {
  const keepFrom = Math.max(live.start, want.start);
  const keepTo = Math.min(live.end, want.end);
  if (!active || keepFrom >= keepTo) {
    return { splice: false, dropTop: 0, dropBottom: 0, addBefore: null, addAfter: null };
  }
  return {
    splice: true,
    dropTop: keepFrom - live.start,
    dropBottom: live.end - keepTo,
    addBefore: want.start < keepFrom ? { start: want.start, end: keepFrom } : null,
    addAfter: want.end > keepTo ? { start: keepTo, end: want.end } : null,
  };
}
