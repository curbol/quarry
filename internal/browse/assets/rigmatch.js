// The decisions behind matching a mesh-less animation clip to a body that can play it,
// separated from the three.js code that acts on them.
//
// Every function here is a pure function of plain values — arrays of bone names,
// registry entries, an asset descriptor — so they can be checked without a WebGL
// context. That matters more here than the arithmetic in gridwindow.js: getting one of
// these subtly wrong does not break the page, it previews a plausible-looking but wrong
// animation, or poses a clip onto a rig it does not fit and shows a shredded body.
// Nobody eyeballs 150k thumbnails to catch that.

// clipsForAsset returns the animation clips to preview for an asset, given the file's
// own clips. A per-clip virtual asset (source.clip, from splitting a multi-animation
// model file like a Quaternius library) narrows to just that named clip; every other
// asset previews all of the file's clips. FBX prefixes a clip name with its take
// ("Armature|Walk"), so match the suffix too.
export function clipsForAsset(all, asset) {
  all = all || [];
  const src = (asset && asset.source) || {};
  // clipIndex is the animation's position in the file, which is how the split was made.
  // The name cannot do this job on its own: glTF names are optional and need not be
  // unique, so the index carries a disambiguated label ("Walk (2)", "clip 3") that no
  // animation in the file is actually called.
  if (Number.isInteger(src.clipIndex) && all[src.clipIndex]) return [all[src.clipIndex]];
  const want = src.clip;
  if (!want) return all;
  const hit = all.filter((c) => c.name === want || c.name.endsWith('|' + want));
  return hit.length ? hit : all;
}

// clipsMatching is clipsForAsset against a *different* file: the root-motion sibling,
// which has its own animations and its own indices. Only a name match counts, plus the
// whole-file case where the sibling holds exactly one clip and there is nothing to pick
// wrong. Falling back to "all of them" the way clipsForAsset does would hand the toggle
// an arbitrary animation and present it as this clip's travel variant.
export function clipsMatching(all, asset) {
  all = all || [];
  const want = asset && asset.source && asset.source.clip;
  if (!want) return all;
  const hit = all.filter((c) => c.name === want || c.name.endsWith('|' + want));
  if (hit.length) return hit;
  return all.length === 1 ? all : [];
}

// clipBaseName strips the FBX take prefix, so "Take 001|Walk" and "Walk" are the same
// animation under two exporters. clipsForAsset and clipsMatching compare the same way.
function clipBaseName(c) {
  const n = (c && c.name) || '';
  const bar = n.lastIndexOf('|');
  return bar < 0 ? n : n.slice(bar + 1);
}

// pairClipsByName aligns a file's animations with the same animations in its
// root-motion sibling, returning for each clip of `a` the index of its counterpart in
// `b`, or -1 where there is none.
//
// The toggle needs this because the two lists come out of two different files. A card
// for a whole animation file carries no clip name, so clipsMatching hands back every
// clip the sibling holds, and the lists then share neither length nor order. Swapping
// between them by position picked whatever sat at that index: with fewer clips on the
// other side the viewer silently fell back to the first one and lost the user's place,
// and with the same count in a different order it played one animation while presenting
// it as another's travel variant — a clip that loads, on the right body, wrong.
//
// Position is the fallback only when the two hold the same number of clips and no name
// matched at all, which is what a pair exported with unnamed or mechanically renamed
// takes looks like. A partial match keeps its -1s: a clip with no counterpart has no
// travel variant to offer, and offering one anyway is the failure this exists to stop.
//
// A name is consumed once it is taken, so a file holding two clips under one name — or
// two unnamed ones, which is the same thing here — pairs them off in order rather than
// sending both to the first. Handing every duplicate the same counterpart is the
// position bug again, wearing a name.
export function pairClipsByName(a, b) {
  a = a || [];
  b = b || [];
  const free = new Map();
  b.forEach((c, i) => {
    const k = clipBaseName(c);
    const q = free.get(k);
    if (q) q.push(i);
    else free.set(k, [i]);
  });
  const out = a.map((c) => {
    const q = free.get(clipBaseName(c));
    return q && q.length ? q.shift() : -1;
  });
  if (a.length === b.length && out.every((i) => i < 0)) return a.map((_, i) => i);
  return out;
}

// clipAcross is where the toggle lands: the counterpart pairClipsByName found, or the
// current index when there is none. It sits here rather than at the call site because
// it is the last step between a correct pairing and the wrong animation being shown as
// this one's travel variant, and it is the same arithmetic the pairing itself exists to
// get right — read the pairs the wrong way round and the sibling list's own index is
// used against the in-place list.
//
// pairs is always the mapping *from* the side being left. The caller flips which list
// that is before asking, so the two are never the same array on two consecutive calls.
export function clipAcross(pairs, curClip) {
  const next = (pairs || [])[curClip];
  return next >= 0 ? next : curClip;
}

// togglePairable reports whether the toggle has anywhere to go for this clip. An empty
// mapping means the other side was never split into clips at all — a whole animation
// file, where every clip is reachable — so the toggle stands; otherwise it needs a
// counterpart, because offering one that does not exist is how a clip gets presented as
// another's travel variant.
export function togglePairable(pairs, curClip) {
  return !(pairs || []).length || (pairs || [])[curClip] >= 0;
}

// coversBones reports how many bone names a rig and a clip share when the rig can
// actually play the clip, and 0 when it cannot: the clip must drive most of the rig
// (≥60% of the rig's bones — so nearly the whole body animates) AND cover a good part
// of the clip (≥45% of its bones). Requiring both rejects the small/partial rigs that
// share a handful of names but pose a full clip into garbage (the shredded animation
// thumbnails), and superset showcase rigs, while still letting a body play a richer
// clip.
//
// The shared count is returned rather than a bare true because ranking candidates
// wants it too, and a call site that recomputed the overlap to get it would end up
// holding a second copy of these thresholds. Names are counted distinct on both sides:
// a cached entry can repeat a name per bone instance, and coverage is about which
// bones are driven, not how many times.
export function coversBones(have, want) {
  if (!have || !have.length || !want || !want.length) return 0;
  const rig = new Set(have);
  const clip = new Set(want);
  let hit = 0;
  for (const b of clip) if (rig.has(b)) hit++;
  if (hit / rig.size < 0.6 || hit / clip.size < 0.45) return 0;
  return hit;
}

// matchRig picks the registered character whose skeleton best covers a clip's bones.
// A pinned character that covers the clip wins over a higher-coverage unpinned one, so
// pinning a body for a rig makes it the default for every clip on that rig.
//
// clipName breaks the tie a pack of body variants otherwise loses on coverage: the bodies
// share one skeleton, so each covers every clip exactly as well as the others, and which
// one a clip lands on comes down to what was registered first. A body named for the
// clip's own series is the one the pack means (HumanM_Model for HumanM@CombatIdle1H01),
// and it ranks above coverage because coverage cannot separate them at all. Vendors that
// name bodies and clips differently match nothing here and rank as they did.
//
// Auto-match is scoped to the clip's own vendor: cross-vendor skeletons share enough
// bone names to pass the coverage bar but differ in rest pose, posing a clip into
// shredded garbage still. A legacy entry with no recorded vendor is a wildcard until it
// is re-registered, so old caches keep working.
export function matchRig(entries, bones, vendor, clipName) {
  if (!bones || !bones.length) return null;
  const series = clipName ? nameSeries(clipName) : null;
  let best = null, bestScore = -1, bestPinned = false, bestNamed = false;
  for (const e of entries || []) {
    if (vendor && e.vendor && e.vendor !== vendor) continue;
    if (e.bones.length > new Set(e.bones).size * 1.4) continue; // legacy cache: skip multi-skeleton showcase meshes, whose bones repeat per character (register now rejects them)
    const hit = coversBones(e.bones, bones);
    if (!hit) continue; // rig must fit the clip
    const pinned = !!e.pinned;
    const named = !!series && nameSeries(e.name) === series;
    // Rank by absolute shared bones: prefer the fullest matching body.
    const better = pinned !== bestPinned ? pinned
      : named !== bestNamed ? named
        : hit > bestScore;
    if (better) { best = e; bestScore = hit; bestPinned = pinned; bestNamed = named; }
  }
  return best;
}

// hasNamedBody reports whether the registry already holds the body a clip is named after,
// so a caller can tell the case matchRig can settle on its own from the one where the
// entry it would rank first is missing and has to be looked up.
//
// A registry that predates the naming rule holds whichever body was registered first, and
// that body fits — one skeleton, several meshes — so nothing would ever go looking for the
// named one again. This is the cheap question asked before paying for that search.
export function hasNamedBody(entries, vendor, clipName) {
  const series = nameSeries(clipName);
  if (!series) return true; // nothing to look for, so nothing is missing
  return (entries || []).some((e) => (!vendor || !e.vendor || e.vendor === vendor) && nameSeries(e.name) === series);
}

// searchedSkeleton reports whether one of the bone sets already searched for is the same
// skeleton as this clip's, so searching again would load the same models to the same end.
// A pack can ship two skeletons that share no bone name — a Unity-named body and clips
// beside an Unreal-named body and clips — and a search that came up empty for one says
// nothing about the other, so each has to be searched on its own.
//
// Sameness is the coverage test read the other way round: a rig that could play this clip
// would have had to cover an already-searched set too, and the search that set made found
// none. Erring toward searching again costs a repeated pass; erring the other way leaves
// a whole skeleton's clips with no body for the session.
export function searchedSkeleton(tried, bones) {
  return (tried || []).some((prev) => coversBones(prev, bones) > 0);
}

// nameSeries is a file name's leading word — "A" in A_POLY_BOW_Cmp_Idle, "Paladin" in
// Paladin WProp J Nordstrom. Files a vendor generates as one series share it.
export const nameSeries = (name) => (name || '').split(/[_@\-. ]/)[0].toLowerCase();

// LOADABLE_RIG_EXTS are the container formats the preview can actually open. A pack's
// heaviest file is often a .blend, which is not one of them.
export const LOADABLE_RIG_EXTS = ['fbx', 'glb', 'gltf'];

// packRigCandidates narrows a pack's heaviest-first listing to what is worth loading as
// a rig. Weight alone finds a character body, which outweighs every clip beside it —
// but not a prop rig: a bow its own clips animate is lighter than the character
// animations it ships with, and hundreds of those outrank it. Dropping the series the
// clip itself belongs to leaves the things a pack animates rather than the animations,
// and the heaviest of those is the rig.
//
// A model carrying that same series is the exception, and goes first rather than out. The
// series names what a file is *of*, and a pack that ships a body per variant names each
// one for the clips that drive it — HumanM_Model beside HumanM@CombatIdle1H01 — so on a
// model the shared series is the strongest evidence in the listing, while on an animation
// it is what marks the file as one of the clips. Weight cannot stand in for it: the pack
// ships HumanF_Model heavier than HumanM_Model, and every male clip would borrow the
// female body.
export function packRigCandidates(items, clipName, limit = 3) {
  const series = nameSeries(clipName);
  const loadable = (items || []).filter((it) => LOADABLE_RIG_EXTS.includes(it.ext));
  const sameSeries = (it) => nameSeries(it.name) === series;
  const named = loadable.filter((it) => it.category === 'model' && sameSeries(it));
  return named.concat(loadable.filter((it) => !sameSeries(it))).slice(0, limit);
}

// BIND_FIT_MARGIN is how much closer the stored bind has to sit before it overrules the
// nodes. The two answers are an order of magnitude apart across the library — a tenth of
// the distance where the stored bind is the right one, level where the measure cannot
// tell — so anything between them decides the same way.
const BIND_FIT_MARGIN = 0.5;

// storedBindFits reports whether a skin's stored bind pose sits closer to the mesh than
// the pose its skeleton nodes are in. samples carries, per bone, how far that bone's own
// vertices lie from each candidate: { stored, rest }, each an array of distances.
//
// A vertex the skin gives to one bone is carried rigidly by it, so under the right
// candidate it sits in that bone's own neighbourhood, and under the wrong one it sits
// wherever that bone happens to be — trailing behind at that distance in every frame,
// which is the long fingers on a hand whose bones the two candidates place 20cm apart.
// The median across bones is what decides, so a densely modelled head cannot outvote a
// hand.
//
// Positions are all it reads, so two candidates that place a bone identically and
// disagree only about its roll come back level. That is a tie, and a tie leaves the nodes
// in charge: this only ever overrules them on evidence it can actually see.
export function storedBindFits(samples) {
  const stored = [], rest = [];
  for (const s of samples || []) {
    if (!s || !s.stored || !s.stored.length || !s.rest || !s.rest.length) continue;
    stored.push(median(s.stored));
    rest.push(median(s.rest));
  }
  if (!stored.length) return false;
  return median(stored) < median(rest) * BIND_FIT_MARGIN;
}

function median(values) {
  const s = [...values].sort((a, b) => a - b);
  const h = s.length >> 1;
  return s.length % 2 ? s[h] : (s[h - 1] + s[h]) / 2;
}

// stackedCharacter picks which skeleton is the character in a file that stacks several
// copies of one rig. A vendor ships a pack's whole cast that way, each character skinned
// to its own copy of the same bone names; only one copy is built, its bones carrying the
// rest offsets that give a body its proportions, while the others collapse to the
// armature origin. Returns that copy's index, or -1 when the file is not that shape and
// has to be left as it is: a lone skeleton, copies that are all built (a character whose
// props ride on rig copies of their own), or skeletons that are not the same rig at all.
//
// A stack has to be collapsed before anything poses on it, because a repeated bone name
// resolves to one node and one node only — three binds each animation track to the first
// bone of that name — so every copy past that one holds a rest pose nothing drives while
// its mesh is skinned as though it moved.
export function stackedCharacter(skeletons) {
  const skels = skeletons || [];
  if (skels.length < 2) return -1;
  const sameRig = (s) => [...new Set(s.names)].sort().join('|');
  const rig = sameRig(skels[0]);
  let built = -1;
  for (let i = 0; i < skels.length; i++) {
    if (sameRig(skels[i]) !== rig) return -1;
    if (!skels[i].placed) continue;
    if (built >= 0) return -1; // two built copies: nothing marks out which is the character
    built = i;
  }
  return built;
}
