import test from 'node:test';
import assert from 'node:assert/strict';
import { lastMotionTime, trimmedDuration, DEAD_TAIL, STILL_TRACK } from '../assets/cliptrim.js';

// A track that moves steadily for `moving` frames and then holds its last value.
// valueSize 1 keeps the arithmetic readable; the code is per-component either way.
function padded({ frames, moving, step = 1, valueSize = 1, dt = 0.1 }) {
  const times = [], values = [];
  let v = 0;
  for (let i = 0; i < frames; i++) {
    times.push(i * dt);
    if (i > 0 && i < moving) v += step;
    for (let k = 0; k < valueSize; k++) values.push(v);
  }
  return { times, values, valueSize };
}

test('a clip that holds its final pose reports the frame it stopped moving', () => {
  // Moves through frame 5 (t=0.5), then holds to frame 20 (t=2.0).
  const tr = padded({ frames: 21, moving: 6 });
  assert.equal(lastMotionTime([tr]).toFixed(2), '0.50');
});

test('a clip moving to its last frame is not trimmed', () => {
  const tr = padded({ frames: 21, moving: 21 });
  assert.equal(lastMotionTime([tr]).toFixed(2), '2.00');
  assert.equal(trimmedDuration([tr], 2.0), 0, 'nothing to cut, so no new duration');
});

test('the latest-stopping track decides, not the first', () => {
  const early = padded({ frames: 21, moving: 4 });  // stops at t=0.3
  const late = padded({ frames: 21, moving: 12 });  // stops at t=1.1
  assert.equal(lastMotionTime([early, late]).toFixed(2), '1.10');
  assert.equal(lastMotionTime([late, early]).toFixed(2), '1.10', 'order must not matter');
});

// The threshold is a share of each track's own peak, which is what lets a big mover and
// a jittery bone coexist: an absolute floor would either ignore the quiet track
// entirely or let the noisy one hold every clip open.
test('end-of-clip jitter far below a track\'s own peak does not count as motion', () => {
  const tr = padded({ frames: 21, moving: 6, step: 1 });
  // A 1% wobble on the final frame, against a peak per-frame change of 1.
  tr.values[20] = tr.values[19] + 0.01;
  assert.equal(lastMotionTime([tr]).toFixed(2), '0.50', 'jitter under the floor is not the end');
});

test('a late change above the floor is the end, even when it is small', () => {
  const tr = padded({ frames: 21, moving: 6, step: 1 });
  tr.values[20] = tr.values[19] + 0.5; // half the peak, well over the 5% floor
  assert.equal(lastMotionTime([tr]).toFixed(2), '2.00');
});

test('a track that never really moves contributes nothing', () => {
  const still = padded({ frames: 21, moving: 21, step: 1e-6 });
  assert.equal(lastMotionTime([still]), 0);
  // ...and cannot drag a real track's answer later than it belongs.
  const real = padded({ frames: 21, moving: 6 });
  assert.equal(lastMotionTime([still, real]).toFixed(2), '0.50');
  // Nor can it produce a trim of its own: a clip where nothing moves is left at its
  // stored duration rather than cut to nothing.
  assert.equal(trimmedDuration([still], 2.0), 0, 'a clip where nothing moves is left alone');
});

// What STILL_TRACK has to separate, in absolute terms rather than in terms of itself:
// float noise on a bone that is not animated, from a real but small rotation. Written
// against the constant, this test would move with it and pin nothing — and both
// directions are silent. Too high and a subtle hand or finger animation is discarded as
// noise, so the clip is reported as stopping before it does; too low and a numerically
// noisy bone holds every clip in a padded pack open to its full slot length.
test('float noise is not motion, but a small real rotation is', () => {
  // Peak per-keyframe change is exactly `step` for a single-component track.
  const noise = padded({ frames: 21, moving: 6, step: 1e-5 });
  assert.equal(lastMotionTime([noise]), 0, '1e-5 per frame is a bone that is not animated');

  const subtle = padded({ frames: 21, moving: 6, step: 1e-2 });
  assert.equal(lastMotionTime([subtle]).toFixed(2), '0.50', '1e-2 per frame is a real, small rotation');

  // And the constant itself sits between them, so the two claims above are about it.
  assert.ok(STILL_TRACK > 1e-5 && STILL_TRACK <= 1e-2, `STILL_TRACK = ${STILL_TRACK}`);
});

test('a dead tail is only cut when it is worth cutting', () => {
  const tr = padded({ frames: 21, moving: 6 }); // stops at 0.5
  // Comfortably past the floor: cut.
  assert.equal(trimmedDuration([tr], 2.0).toFixed(2), '0.50');
  // Just under it: a clip that was not padded must not be shortened.
  assert.equal(trimmedDuration([tr], 0.5 + DEAD_TAIL - 0.01), 0);
  // Just over it: cut.
  assert.equal(trimmedDuration([tr], 0.5 + DEAD_TAIL + 0.01).toFixed(2), '0.50');
});

test('degenerate tracks are ignored rather than throwing', () => {
  assert.equal(lastMotionTime([]), 0);
  assert.equal(lastMotionTime(null), 0);
  assert.equal(lastMotionTime([{ times: [0], values: [0], valueSize: 1 }]), 0, 'one keyframe is not motion');
  assert.equal(lastMotionTime([{ times: [], values: [], valueSize: 1 }]), 0);
  assert.equal(lastMotionTime([{ times: [0, 1], values: [0, 1], valueSize: 0 }]), 0, 'no components to compare');
});

test('multi-component tracks sum their components', () => {
  // A quaternion-shaped track: four components, each moving a little.
  const tr = padded({ frames: 11, moving: 4, step: 0.25, valueSize: 4 });
  assert.equal(lastMotionTime([tr]).toFixed(2), '0.30');
});
