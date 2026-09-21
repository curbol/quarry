import test from 'node:test';
import assert from 'node:assert/strict';

// charstore's shim falls back to memory when localStorage will not take a write. The
// fallback is a module-level latch, so this lives in its own file: node --test runs each
// file in its own process, and a test that trips the latch would otherwise decide every
// test after it in the same one.
//
// The failure it covers is the read and the write disagreeing. localStorage present but
// setItem throwing — a quota exhausted, a browser with site data blocked, Safari private
// browsing — sent the write to memory while the read kept going to localStorage and
// handed back the value from before it. Every save vanished with nothing reported, and
// resolveRig's eviction could not make progress because match() kept being handed the
// entry it had just removed.
//
// Installed before the import, since the shim reads localStorage at call time but picks
// its bound at module load.
let thrown = 0;
const backing = new Map();
globalThis.localStorage = {
  getItem: (k) => (backing.has(k) ? backing.get(k) : null),
  setItem: () => { thrown++; throw new Error('QuotaExceededError'); },
  removeItem: (k) => backing.delete(k),
};

const { CharRegistry, resolveRig } = await import('../assets/charstore.js');

const BONES = Array.from({ length: 12 }, (_, i) => 'Bone' + i);
const rig = (id, extra = {}) => ({ id, bones: BONES, ...extra });

test('a write localStorage refuses is still readable back', () => {
  // The value localStorage would keep handing back: what was there before the write.
  backing.set(CharRegistry.key, JSON.stringify([rig('stale')]));

  assert.equal(CharRegistry.add(rig('fresh')), true);
  assert.ok(thrown > 0, 'the fixture never exercised the throwing setItem');

  const ids = CharRegistry.list().map((e) => e.id);
  assert.ok(ids.includes('fresh'), `the saved entry is not readable back: ${ids}`);
});

test('a removal a refused write cannot persist still takes effect', () => {
  CharRegistry.save([rig('a'), rig('b')]);
  CharRegistry.remove('a');
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['b'],
    'remove() reported success while list() kept serving the pre-removal value');
});

// The whole reason the two halves have to agree: resolveRig evicts a candidate that will
// not load and asks the registry again. With the read behind the write, match() returns
// the same entry forever — an unbounded stream of failing loads in the lightbox, and in
// the worker a spin the job deadline does not stop, since that only stops a job being
// waited on. tried is what bounds the walk now, so this holds even here.
test('the rig search terminates even when the registry cannot persist an eviction', async () => {
  CharRegistry.save([rig('a', { vendor: 'acme' }), rig('b', { vendor: 'acme' }), rig('c', { vendor: 'acme' })]);
  CharRegistry.seed = async () => {};
  CharRegistry.registerNamed = async () => {};
  CharRegistry.discoverForVendor = async () => {};
  // Nothing takes: the shape a store that cannot persist leaves the loop in.
  CharRegistry.remove = () => {};

  let tries = 0;
  const got = await resolveRig(BONES, { vendor: 'acme', name: 'Walk' }, async () => {
    tries++;
    if (tries > 50) throw new Error(`unbounded: tryLoad called ${tries} times with no progress`);
    return null;
  }, () => false);

  assert.equal(got, null);
  assert.ok(tries > 0, 'the search never reached a candidate; this fixture proves nothing');
  assert.ok(tries <= 50, 'the search did not terminate');
});

// The bound the page actually runs under. store.limit is picked at module load from
// whether localStorage exists, so this is the only process that sees STORED_MAX —
// charstore.test.mjs runs the no-localStorage branch and pins MEMORY_MAX, ten times
// larger. Without an assertion here, STORED_MAX is held up by nothing but being
// smaller than the constant the other file checks, and a change collapsing the two
// leaves the whole suite green. What it costs is the reason the smaller bound exists:
// the registry is one localStorage value, and a few hundred skeletons of bone-name
// arrays grow it until setItem throws, after which every save is silently dropped.
test('the registry is bounded by the stored limit, not the memory one', async () => {
  const { STORED_MAX, MEMORY_MAX } = await import('../assets/charstore.js');
  assert.ok(STORED_MAX < MEMORY_MAX, 'the stored bound must be the tighter of the two');

  CharRegistry.save([]);
  for (let i = 0; i < STORED_MAX + 10; i++) CharRegistry.add(rig('r' + i));

  const ids = CharRegistry.list().map((e) => e.id);
  assert.equal(ids.length, STORED_MAX,
    `the registry holds ${ids.length} entries on the localStorage branch, want ${STORED_MAX}`);
  // Most recent first, so the newest survive and the oldest are the ones dropped.
  assert.equal(ids[0], 'r' + (STORED_MAX + 9));
  assert.ok(!ids.includes('r0'), 'the oldest entry outlived the bound');
});
