// Deciding where an animation actually stops. Some libraries pad every clip to a fixed
// slot length — Quaternius Turn90 finishes at ~1.1s and then holds the final pose to
// 2.0s — so playing the stored duration ends with a second of a character standing
// still, and looping stalls there every cycle.
//
// Its own module, and free of three, because the decision is arithmetic over keyframe
// values and both ways of getting it wrong are silent: trim too eagerly and the end of
// a slow settle is cut off, trim too little and every card in an affected pack holds a
// pose. Nothing about that is visible in a thumbnail unless you already know what the
// clip was supposed to do, so it is checked by `node --test` instead of by eye.

// MOTION_FLOOR is the share of a track's own peak per-keyframe change that a keyframe
// has to exceed to count as motion. Relative rather than absolute because tracks are
// not in comparable units — a root position track moves in world units, a quaternion
// track in components of magnitude at most one — and because imperceptible
// end-of-clip jitter on a single bone should not hold the whole clip open.
export const MOTION_FLOOR = 0.05;

// STILL_TRACK is the peak below which a track is treated as never really moving, so a
// bone that is merely numerically noisy contributes no last-motion time at all.
export const STILL_TRACK = 1e-3;

// DEAD_TAIL is how much held pose has to be at the end before it is worth cutting.
// Under this, trimming would only shave a frame or two off clips that are already
// tight, and shortening a clip that was not padded is the worse error of the two.
export const DEAD_TAIL = 0.3;

// lastMotionTime returns the latest time at which any track still changes, or 0 when
// nothing does. tracks are plain {times, values, valueSize} objects — a THREE
// KeyframeTrack answers to that shape once getValueSize() is read off it — so this can
// be exercised without a browser or a loader.
export function lastMotionTime(tracks) {
  let lastMotion = 0;
  for (const tr of tracks || []) {
    const vs = tr.valueSize;
    const v = tr.values, times = tr.times, n = times ? times.length : 0;
    if (n < 2 || !vs) continue;
    // Per-keyframe change, and the track's peak change.
    let peak = 0;
    const chg = new Array(n).fill(0);
    for (let i = 1; i < n; i++) {
      let d = 0;
      for (let k = 0; k < vs; k++) d += Math.abs(v[i * vs + k] - v[(i - 1) * vs + k]);
      chg[i] = d;
      if (d > peak) peak = d;
    }
    if (peak < STILL_TRACK) continue;
    const thresh = peak * MOTION_FLOOR;
    // From the end: the first keyframe still moving is where this track stops.
    for (let i = n - 1; i > 0; i--) {
      if (chg[i] > thresh) { if (times[i] > lastMotion) lastMotion = times[i]; break; }
    }
  }
  return lastMotion;
}

// trimmedDuration returns the duration a clip of this length should play to, given
// where its tracks stop — or 0 when it should be left alone. Split from lastMotionTime
// so the "is the tail worth cutting" rule is one expression rather than a condition
// spread across the caller.
export function trimmedDuration(tracks, duration) {
  const lastMotion = lastMotionTime(tracks);
  if (lastMotion > 0 && duration - lastMotion > DEAD_TAIL) return lastMotion;
  return 0;
}
