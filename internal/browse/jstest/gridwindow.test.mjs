// The grid's recycling arithmetic. These live outside assets/ because that directory
// is embedded into the binary and served: a test file there would ship to the browser.
import test from 'node:test';
import assert from 'node:assert/strict';

import {
  LIVE, SLACK, wantedRange, visibleRange, needsRebuild, spacerRows,
} from '../assets/gridwindow.js';

// A realistic desktop grid: six columns of 216px rows under a 900px viewport.
const COLS = 6, ROW_H = 216, VIEWPORT_H = 900;
const BIG = 150000;

// scroll walks a viewport across a list, rebuilding when the window says to, and
// reports both how often that happened and whether the viewport was ever looking at
// cards outside the live range — which would be a hole in the grid.
function scroll({ fromRow, toRow, total }) {
  const at = (row) => ({ scrollTop: row * ROW_H, rowH: ROW_H, cols: COLS, total });
  let live = wantedRange({ ...at(fromRow), size: LIVE });
  let rebuilds = 0, holes = 0;
  const step = toRow >= fromRow ? 1 : -1;
  for (let row = fromRow; step > 0 ? row <= toRow : row >= toRow; row += step) {
    const view = visibleRange({ ...at(row), viewportH: VIEWPORT_H });
    if (needsRebuild({ live, view, total })) {
      rebuilds++;
      live = wantedRange({ ...at(row), size: LIVE });
    }
    if (view.start < live.start || view.end > live.end) holes++;
  }
  return { rebuilds, holes };
}

test('scrolling a row does not rebuild the window', () => {
  // The regression this exists for. The condition used to compare the wanted range
  // against the live one; because the wanted range slides with the viewport, that was
  // only ever satisfied at exact equality, so every row of scroll rebuilt every card.
  for (const [name, from, to] of [['down', 200, 201], ['up', 200, 199]]) {
    const { rebuilds } = scroll({ fromRow: from, toRow: to, total: BIG });
    assert.equal(rebuilds, 0, `scrolling one row ${name} rebuilt the window`);
  }
});

test('a long scroll rebuilds rarely and never leaves a hole', () => {
  const { rebuilds, holes } = scroll({ fromRow: 0, toRow: 3000, total: BIG });
  assert.equal(holes, 0, 'the viewport saw cards outside the live range');
  // The bound is what makes this a test rather than a description: the broken version
  // scored one rebuild per row.
  assert.ok(rebuilds < 100, `${rebuilds} rebuilds over 3000 rows; expected far fewer`);
});

test('scrolling back up is as cheap as scrolling down', () => {
  // The window is centred on the viewport for this reason. Biasing it down the list
  // leaves the upward direction with a fraction of the play, and a grid that stutters
  // only when you scroll back is a worse bug than one that stutters evenly.
  const down = scroll({ fromRow: 500, toRow: 900, total: BIG });
  const up = scroll({ fromRow: 900, toRow: 500, total: BIG });
  assert.equal(up.holes, 0);
  assert.ok(up.rebuilds <= down.rebuilds + 1,
    `upward scrolling rebuilt ${up.rebuilds} times against ${down.rebuilds} downward`);
});

test('the ends of the list have no margin to demand', () => {
  // At the top there is nothing above the live range and at the bottom nothing below,
  // so requiring slack on that side would rebuild forever.
  assert.equal(scroll({ fromRow: 0, toRow: 20, total: BIG }).rebuilds, 0);

  const lastRow = Math.ceil(BIG / COLS) - 1;
  assert.equal(scroll({ fromRow: lastRow, toRow: lastRow - 20, total: BIG }).holes, 0);
});

test('a list that fits in the window never rebuilds', () => {
  const total = LIVE - 1;
  const { rebuilds } = scroll({ fromRow: 0, toRow: 40, total });
  assert.equal(rebuilds, 0);
});

test('the live range stays bounded however far the list runs', () => {
  for (const row of [0, 50, 5000, 24999]) {
    const r = wantedRange({ scrollTop: row * ROW_H, rowH: ROW_H, cols: COLS, total: BIG, size: LIVE });
    assert.ok(r.end - r.start <= LIVE + COLS, `live range of ${r.end - r.start} at row ${row}`);
    assert.ok(r.start >= 0 && r.end <= BIG);
    assert.equal(r.start % COLS, 0, 'the range must start on a row boundary or the spacers misalign');
  }
});

// visibleRange decides what "the user is looking at" means, and the scroll harness
// above computes its holes counter from it. Tested only through that, a version that
// under-reported the viewport would report zero holes for the wrong reason and every
// assertion here would still pass while the real grid showed blank rows.
test('visibleRange covers every row the viewport touches, end-exclusive', () => {
  const at = (scrollTop, total = BIG) =>
    visibleRange({ scrollTop, viewportH: VIEWPORT_H, rowH: ROW_H, cols: COLS, total });

  // Flush at the top: 900px of viewport over 216px rows spans 0..900, touching rows
  // 0..4 (row 4 starts at 864). Five rows, end-exclusive.
  assert.deepEqual(at(0), { start: 0, end: 5 * COLS });

  // Scrolled 200px, still inside row 0: the viewport now spans 200..1100 and reaches
  // into row 5 (1080..1296). The partly visible row at each end has to be inside the
  // range, or its cards are the hole.
  assert.deepEqual(at(200), { start: 0, end: 6 * COLS });

  // Scrolled past whole rows: 2268 is halfway through row 10, and 2268..3168 ends
  // inside row 14.
  assert.deepEqual(at(2268), { start: 10 * COLS, end: 15 * COLS });

  // Both ends clamp to total rather than running past the end of the list.
  assert.deepEqual(at(0, 3), { start: 0, end: 3 });
  assert.deepEqual(at(ROW_H * 100, 30), { start: 30, end: 30 });

  // A viewport shorter than one row still covers the row it is inside.
  assert.deepEqual(
    visibleRange({ scrollTop: 0, viewportH: 10, rowH: ROW_H, cols: COLS, total: BIG }),
    { start: 0, end: COLS },
  );
});

test('spacers account for exactly the rows that are not live', () => {
  const total = 10000;
  const live = wantedRange({ scrollTop: 300 * ROW_H, rowH: ROW_H, cols: COLS, total, size: LIVE });
  const rows = spacerRows({ start: live.start, end: live.end, total, cols: COLS });
  const liveRows = Math.ceil((live.end - live.start) / COLS);
  assert.equal(rows.before + liveRows + rows.after, Math.ceil(total / COLS),
    'the spacers and the live rows must add up to the whole list, or the scrollbar lies');

  // A partial last row is where the ceil matters: 10003 items is 1667 rows of six with
  // one row holding a single card, and a floor here would leave the scrollbar a row short.
  const ragged = 10003;
  const tail = spacerRows({ start: 600, end: 900, total: ragged, cols: COLS });
  assert.equal(tail.before + Math.ceil((900 - 600) / COLS) + tail.after, Math.ceil(ragged / COLS));

  // Nothing live, and a live range covering everything, are both exact.
  assert.deepEqual(spacerRows({ start: 0, end: 0, total: ragged, cols: COLS }),
    { before: 0, after: Math.ceil(ragged / COLS) });
  assert.deepEqual(spacerRows({ start: 0, end: ragged, total: ragged, cols: COLS }),
    { before: 0, after: 0 });
});

test('needsRebuild is driven by margin, not by movement', () => {
  const live = { start: 1000, end: 2500 };
  const total = BIG;
  // Plenty of margin on both sides: no rebuild, however far from centred.
  assert.equal(needsRebuild({ live, view: { start: 1500, end: 1600 }, total }), false);
  // Running out below.
  assert.equal(needsRebuild({ live, view: { start: 1500, end: 2400 }, total }), true);
  // Running out above.
  assert.equal(needsRebuild({ live, view: { start: 1100, end: 1200 }, total }), true);
  // Against the start of the list, the missing margin above is not a reason.
  assert.equal(needsRebuild({ live: { start: 0, end: 1500 }, view: { start: 0, end: 100 }, total }), false);
  // Against the end of the list, likewise below.
  assert.equal(
    needsRebuild({ live: { start: 1000, end: total }, view: { start: total - 50, end: total }, total }),
    false,
  );
});

test('SLACK leaves room to scroll before the rebuild is due', () => {
  // A rebuild that fired with less than a screenful in hand would be visible as a
  // stutter, so the margin has to exceed what one viewport can cross.
  const perScreen = Math.ceil(VIEWPORT_H / ROW_H) * COLS;
  assert.ok(SLACK > perScreen, `slack of ${SLACK} items is under one screenful (${perScreen})`);
});
