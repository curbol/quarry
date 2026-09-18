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
function setup(ids, { discover = () => {} } = {}) {
  CharRegistry.save(ids.map((id) => ({ id, bones: BONES, vendor: 'acme' })));
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
