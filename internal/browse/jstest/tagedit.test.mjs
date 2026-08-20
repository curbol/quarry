// What one tag edit does to one card's displayed tags. See the sibling gridwindow test
// for why these live outside assets/.
import test from 'node:test';
import assert from 'node:assert/strict';

import { nextTags } from '../assets/tagedit.js';

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
