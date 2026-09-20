import test from 'node:test';
import assert from 'node:assert/strict';
import { CharRegistry, resolveRig } from '../assets/charstore.js';

// resolveRig walks the registry, evicting an entry that will not load and falling
// through to vendor discovery when nothing does. seed, registerNamed and
// discoverForVendor are composed onto CharRegistry by scene.js, which needs THREE and a
// server; here they are stubs, which is the whole reason this is checkable at all.
//
// What these pin is the difference between "this entry does not load" and "nobody is
// waiting for this search any more". Both reach the loop as a falsy tryLoad, and
// treating the second as the first empties the registry of every body that covers the
// skeleton — the failure that left a scrolled-past clip card's whole pack blank.

const BONES = Array.from({ length: 12 }, (_, i) => 'Bone' + i);

// setup seeds the registry and the three methods scene.js supplies, recording what the
// search asked for so a test can assert an abandoned one asked for nothing.
function setup(ids, { discover = () => {}, name } = {}) {
  CharRegistry.save(ids.map((id) => ({ id, bones: BONES, vendor: 'acme', name })));
  const calls = { named: 0, discovered: 0 };
  CharRegistry.seed = async () => {};
  CharRegistry.registerNamed = async () => { calls.named++; };
  CharRegistry.discoverForVendor = async () => { calls.discovered++; discover(); };
  return calls;
}

const asset = { vendor: 'acme', name: 'Walk' };
const never = () => false;

test('a candidate that will not load is evicted and the search continues', async () => {
  setup(['a', 'b', 'c']);
  const tried = [];
  const got = await resolveRig(BONES, asset, async (m) => {
    tried.push(m.id);
    return m.id === 'c' ? 'rig-c' : null;
  }, never);
  assert.equal(got, 'rig-c');
  assert.deepEqual(tried, ['a', 'b', 'c']);
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['c'], 'only the loadable one survives');
});

// The regression this file exists for. An abandoned job's tryLoad returns falsy for
// every remaining candidate; evicting on that is what wiped the registry.
test('a search abandoned mid-load evicts nothing', async () => {
  const calls = setup(['a', 'b', 'c']);
  let abandoned = false;
  const got = await resolveRig(BONES, asset, async () => {
    abandoned = true; // the job was cancelled while this load was in flight
    return null;
  }, () => abandoned);
  assert.equal(got, null);
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['a', 'b', 'c'], 'the registry is untouched');
  assert.equal(calls.discovered, 0, 'and an abandoned search does not pay for discovery');
});

// Cancellation between attempts is not enough on its own: the tear-down lands during
// the await, so the check has to be after it as well as in the loop condition.
test('cancelling before the search starts does nothing at all', async () => {
  const calls = setup(['a']);
  let loads = 0;
  const got = await resolveRig(BONES, asset, async () => { loads++; return null; }, () => true);
  assert.equal(got, null);
  assert.equal(loads, 0);
  assert.equal(calls.named, 0);
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['a']);
});

// registerNamed is a search and a load, paid once per vendor and series. hasNamed is the
// cheap question asked first, and every other test here leaves the entries unnamed — so
// the branch that skips the search was only ever measured from the side that runs it.
test('the named body already being registered skips the search for it', async () => {
  const calls = setup(['a'], { name: 'Walk_Model' });
  assert.equal(await resolveRig(BONES, asset, async () => 'rig', never), 'rig');
  assert.equal(calls.named, 0, 'the body the clip is named after is already here');

  const missing = setup(['a'], { name: 'Other_Model' });
  assert.equal(await resolveRig(BONES, asset, async () => 'rig', never), 'rig');
  assert.equal(missing.named, 1, 'and is looked up when it is not');
});

// Tear-down does not wait for a convenient moment. The check after each await is what
// stops an abandoned search paying for the next step — and the one after discovery is
// what stops it running attempt() a second time against a registry it is no longer
// allowed to evict from. Cancelling before the search starts returns at the first check
// and reaches neither.
test('cancellation landing inside a step stops the one after it', async () => {
  let cancelled = false;
  const named = setup(['a']);
  CharRegistry.registerNamed = async () => { named.named++; cancelled = true; };
  let loads = 0;
  assert.equal(await resolveRig(BONES, asset, async () => { loads++; return null; }, () => cancelled), null);
  assert.equal(named.named, 1);
  assert.equal(loads, 0, 'a search torn down during registerNamed still went on to load');
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['a'], 'and evicted nothing');

  cancelled = false;
  const disc = setup(['a']);
  let attempts = 0;
  CharRegistry.discoverForVendor = async () => {
    disc.discovered++;
    CharRegistry.add({ id: 'found', bones: BONES, vendor: 'acme' });
    cancelled = true;
  };
  assert.equal(await resolveRig(BONES, asset, async () => { attempts++; return null; }, () => cancelled), null);
  assert.equal(disc.discovered, 1);
  assert.equal(attempts, 1, 'the retry after discovery ran for a search nobody was waiting on');
  // The first pass evicted 'a' legitimately — it would not load and the search was
  // still wanted. What must survive is what discovery just found: a retry nobody is
  // waiting on would walk it and evict it as unloadable, and the memos recording the
  // vendor as searched stay set, so nothing finds it again this session.
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['found']);
});

// Discovery runs only when the registry holds nothing that fits, and what it registers
// is searched in turn — the ordering one of the two hand-written copies got wrong.
test('an empty registry falls through to discovery and then retries', async () => {
  const calls = setup([], { discover: () => CharRegistry.add({ id: 'found', bones: BONES, vendor: 'acme' }) });
  const got = await resolveRig(BONES, asset, async (m) => (m.id === 'found' ? 'rig' : null), never);
  assert.equal(got, 'rig');
  assert.equal(calls.discovered, 1);
});

test('a registry that already answers never pays for discovery', async () => {
  const calls = setup(['a']);
  assert.equal(await resolveRig(BONES, asset, async () => 'rig', never), 'rig');
  assert.equal(calls.discovered, 0);
});

// The two callers disagree about what tryLoad returns when it is superseded, by design:
// the worker returns null, the viewer returns true. That is why `cancelled` is required
// rather than defaulted — a default would silently pick one convention and read the
// other's answer backwards. Every stub above returns a rig or null, which is the
// worker's half; this is the viewer's, and it had no test.
//
// A truthy return means "this played, stop here". Nothing is stale, so nothing may be
// evicted, and no search may start: the viewer is already showing something else.
test('a truthy result from a superseded load ends the search and touches nothing', async () => {
  const calls = setup(['a', 'b', 'c']);
  const tried = [];
  const got = await resolveRig(BONES, asset, async (m) => {
    tried.push(m.id);
    return true; // the viewer's convention: superseded, and it says so by succeeding
  }, never);

  assert.equal(got, true, 'the caller\'s own sentinel comes back to it');
  assert.deepEqual(tried, ['a'], 'the search stopped at the first candidate');
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['a', 'b', 'c'], 'nothing was evicted');
  // registerNamed runs ahead of the first load, so it is expected here; discovery is
  // the expensive one and it must not run once something has answered.
  assert.equal(calls.discovered, 0, 'a vendor-wide model search ran even though a candidate answered');
});
