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
// Auto-match is scoped to the clip's own vendor: cross-vendor skeletons share enough
// bone names to pass the coverage bar but differ in rest pose, posing a clip into
// shredded garbage still. A legacy entry with no recorded vendor is a wildcard until it
// is re-registered, so old caches keep working.
export function matchRig(entries, bones, vendor) {
  if (!bones || !bones.length) return null;
  let best = null, bestScore = -1, bestPinned = false;
  for (const e of entries || []) {
    if (vendor && e.vendor && e.vendor !== vendor) continue;
    if (e.bones.length > new Set(e.bones).size * 1.4) continue; // legacy cache: skip multi-skeleton showcase meshes, whose bones repeat per character (register now rejects them)
    const hit = coversBones(e.bones, bones);
    if (!hit) continue; // rig must fit the clip
    const pinned = !!e.pinned;
    // Rank by absolute shared bones: prefer the fullest matching body.
    if ((pinned && !bestPinned) || (pinned === bestPinned && hit > bestScore)) {
      best = e; bestScore = hit; bestPinned = pinned;
    }
  }
  return best;
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
export function packRigCandidates(items, clipName, limit = 3) {
  const series = nameSeries(clipName);
  return (items || [])
    .filter((it) => nameSeries(it.name) !== series && LOADABLE_RIG_EXTS.includes(it.ext))
    .slice(0, limit);
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
