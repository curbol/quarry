import test from 'node:test';
import assert from 'node:assert/strict';
import { CharRegistry, contentURL, thumbURL } from '../assets/charstore.js';

// Node has no localStorage, which is the same branch the thumbnail worker takes, so
// these run against the in-memory fallback the shim provides for exactly that case.
function reset() {
  CharRegistry.save([]);
}

function rig(id, extra = {}) {
  return { id, bones: Array.from({ length: 12 }, (_, i) => 'Bone' + i), ...extra };
}

test('the URL builders escape the id', () => {
  assert.equal(contentURL('a b&c'), '/api/content?id=a%20b%26c');
  assert.equal(thumbURL('a/b'), '/api/thumb?id=a%2Fb');
});

test('add reports whether what the matcher can pick actually changed', () => {
  reset();
  assert.equal(CharRegistry.add(rig('a')), true, 'a new entry is a change');
  assert.equal(CharRegistry.add(rig('a')), false, 'the identical entry again is not');
  assert.equal(CharRegistry.add(rig('a', { vendor: 'synty' })), true, 'a changed field is');
});

test('too few bones is not a rig', () => {
  reset();
  assert.equal(CharRegistry.add({ id: 'thin', bones: ['Root'] }), false);
  assert.equal(CharRegistry.list().length, 0);
});

// rigEntry rebuilds an entry from the model and knows nothing about pinning, so
// re-registering a character the user pinned must not quietly un-pin it.
test('re-registering carries the pinned flag across', () => {
  reset();
  CharRegistry.add(rig('a'));
  assert.equal(CharRegistry.pin('a', true), true, 'pin reports the flag moved');
  assert.equal(CharRegistry.pin('a', true), false, 'and reports when it did not');
  CharRegistry.add(rig('a', { vendor: 'synty' }));
  assert.equal(CharRegistry.isPinned('a'), true);
});

test('the most recent entry leads, and remove takes only its own', () => {
  reset();
  CharRegistry.add(rig('a'));
  CharRegistry.add(rig('b'));
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['b', 'a']);
  CharRegistry.add(rig('a', { vendor: 'v' }));
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['a', 'b'], 're-adding moves it to the front, not a duplicate');
  CharRegistry.remove('a');
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['b']);
});

// The store is what a scroll and a session accumulate into, and it is a single
// localStorage value: unbounded, it grows until a save throws and every later one is
// silently dropped.
test('the registry is bounded', () => {
  reset();
  for (let i = 0; i < 50; i++) CharRegistry.add(rig('c' + i));
  assert.equal(CharRegistry.list().length, 40);
  assert.equal(CharRegistry.list()[0].id, 'c49', 'the newest survives the trim, not the oldest');
});

// A value that will not parse has to read as an empty registry rather than throwing out
// of every caller — list() is on the path of every card that needs a body — and it has
// to say so, because the next save overwrites it and the user's pinned characters go
// with it. The shim reads localStorage when there is one, which is also the branch the
// page takes, so this is where a truncated or hand-edited value actually arrives.
test('a corrupt stored value reads as empty rather than throwing, and says so', () => {
  const backing = new Map();
  globalThis.localStorage = {
    getItem: (k) => (backing.has(k) ? backing.get(k) : null),
    setItem: (k, v) => backing.set(k, String(v)),
  };
  const warned = [];
  const prevWarn = console.warn;
  console.warn = (...args) => warned.push(args);
  try {
    CharRegistry.add(rig('a'));
    assert.equal(CharRegistry.list().length, 1, 'the fake storage is the one being read');
    backing.set(CharRegistry.key, '[{"id":"a",');
    assert.deepEqual(CharRegistry.list(), []);
  } finally {
    console.warn = prevWarn;
    delete globalThis.localStorage;
  }
  assert.equal(warned.length, 1, 'silently it is indistinguishable from a fresh profile');
});

// The ranking itself is rigmatch.js's, checked there. What is pinned here is that the
// registry hands it the stored list and its own vendor scoping, so a stored body is
// reachable at all — and that a body from another vendor is not, since cross-vendor
// skeletons share enough bone names to pass the coverage bar with a different rest pose.
test('match picks a stored body that covers the clip, within its vendor', () => {
  reset();
  const clipBones = Array.from({ length: 12 }, (_, i) => 'Bone' + i);
  CharRegistry.add({ id: 'body', vendor: 'synty', name: 'Character_Model', bones: clipBones.concat(['Prop', 'Cape']) });
  const got = CharRegistry.match(clipBones, 'synty', 'Character@Walk');
  assert.ok(got && got.id === 'body', `match = ${JSON.stringify(got)}`);
  assert.equal(CharRegistry.match(clipBones, 'kevdev', 'Character@Walk'), null, 'another vendor\'s body is not a match');
  assert.equal(CharRegistry.match([], 'synty', 'Character@Walk'), null, 'a clip with no bones asks for nothing');
});

test('hasNamed asks about the body a clip is named after', () => {
  reset();
  assert.equal(CharRegistry.hasNamed({ vendor: 'synty', name: 'HumanM@CombatIdle' }), false);
  CharRegistry.add(rig('m', { vendor: 'synty', name: 'HumanM_Model' }));
  assert.equal(CharRegistry.hasNamed({ vendor: 'synty', name: 'HumanM@CombatIdle' }), true);
  assert.equal(CharRegistry.hasNamed({ vendor: 'kevdev', name: 'HumanM@CombatIdle' }), false);
});
