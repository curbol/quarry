// The thumbnail worker's "is this job still wanted" decision. See the sibling
// gridwindow test for why these live outside assets/.
import test from 'node:test';
import assert from 'node:assert/strict';

import { JobTracker } from '../assets/jobtracker.js';

// worker replays the worker's dispatch against a tracker: jobs are queued in arrival
// order and each checks, at the moment it reaches the front, whether it is still the
// current request for its asset.
function worker() {
  const jobs = new JobTracker();
  const queued = [];
  let seq = 0;
  return {
    tracker: jobs,
    request(id) {
      const s = ++seq;
      jobs.note(id, s);
      queued.push({ id, seq: s });
      return s;
    },
    cancel(id) { jobs.cancel(id); },
    // run drains the queue and returns the jobs that actually rendered.
    run() {
      const ran = queued.filter((j) => jobs.isCurrent(j.id, j.seq));
      queued.length = 0;
      return ran;
    },
  };
}

test('a request that is never cancelled renders', () => {
  const w = worker();
  w.request('a');
  assert.equal(w.run().length, 1);
});

test('a cancelled request does not render', () => {
  const w = worker();
  w.request('a');
  w.cancel('a');
  assert.equal(w.run().length, 0);
});

test('cancel then re-request renders exactly once', () => {
  // The regression this exists for. A grid rebuild cancels every pending id and the
  // observer replacing it immediately re-requests whatever is still on screen. Under
  // an id-keyed cancel set the re-request cleared the cancel for the job already
  // queued, so both ran — and the second result displaced the first's cache entry,
  // orphaning the object URL the visible image was using.
  const w = worker();
  w.request('a');
  w.cancel('a');
  const fresh = w.request('a');

  const ran = w.run();
  assert.equal(ran.length, 1, 'the superseded job rendered as well as the fresh one');
  assert.equal(ran[0].seq, fresh, 'the job that rendered was not the newest request');
});

test('re-requesting without a cancel also renders once', () => {
  // Two observers can ask for the same still-visible card without a cancel between.
  const w = worker();
  w.request('a');
  const fresh = w.request('a');
  const ran = w.run();
  assert.equal(ran.length, 1);
  assert.equal(ran[0].seq, fresh);
});

test('a whole-grid cancel drops every pending job and keeps the survivors', () => {
  const w = worker();
  for (const id of ['a', 'b', 'c']) w.request(id);
  for (const id of ['a', 'b', 'c']) w.cancel(id);
  // Only 'b' is still on screen after the rebuild.
  const fresh = w.request('b');

  const ran = w.run();
  assert.deepEqual(ran, [{ id: 'b', seq: fresh }]);
});

test('one asset in flight does not affect another', () => {
  const w = worker();
  const a = w.request('a');
  w.request('b');
  w.cancel('b');
  const ran = w.run();
  assert.deepEqual(ran, [{ id: 'a', seq: a }]);
});

test('the tracker holds one entry per wanted asset, not per id ever seen', () => {
  // The set this replaced never shrank for a job that finished before its cancel
  // arrived, so it grew one retained string per asset over a long scroll.
  const w = worker();
  for (let i = 0; i < 1000; i++) {
    w.request('asset-' + i);
    w.cancel('asset-' + i);
  }
  w.run();
  assert.equal(w.tracker.size, 0, 'cancelled assets are still being tracked');

  w.request('still-here');
  assert.equal(w.tracker.size, 1);
});

test('a result is matched to its own request, not merely to its asset', () => {
  // What the client does with the echoed sequence number: a result whose request was
  // superseded is dropped before an object URL is ever created for it.
  const jobs = new JobTracker();
  jobs.note('a', 1);
  jobs.note('a', 2);
  assert.equal(jobs.isCurrent('a', 1), false, 'a stale result was accepted');
  assert.equal(jobs.isCurrent('a', 2), true);
  assert.equal(jobs.isCurrent('never-asked', 1), false);
});
