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
  clipsForAsset, clipsMatching, coversBones, matchRig, nameSeries, packRigCandidates, searchedSkeleton,
  stackedCharacter, storedBindFits, hasNamedBody,
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
  // ...but only on a "|" boundary, or "Walk" would claim "SideWalk". Two clips, so a
  // wrong suffix match and the "no match, take them all" fallback do not look alike:
  // with one clip both produce the same array and the assertion proves nothing.
  const neither = clips('SideWalk', 'Run');
  assert.deepEqual(clipsForAsset(neither, forClip('Walk')), neither);
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
  // The same "|" boundary clipsForAsset needs, where getting it wrong costs more than a
  // widened selection: the toggle would play SideWalk and present it as Walk's travel.
  assert.deepEqual(clipsMatching(clips('SideWalk', 'Sprint'), forClip('Walk')), []);
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

  // The clip side, pinned as tightly as the rig side above. Without a pair straddling
  // it the threshold could move from 0.3 to 0.6 with every test here still passing,
  // and the only symptom would be a clip posed onto a rig missing half its bones.
  //
  // 9 shared of a 15-bone rig is 60% of the rig and 45% of the 20-bone clip: in.
  assert.equal(coversBones([...bones(9), ...bones(6, 'y')], clip), 9);
  // 8 shared of a 13-bone rig is 61.5% of the rig and 40% of the clip: out.
  assert.equal(coversBones([...bones(8), ...bones(5, 'y')], clip), 0);

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

const entry = (o) => ({ id: o.id, name: o.name, vendor: o.vendor, bones: o.bones, pinned: o.pinned });

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

// A pack that ships two skeletons is the case this exists for: DoubleL ships Unity-named
// clips beside a Unity-named body and Unreal-named clips beside an Unreal-named body, and
// the two share no bone name. One search per pack registers whichever body covers the clip
// that reached it first and stops, leaving the other skeleton's clips with nothing to pose
// on for the session.
test('a skeleton sharing no bone with one already searched for is searched on its own', () => {
  const unity = bones(70);
  const unreal = bones(90, 'u');
  assert.equal(searchedSkeleton([unity], unity), true);
  assert.equal(searchedSkeleton([unity], unreal), false);
  assert.equal(searchedSkeleton([unity, unreal], unreal), true);
});

// Nothing searched for yet: the first clip of a pack has to reach the search.
test('an empty history has searched for nothing', () => {
  assert.equal(searchedSkeleton([], bones(20)), false);
  assert.equal(searchedSkeleton(undefined, bones(20)), false);
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

// kevdev names a body for the clips that drive it, so the clip's own series is what
// picks it out — while on the animations beside it that same series is what marks them
// as clips. Weight alone reaches for the wrong one: HumanF_Model outweighs HumanM_Model,
// and the pack's male clips would every one of them borrow the female body.
test('a model named for the clip series leads, the clips sharing it still go', () => {
  const page = [
    { name: 'HumanF_MeleeAnimations.blend', ext: 'blend', category: 'animation' },
    { name: 'HumanF_Model.fbx', ext: 'fbx', category: 'model' },
    { name: 'HumanM@IdleWounded01.fbx', ext: 'fbx', category: 'animation' },
    { name: 'HumanF@Idle01.fbx', ext: 'fbx', category: 'animation' },
    { name: 'HumanM_Model.fbx', ext: 'fbx', category: 'model' },
  ];
  assert.deepEqual(
    packRigCandidates(page, 'HumanM@CombatIdle1H01.fbx').map((it) => it.name),
    ['HumanM_Model.fbx', 'HumanF_Model.fbx', 'HumanF@Idle01.fbx'],
  );
  assert.deepEqual(
    packRigCandidates(page, 'HumanF@CombatIdle1H01.fbx').map((it) => it.name),
    ['HumanF_Model.fbx', 'HumanM@IdleWounded01.fbx', 'HumanM_Model.fbx'],
  );
});

const skel = (names, placed = 0) => ({ names, placed });

// Synty ships a pack's characters stacked in one file, every one of them on its own
// copy of the same bone names. Only the built copy carries rest offsets.
test('the built copy is the character in a stack of one rig', () => {
  const rig = bones(50);
  assert.equal(stackedCharacter([skel(rig), skel(rig, 49)]), 1);
  // A pack's whole cast: 25 copies, one of them built.
  const cast = Array.from({ length: 25 }, (_, i) => skel(rig, i === 22 ? 49 : 0));
  assert.equal(stackedCharacter(cast), 22);
});

// The stack is only safe to collapse when every copy really is the same rig, and when
// exactly one of them is built. Anything else is a file this knows nothing about.
test('only a stack of one rig with a single built copy collapses', () => {
  const rig = bones(50);
  // One skeleton: a plain character, nothing to collapse.
  assert.equal(stackedCharacter([skel(rig, 49)]), -1);
  // Every copy built: a character carrying props on rig copies, which poses correctly
  // as it is — the outer copy drags the props with it.
  assert.equal(stackedCharacter([skel(rig, 49), skel(rig, 49)]), -1);
  // No copy built at all: not the shape this reads.
  assert.equal(stackedCharacter([skel(rig), skel(rig)]), -1);
  // Two built among many: nothing marks out which one is the character.
  assert.equal(stackedCharacter([skel(rig), skel(rig, 49), skel(rig, 49)]), -1);
  // Different rigs sharing a file: a showcase of genuinely different skeletons, where
  // no one of them stands in for the rest.
  assert.equal(stackedCharacter([skel(rig), skel(bones(30, 'z'), 29)]), -1);
  assert.equal(stackedCharacter([]), -1);
  assert.equal(stackedCharacter(null), -1);
});

// A single character's own rig can repeat a name (the Synty body names both hands'
// finger bones alike), so copies are compared on the names they carry, not the count.
test('a rig that repeats a name within itself still stacks', () => {
  const rig = [...bones(50), 'Thumb_01', 'Thumb_01'];
  assert.equal(stackedCharacter([skel(rig), skel(rig, 49)]), 1);
});

// Both candidate binds place a bone somewhere, and the mesh says which. A bone whose own
// vertices sit centimetres away under the stored bind and 20cm away under the nodes is
// the doublel Unreal mannequin, where rebinding to the nodes is what stretched every
// finger into a streak.
const fit = (stored, rest) => ({ stored, rest });

test('the bind the mesh sits closest to wins', () => {
  assert.equal(storedBindFits([fit([2, 2.5], [22, 23]), fit([3], [20])]), true);
  assert.equal(storedBindFits([fit([22, 23], [2, 2.5]), fit([20], [3])]), false);
});

// A rig whose two candidates place every bone identically and disagree only about a
// bone's roll measures level, and level is not evidence. The nodes keep the rig, which is
// what the borrowed-rig case needs and what the explosive RPG character relies on.
test('a tie leaves the nodes in charge', () => {
  assert.equal(storedBindFits([fit([5.28], [5.29]), fit([6], [6])]), false);
  // Closer, but not by the margin: still not enough to overrule the nodes.
  assert.equal(storedBindFits([fit([8], [10])]), false);
});

// Only bones the skin gives whole vertices to can be measured. A rig that offers none
// leaves nothing to judge on, and judging on nothing must not overrule the nodes.
test('nothing to measure is not evidence for the stored bind', () => {
  assert.equal(storedBindFits([]), false);
  assert.equal(storedBindFits(null), false);
  assert.equal(storedBindFits([fit([], []), null, fit([2], [])]), false);
});

// The median is taken across bones, not across vertices, so a densely modelled head
// cannot outvote the hands. Here one bone carries 8 samples that favour the nodes and
// three carry one each that favour the stored bind.
test('a dense bone does not outvote the sparse ones', () => {
  const dense = fit(Array(8).fill(30), Array(8).fill(3));
  assert.equal(storedBindFits([dense, fit([1], [30]), fit([1], [30]), fit([1], [30])]), true);
});

// A pack's body variants are one skeleton wearing different meshes, so every one of them
// covers every clip equally and coverage has nothing left to say. The name does: the male
// clip belongs on the male body however the registry happens to be ordered.
test('a body named for the clip series wins a tie coverage cannot break', () => {
  const rig = bones(52);
  const f = entry({ id: 'f', name: 'HumanF_Model.fbx', vendor: 'kevdev', bones: rig });
  const m = entry({ id: 'm', name: 'HumanM_Model.fbx', vendor: 'kevdev', bones: rig });
  assert.equal(matchRig([f, m], rig, 'kevdev', 'HumanM@CombatIdle1H01.fbx').id, 'm');
  assert.equal(matchRig([m, f], rig, 'kevdev', 'HumanM@CombatIdle1H01.fbx').id, 'm');
  assert.equal(matchRig([m, f], rig, 'kevdev', 'HumanF@Idle01.fbx').id, 'f');
  // No clip name, or a vendor that names bodies and clips differently: ranked as before.
  assert.equal(matchRig([f, m], rig, 'kevdev').id, 'f');
  assert.equal(matchRig([f, m], rig, 'kevdev', 'A_POLY_BOW_Cmp_Idle.fbx').id, 'f');
});

// The name is a tiebreak, not an override: a pinned body is still the one the user chose,
// and a body that cannot play the clip is still no candidate.
test('the clip name yields to pinning and to coverage', () => {
  const rig = bones(52);
  const named = entry({ id: 'named', name: 'HumanM_Model.fbx', vendor: 'kevdev', bones: rig });
  const pinned = entry({ id: 'pinned', name: 'HumanF_Model.fbx', vendor: 'kevdev', bones: rig, pinned: true });
  assert.equal(matchRig([named, pinned], rig, 'kevdev', 'HumanM@Idle.fbx').id, 'pinned');
  // Named for the clip but a different skeleton: coverage rejects it before the name counts.
  const stranger = entry({ id: 'stranger', name: 'HumanM_Model.fbx', vendor: 'kevdev', bones: bones(52, 'z') });
  const other = entry({ id: 'other', name: 'HumanF_Model.fbx', vendor: 'kevdev', bones: rig });
  assert.equal(matchRig([stranger, other], rig, 'kevdev', 'HumanM@Idle.fbx').id, 'other');
});

// A registry written before the naming rule holds whichever body was registered first,
// and that body fits, so nothing would ever look for the named one again. hasNamedBody is
// the question asked before paying for that lookup.
test('the named body is missing until the registry holds one', () => {
  const f = entry({ id: 'f', name: 'HumanF_Model.fbx', vendor: 'kevdev', bones: bones(52) });
  const m = entry({ id: 'm', name: 'HumanM_Model.fbx', vendor: 'kevdev', bones: bones(52) });
  assert.equal(hasNamedBody([f], 'kevdev', 'HumanM@CombatIdle1H01.fbx'), false);
  assert.equal(hasNamedBody([f, m], 'kevdev', 'HumanM@CombatIdle1H01.fbx'), true);
  assert.equal(hasNamedBody([f], 'kevdev', 'HumanF@Idle01.fbx'), true);
  assert.equal(hasNamedBody([], 'kevdev', 'HumanM@Idle.fbx'), false);
});

// Scoped to the clip's vendor the same way matching is, since a body from another vendor
// is never what match() would return however it is named. A vendorless legacy entry stays
// the wildcard it is everywhere else.
test('a named body from another vendor does not count as holding it', () => {
  const other = entry({ id: 'o', name: 'HumanM_Model.fbx', vendor: 'synty', bones: bones(52) });
  const legacy = entry({ id: 'l', name: 'HumanM_Model.fbx', bones: bones(52) });
  assert.equal(hasNamedBody([other], 'kevdev', 'HumanM@Idle.fbx'), false);
  assert.equal(hasNamedBody([legacy], 'kevdev', 'HumanM@Idle.fbx'), true);
});

// No series to look for means nothing is missing, so no search is paid for.
test('a clip with no name asks for nothing', () => {
  assert.equal(hasNamedBody([], 'kevdev', ''), true);
  assert.equal(hasNamedBody([], 'kevdev', null), true);
  assert.equal(hasNamedBody(null, 'kevdev', 'HumanM@Idle.fbx'), false);
});
