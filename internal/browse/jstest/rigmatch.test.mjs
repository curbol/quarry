// Matching a mesh-less animation clip to a body that can play it. These live outside
// assets/ because that directory is embedded into the binary and served: a test file
// there would ship to the browser.
//
// Getting one of these wrong does not break the page. It previews a different animation
// than the card names, or poses a clip onto a rig it does not fit and renders a shredded
// body — over a library of 150k thumbnails, neither is something anyone eyeballs.
import test from 'node:test';
import assert from 'node:assert/strict';

import {
  clipsForAsset, clipsMatching, coversBones, matchRig, nameSeries, packRigCandidates,
} from '../assets/rigmatch.js';

const clips = (...names) => names.map((name) => ({ name }));
const forClip = (clip, clipIndex) => ({ source: clipIndex === undefined ? { clip } : { clip, clipIndex } });

// A GLB's animation names are optional and need not be unique, so the split labels a
// clip "Walk (2)" — a name no animation in the file carries. Only the index can find it.
test('the index picks the clip, not the label', () => {
  const all = clips('Walk', 'Walk', 'Run');
  assert.deepEqual(clipsForAsset(all, forClip('Walk (2)', 1)), [all[1]]);
  assert.deepEqual(clipsForAsset(all, forClip('Walk', 0)), [all[0]]);
});

// An index the file no longer has is a stale card against a re-exported model. Falling
// through to the name is the recovery; falling through to "all of them" would preview
// an arbitrary animation as if it were the named one.
test('a stale index falls back to the name', () => {
  const all = clips('Walk', 'Run');
  assert.deepEqual(clipsForAsset(all, forClip('Run', 7)), [all[1]]);
  assert.deepEqual(clipsForAsset(all, forClip('Missing', 7)), all);
});

test('a whole-file asset previews every clip', () => {
  const all = clips('Walk', 'Run');
  assert.deepEqual(clipsForAsset(all, { source: {} }), all);
  assert.deepEqual(clipsForAsset(all, null), all);
  assert.deepEqual(clipsForAsset(null, forClip('Walk')), []);
});

// FBX prefixes a clip name with its take.
test('a take-prefixed FBX name matches on its suffix', () => {
  const all = clips('Armature|Walk', 'Armature|Run');
  assert.deepEqual(clipsForAsset(all, forClip('Walk')), [all[0]]);
  // ...but only on a "|" boundary, or "Walk" would claim "SideWalk".
  assert.deepEqual(clipsForAsset(clips('SideWalk'), forClip('Walk')), clips('SideWalk'));
});

// The root-motion sibling is a different file with its own indices, so only a name can
// carry across. Where the name misses, the sibling is usable only when it holds exactly
// one clip and there is nothing to pick wrong — otherwise the toggle would present an
// arbitrary animation as this clip's travel variant.
test('the root-motion sibling matches by name, or not at all', () => {
  const many = clips('Walk', 'Run');
  assert.deepEqual(clipsMatching(many, forClip('Run')), [many[1]]);
  assert.deepEqual(clipsMatching(many, forClip('Sprint')), []);
  const one = clips('SomethingElse');
  assert.deepEqual(clipsMatching(one, forClip('Sprint')), one);
  assert.deepEqual(clipsMatching(many, { source: {} }), many);
  // clipsForAsset's "all of them" fallback is exactly what this must not do.
  assert.notDeepEqual(clipsMatching(many, forClip('Sprint')), many);
});

const bones = (n, prefix = 'b') => Array.from({ length: n }, (_, i) => prefix + i);

// Both thresholds, and both directions. A rig that shares a handful of names with a
// full clip poses it into garbage; a showcase superset rig passes the clip's side and
// fails its own.
test('coverage needs the clip to drive the rig and the rig to cover the clip', () => {
  const clip = bones(20);
  assert.equal(coversBones(clip, clip), 20);

  // A partial rig: every one of its bones is driven, but it covers a quarter of the clip.
  assert.equal(coversBones(bones(5), clip), 0);
  // A superset rig: it covers the whole clip, but the clip drives a fifth of it.
  assert.equal(coversBones([...clip, ...bones(80, 'x')], clip), 0);
  // Just inside both: 12 of 20 shared is 60% of the rig and 60% of the clip.
  assert.equal(coversBones([...bones(12), ...bones(8, 'y')], clip), 12);
  // Just outside the rig side: 11 of 20 is 55%.
  assert.equal(coversBones([...bones(11), ...bones(9, 'y')], clip), 0);

  assert.equal(coversBones([], clip), 0);
  assert.equal(coversBones(clip, []), 0);
  assert.equal(coversBones(null, undefined), 0);
});

// A cached entry can repeat a name per bone instance. Coverage is about which bones are
// driven, not how many times, so both sides count distinct names.
test('repeated bone names count once', () => {
  const clip = bones(10);
  assert.equal(coversBones([...clip, ...clip], clip), 10);
});

const entry = (o) => ({ id: o.id, vendor: o.vendor, bones: o.bones, pinned: o.pinned });

test('a pinned rig beats a better-covering unpinned one', () => {
  const clip = bones(20);
  const full = entry({ id: 'full', vendor: 'synty', bones: clip });
  const pinned = entry({ id: 'pinned', vendor: 'synty', bones: [...bones(14), ...bones(6, 'y')], pinned: true });
  assert.equal(matchRig([full, pinned], clip, 'synty').id, 'pinned');
  // Unpinned, the fullest match wins.
  assert.equal(matchRig([full, { ...pinned, pinned: false }], clip, 'synty').id, 'full');
});

// Cross-vendor skeletons share enough bone names to pass the coverage bar but differ in
// rest pose, so a clip posed on one still comes out shredded.
test('auto-match is scoped to the clip vendor, and a vendorless entry is a wildcard', () => {
  const clip = bones(20);
  const other = entry({ id: 'other', vendor: 'kevdev', bones: clip });
  const legacy = entry({ id: 'legacy', bones: clip });
  assert.equal(matchRig([other], clip, 'synty'), null);
  assert.equal(matchRig([other], clip, 'kevdev').id, 'other');
  assert.equal(matchRig([legacy], clip, 'synty').id, 'legacy');
  // No vendor on the clip either: everything is in scope.
  assert.equal(matchRig([other], clip, '').id, 'other');
});

// A showcase mesh holds several characters' skeletons, so its bone names repeat per
// character. It covers any one clip trivially and poses it onto the wrong body.
test('a legacy multi-skeleton showcase entry is skipped', () => {
  const clip = bones(20);
  const showcase = entry({ id: 'showcase', vendor: 'synty', bones: [...clip, ...clip, ...clip] });
  assert.equal(matchRig([showcase], clip, 'synty'), null);
  // A little repetition is not a showcase: the cutoff is 1.4x.
  const slight = entry({ id: 'slight', vendor: 'synty', bones: [...clip, ...bones(5)] });
  assert.equal(matchRig([slight], clip, 'synty').id, 'slight');
});

test('no bones, no entries, no match', () => {
  assert.equal(matchRig([entry({ id: 'a', bones: bones(20) })], [], 'synty'), null);
  assert.equal(matchRig([], bones(20), 'synty'), null);
  assert.equal(matchRig(null, bones(20), 'synty'), null);
});

test('nameSeries is the leading word, on any of the separators a vendor uses', () => {
  assert.equal(nameSeries('A_POLY_BOW_Cmp_Idle'), 'a');
  assert.equal(nameSeries('Paladin WProp J Nordstrom'), 'paladin');
  assert.equal(nameSeries('HumanF@TurnLeft'), 'humanf');
  assert.equal(nameSeries('Sword-Swing.fbx'), 'sword');
  assert.equal(nameSeries(''), '');
  assert.equal(nameSeries(undefined), '');
});

// The clip's own series is what the pack animates *with*; dropping it leaves the things
// the pack animates, and the heaviest of those is the rig. A .blend outweighs all of
// them and cannot be opened at all.
test('pack candidates drop the clip series and anything unloadable', () => {
  const page = [
    { name: 'Scene.blend', ext: 'blend' },
    { name: 'A_Walk.fbx', ext: 'fbx' },
    { name: 'POLY_Bow.fbx', ext: 'fbx' },
    { name: 'POLY_Quiver.glb', ext: 'glb' },
    { name: 'POLY_Arrow.gltf', ext: 'gltf' },
    { name: 'POLY_Target.fbx', ext: 'fbx' },
  ];
  assert.deepEqual(
    packRigCandidates(page, 'A_POLY_BOW_Cmp_Idle').map((it) => it.name),
    ['POLY_Bow.fbx', 'POLY_Quiver.glb', 'POLY_Arrow.gltf'],
  );
  // Heaviest-first order is the caller's; this only narrows and caps.
  assert.equal(packRigCandidates(page, 'A_Walk', 10).length, 4);
  assert.deepEqual(packRigCandidates(null, 'A_Walk'), []);
});
