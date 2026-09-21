// What one tag edit does to one card's displayed tags. See the sibling gridwindow test
// for why these live outside assets/.
import test from 'node:test';
import assert from 'node:assert/strict';

import { foldTagEdit, nextTags } from '../assets/tagedit.js';

const apply = (o) => nextTags({ tag: 'hero', ...o });

test('adding is certain the moment the card shares a fingerprint with the edit', () => {
  assert.deepEqual(
    apply({ cardFingerprints: ['a', 'b'], cardTags: [], edited: ['a'], on: true }),
    ['hero'],
  );
  // Already present, and not duplicated.
  assert.deepEqual(
    apply({ cardFingerprints: ['a'], cardTags: ['hero'], edited: ['a'], on: true }),
    ['hero'],
  );
});

test('removing needs every one of the card\'s fingerprints in the edit', () => {
  // A grouped card is several byte-identical copies. Untagging one of them says nothing
  // about the others, and the card's tags are the union — so the tag stays.
  assert.deepEqual(
    apply({ cardFingerprints: ['a', 'b'], cardTags: ['hero'], edited: ['a'], on: false }),
    ['hero'],
  );
  assert.deepEqual(
    apply({ cardFingerprints: ['a', 'b'], cardTags: ['hero'], edited: ['a', 'b'], on: false }),
    [],
  );
});

test('an edit wider than the card still clears it', () => {
  // Untagging a whole set from the lightbox: the edit covers fingerprints this card
  // does not have, which is not a reason to keep the tag on the ones it does.
  assert.deepEqual(
    apply({ cardFingerprints: ['a'], cardTags: ['hero', 'wip'], edited: ['a', 'b', 'c'], on: false }),
    ['wip'],
  );
});

test('the card\'s other tags survive, sorted', () => {
  assert.deepEqual(
    apply({ cardFingerprints: ['a'], cardTags: ['wip', 'biome:forest'], edited: ['a'], on: true }),
    ['biome:forest', 'hero', 'wip'],
  );
});

test('a card with no fingerprints is never cleared by someone else\'s edit', () => {
  // [].every() is true, so an unguarded implementation drops the tag from a card the
  // edit never touched. Only reachable through a card with no fingerprints at all, but
  // that is exactly the shape a fingerprint-less asset produces.
  assert.deepEqual(
    apply({ cardFingerprints: [], cardTags: ['hero'], edited: ['a'], on: false }),
    ['hero'],
  );
});

// The guard has two halves and only the empty-list one was covered. A card whose
// fingerprints are absent altogether — an asset whose content could not be read, which
// the server sends with no fingerprint at all — reaches the same place, and without the
// truthiness test every() on undefined throws rather than leaving the tag alone.
test('a card with no fingerprint field is left alone by someone else\'s removal', () => {
  assert.deepEqual(
    nextTags({ cardFingerprints: undefined, cardTags: ['hero', 'wip'], edited: ['fp1'], tag: 'hero', on: false }),
    ['hero', 'wip'],
  );
  // And the adding half does not depend on the card's fingerprints at all: the caller
  // has already established the card is in the edit.
  assert.deepEqual(
    nextTags({ cardFingerprints: undefined, cardTags: [], edited: ['fp1'], tag: 'hero', on: true }),
    ['hero'],
  );
});

// foldTagEdit: which cards an edit reaches, and how often each is touched.
const card = (fingerprints, tags = []) => ({ fingerprints, tags });
const index = (pairs) => new Map(pairs.map(([fp, items]) => [fp, new Set(items)]));

test('an entry whose card the grid recycled out is still folded into', () => {
  const offscreen = card(['a'], []);
  const onscreen = card(['a'], []);
  const watcher = { asset: onscreen, repaint() {} };
  const repaint = foldTagEdit({
    holders: index([['a', [offscreen, onscreen]]]),
    watchers: index([['a', [watcher]]]),
    fingerprints: ['a'],
    tag: 'hero',
    on: true,
  });
  assert.deepEqual(offscreen.tags, ['hero'], 'the entry with no card kept its stale tags');
  assert.deepEqual(onscreen.tags, ['hero']);
  assert.deepEqual(repaint, [watcher]);
});

test('a card is folded once however many of its fingerprints the edit names', () => {
  // Both of this card's fingerprints are in the edit, so the removal is certain — but
  // only while the second pass is not deciding against the tags the first pass wrote.
  const both = card(['a', 'b'], ['hero']);
  foldTagEdit({
    holders: index([['a', [both]], ['b', [both]]]),
    watchers: new Map(),
    fingerprints: ['a', 'b'],
    tag: 'hero',
    on: false,
  });
  assert.deepEqual(both.tags, [], 'a card the edit fully covers kept the tag');
});

test('each watcher is listed once, whatever it is registered under', () => {
  const asset = card(['a', 'b'], []);
  const watcher = { asset, repaint() {} };
  const repaint = foldTagEdit({
    holders: new Map(),
    watchers: index([['a', [watcher]], ['b', [watcher]]]),
    fingerprints: ['a', 'b'],
    tag: 'hero',
    on: true,
  });
  assert.equal(repaint.length, 1, 'a card sharing two edited fingerprints repaints twice');
});

test('an edit naming a fingerprint nothing holds changes and repaints nothing', () => {
  const other = card(['a'], ['hero']);
  const repaint = foldTagEdit({
    holders: index([['a', [other]]]),
    watchers: index([['a', [{ asset: other, repaint() {} }]]]),
    fingerprints: ['z'],
    tag: 'hero',
    on: false,
  });
  assert.deepEqual(other.tags, ['hero']);
  assert.deepEqual(repaint, []);
});
