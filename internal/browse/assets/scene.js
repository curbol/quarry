// Shared 3D pipeline — model loading, orientation, posing, retargeting, and rig
// matching — imported by both the main thread (app.js) and the OffscreenCanvas
// thumbnail worker. three is imported by absolute path so it resolves in a worker
// (which has no import map); it is the same file the document's import map points
// "three" at, so a single three instance is shared across both.
import * as THREE from '/static/vendor/three/three.module.min.js';
import { clipsForAsset, clipsMatching, coversBones, hasNamedBody, matchRig, nameSeries, packRigCandidates, searchedSkeleton, stackedCharacter, storedBindFits } from '/static/rigmatch.js';
import { trimmedDuration } from '/static/cliptrim.js';
import { GLTFLoader } from '/static/vendor/three/jsm/loaders/GLTFLoader.js';
import { FBXLoader } from '/static/vendor/three/jsm/loaders/FBXLoader.js';

export const contentURL = (id) => '/api/content?id=' + encodeURIComponent(id);
export const thumbURL = (id) => '/api/thumb?id=' + encodeURIComponent(id);

// CharRegistry persists to localStorage on the main thread; a worker has none, so it
// falls back to an in-memory store (its rig cache then lasts the worker's lifetime).
const memStore = new Map();
const store = {
  get(k) { try { return typeof localStorage !== 'undefined' ? localStorage.getItem(k) : (memStore.has(k) ? memStore.get(k) : null); } catch { return memStore.has(k) ? memStore.get(k) : null; } },
  set(k, v) { try { if (typeof localStorage !== 'undefined') localStorage.setItem(k, v); else memStore.set(k, v); } catch { memStore.set(k, v); } },
};

const BLANK_PIXEL = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==';
const loadingManager = new THREE.LoadingManager();
loadingManager.setURLModifier((url) => {
  if (url.includes('/api/content')) return url;
  // The FBX loader mints an object URL per embedded texture and, unlike the glTF one,
  // never revokes it. Swapping in a blank pixel here means nothing ever loads that URL
  // either, so the bytes behind it would stay resident for the life of the worker —
  // which is the life of the page, across every card a scroll ever touched.
  if (url.startsWith('blob:')) URL.revokeObjectURL(url);
  return BLANK_PIXEL;
});
// Whether the blanking manager governs glTF too. In the worker it must: a scroll
// touches thousands of models and every texture behind one would stay resident for the
// life of the page. In the lightbox it must not — the user is looking at one model and
// its maps are what it looks like, and this module is shared, so left module-wide the
// comment above describes the worker while the code also silently strips every glTF
// texture in the preview. FBX is unaffected either way: loadModel replaces its
// materials with CLAY, so its textures are never drawn wherever it is loaded.
const inWorker = typeof WorkerGlobalScope !== 'undefined';
const gltfManager = inWorker ? loadingManager : new THREE.LoadingManager();

const CLAY = new THREE.MeshStandardMaterial({ color: 0xc7ccd6, roughness: 0.72, metalness: 0.0 });

// normalizeClip rebases a clip to its first real keyframe. Synty source FBX keep each
// clip at its position on a shared master timeline, so a clip can begin with seconds
// of empty (static) lead-in that no real import would play; trimming it makes playback
// and posed thumbnails start on the actual motion. A no-op for clips already at 0.
function normalizeClip(clip) {
  let tMin = Infinity;
  for (const tr of clip.tracks) if (tr.times.length) tMin = Math.min(tMin, tr.times[0]);
  if (isFinite(tMin) && tMin > 1e-3) {
    // Clone each track's times before shifting. GLTFLoader shares one times buffer across
    // a clip's tracks (and across clips), so subtracting in place double-counts, drives the
    // shared array negative, and collapses durations to 0 (the NaN/0.00s scrubber on GLBs).
    for (const tr of clip.tracks) {
      const t = tr.times.slice();
      for (let i = 0; i < t.length; i++) t[i] -= tMin;
      tr.times = t;
    }
    clip.resetDuration();
  }
  return trimStaticTail(clip);
}

// trimStaticTail shortens a clip that ends by holding a pose, so playback and looping
// stop on the real end. Where that is lives in cliptrim.js, which takes plain keyframe
// arrays and is covered by the Node tests; this is the mutation, and the record of the
// original on userData.trimmedFrom that the UI reads to say the clip was cut.
function trimStaticTail(clip) {
  const cut = trimmedDuration(
    clip.tracks.map((tr) => ({ times: tr.times, values: tr.values, valueSize: tr.getValueSize() })),
    clip.duration,
  );
  if (cut > 0) {
    clip.userData = clip.userData || {};
    clip.userData.trimmedFrom = clip.duration;
    clip.duration = cut;
  }
  return clip;
}

// loadModel returns the display root with its animation clips attached as
// root.animations (GLTFLoader keeps clips on gltf.animations, not the scene).
async function loadModel(url, ext) {
  if (ext === 'glb' || ext === 'gltf') {
    const gltf = await new GLTFLoader(gltfManager).loadAsync(url);
    const root = gltf.scene;
    root.animations = (gltf.animations || []).map(normalizeClip);
    return root;
  }
  const obj = await new FBXLoader(loadingManager).loadAsync(url);
  obj.traverse((o) => { if (o.isMesh) o.material = CLAY; });
  if (obj.animations) obj.animations = obj.animations.map(normalizeClip);
  return obj;
}

// loadSidekick assembles a Synty Sidekick character from its part meshes (ids from
// Source.Parts). Each part is a separate FBX skinned to the same skeleton at the same
// bind pose and root transform, so overlaying them at the origin reconstructs the whole
// figure. Parts that fail to load are skipped; null when none load. The parts keep their
// own (identical) skeletons — enough for a static clay preview.
//
// The parts are fetched together rather than in turn: each is its own FBX and none of
// them informs how the next is loaded, so awaiting them one at a time made a character
// cost the sum of a dozen round trips — once per grid thumbnail and again in the
// lightbox. They are added in declaration order so the scene graph does not depend on
// which request happened to land first.
async function loadSidekick(parts) {
  const loaded = await Promise.all((parts || []).map(
    (pid) => loadModel(contentURL(pid), 'fbx').catch(() => null), // skip a bad part
  ));
  const group = new THREE.Group();
  for (const part of loaded) {
    if (part) group.add(part);
  }
  return group.children.length ? group : null;
}

function boneNames(root) {
  const names = [];
  root.traverse((o) => { if (o.isBone) names.push(o.name); });
  return names;
}

// clipBones returns the distinct bone names a clip's tracks drive.
function clipBones(clip) {
  return [...new Set(clip.tracks.map((t) => t.name.split('.')[0]))];
}

// loadRMClips loads an animation's root-motion (travel) sibling file and returns its
// matching clips — the same clip name as the card. The RM and in-place variants share
// a skeleton, so the clips play on the already-loaded body; only the AnimationClips are
// kept (they outlive disposing the source object). Null when the asset has no sibling.
async function loadRMClips(asset) {
  if (!asset || !asset.rootMotionId) return null;
  try {
    const rmObj = await loadModel(contentURL(asset.rootMotionId), asset.ext);
    const cs = clipsMatching(rmObj.animations, asset);
    dispose(rmObj);
    return cs.length ? cs : null;
  } catch { return null; }
}

// posedBox returns the object's bounds from its posed skeleton (bone world positions), not
// Box3.setFromObject — which for a skinned mesh uses the bind pose at the origin and so
// ignores the animation, leaving displaced/animated poses off-centre. Falls back to
// geometry bounds for a static model with no skeleton. Padded to cover skin past the joints.
const _posedV = new THREE.Vector3();
function posedBox(object) {
  object.updateMatrixWorld(true);
  const box = new THREE.Box3();
  let bones = 0;
  object.traverse((n) => { if (n.isBone) { box.expandByPoint(n.getWorldPosition(_posedV)); bones++; } });
  const meshBox = new THREE.Box3().setFromObject(object);
  if (bones < 2 || box.isEmpty()) return meshBox;
  const boneSpan = box.getSize(_posedV).length();
  const meshSpan = meshBox.isEmpty() ? 0 : meshBox.getSize(new THREE.Vector3()).length();
  // Some rigs keep every bone at the armature origin at bind (they skin the mesh through
  // bind matrices only), so the bone box collapses to a point; frame the mesh instead
  // when the bones don't span it.
  if (meshSpan > 0 && boneSpan < meshSpan * 0.25) return meshBox;
  box.expandByScalar(boneSpan * 0.06 || 0);
  return box;
}

function frameBox(box, camera, controls, offset = 1.5) {
  if (!box || box.isEmpty()) return;
  const size = box.getSize(new THREE.Vector3());
  const center = box.getCenter(new THREE.Vector3());
  const maxDim = Math.max(size.x, size.y, size.z) || 1;
  const fov = camera.fov * Math.PI / 180;
  const dist = (maxDim / 2) / Math.tan(fov / 2) * offset;
  camera.near = dist / 100;
  camera.far = dist * 100;
  camera.position.set(center.x + dist * 0.7, center.y + dist * 0.5, center.z + dist);
  camera.lookAt(center);
  camera.updateProjectionMatrix();
  if (controls) { controls.target.copy(center); controls.update(); }
}

// isRenderable reports whether an object has any geometry to show. Synty ANIMATION
// packs ship mesh-less FBX (skeleton + keyframes only), which would otherwise render
// as an empty void.
function isRenderable(object) {
  const box = new THREE.Box3().setFromObject(object);
  if (box.isEmpty()) return false;
  // A real model has volume. Synty morph-animation FBX ship a flat 3-vertex stub
  // mesh (a degenerate plane) alongside the skeleton; treat that as a clip to pose
  // on a rig, not a stray triangle to render.
  const s = box.getSize(new THREE.Vector3());
  const dims = [s.x, s.y, s.z].sort((a, b) => a - b);
  return dims[0] > dims[2] * 1e-3;
}

// captureRootMotionRest returns the clip source's top-level bone rest quaternion — the
// bone's local rotation before any animation. It encodes the file's own axis convention,
// so a mesh-less clip posed on a body from a different file can be corrected regardless of
// what the character is doing (see uprightRig).
function captureRootRest(obj) {
  const b = rootBone(obj);
  return b ? b.quaternion.clone() : null;
}

// rootBone returns the first bone with no bone above it — the top of the skeleton, in
// traversal order. Three separate things need it (the rest quaternion above, the name
// stripRootMotion keys on, the node the lightbox measures from), and each had written
// the same traversal with the same "first one wins" predicate.
function rootBone(object) {
  let found = null;
  object.traverse((n) => { if (n.isBone && !found && (!n.parent || !n.parent.isBone)) found = n; });
  return found;
}

// rootBoneName is rootBone's name, or null when the object has no skeleton.
function rootBoneName(object) {
  const b = rootBone(object);
  return b ? b.name : null;
}

// uprightRig rotates the whole object so the character stands +Y-up, fixing the axis
// flips from files authored Z-up (kevdev bodies, some explosive clips) or from a mesh-less
// clip whose root-bone axis differs from the body it plays on (kevdev clips on HumanF_Model).
// It measures the character's up (hips->head) at a straight *reference* pose — the bind
// pose, with the root bone forced to the clip's own rest (rootRest) so a cross-file clip's
// root convention is anticipated — then snaps that up to the nearest cardinal axis and
// rotates that axis to +Y. Snapping (not exact hips->head alignment) is deliberate: an
// already-upright rig keeps its natural forward spine lean instead of being tilted back,
// while a 90/180 flip is still fully corrected. It leaves the skeleton in the reference
// pose, so the caller can read a stable framing box (posedBox) before animating. rootRest
// is null for self-contained and retargeted (synty) clips: then it reads the rig's own bind.
function uprightRig(root, rootRest) {
  let rootBone = null, hips = null, head = null, neck = null;
  root.traverse((o) => {
    if (!o.isBone) return;
    const n = o.name.toLowerCase();
    if (!rootBone && (!o.parent || !o.parent.isBone)) rootBone = o;
    if (!hips && (n.includes('hips') || n.includes('pelvis'))) hips = o;
    if (!head && n.includes('head')) head = o;
    if (!neck && n.includes('neck')) neck = o;
  });
  const top = head || neck;
  if (!rootBone || !hips || !top) return;
  root.traverse((o) => { if (o.isSkinnedMesh && o.skeleton) o.skeleton.pose(); });
  if (rootRest) rootBone.quaternion.copy(rootRest);
  root.updateMatrixWorld(true);
  const up = top.getWorldPosition(new THREE.Vector3()).sub(hips.getWorldPosition(new THREE.Vector3()));
  if (up.lengthSq() < 1e-6) return;
  up.normalize();
  const ax = Math.abs(up.x), ay = Math.abs(up.y), az = Math.abs(up.z);
  const card = ax >= ay && ax >= az ? new THREE.Vector3(Math.sign(up.x), 0, 0)
    : ay >= az ? new THREE.Vector3(0, Math.sign(up.y), 0)
      : new THREE.Vector3(0, 0, Math.sign(up.z));
  // The character's up axis in this file's track space; stripRootMotion keeps this axis and
  // zeroes the two horizontal ones (a Z-up file's forward is on Y, not the Y-up default).
  root.userData.upAxis = card.clone();
  root.quaternion.premultiply(new THREE.Quaternion().setFromUnitVectors(card, new THREE.Vector3(0, 1, 0)));
  root.updateMatrixWorld(true);
}

// prepareClipRig orients a rig for a clip and returns its constant reference box. It is the
// single shared setup for both the grid thumbnail and the lightbox, so the two can't drift
// apart — the recurring "one is right, the other is sideways" was two separate code paths.
// Both feed it a pristine skeleton (the lightbox a freshly loaded body, the thumbnail a
// fresh clone) because re-measuring orientation on a reused, already-posed skeleton is
// unreliable. rootRest is already resolved (null for synty / self-contained; the clip's own
// root rest for a cross-file body).
function prepareClipRig(rig, rootRest) {
  uprightRig(rig, rootRest);
  return posedBox(rig);
}

// oneCharacter reduces a file that stacks a pack's whole cast — each character skinned to
// its own copy of one rig, all of them at the origin under the same bone names — to the
// single built copy, so that every bone name resolves to exactly one node. Which copy
// that is, and whether the file is such a stack at all, is stackedCharacter's decision.
//
// The copies that go are unlinked rather than left hidden: they are what breaks posing.
// A track binds to the first bone of its name and a rest rotation is read from the first
// too, so with the copies still in the hierarchy half the reads land on a namesake that
// holds a collapsed rest and never moves. Their meshes are rebound to the surviving
// skeleton — every copy is the same rig at the same origin, so they wear the kept
// character's pose, and hideAlternates thins the stack of bodies afterwards.
function oneCharacter(root) {
  const skels = [];
  root.traverse((n) => { if (n.isSkinnedMesh && n.skeleton && !skels.includes(n.skeleton)) skels.push(n.skeleton); });
  const i = stackedCharacter(skels.map((s) => ({
    names: s.bones.map((b) => b.name),
    placed: s.bones.filter((b) => b.position.lengthSq() > 1e-8).length,
  })));
  if (i < 0) return root;
  const keep = skels[i];
  const kept = new Set(keep.bones);
  const drop = [];
  root.traverse((n) => { if (n.isBone && !kept.has(n)) drop.push(n); });
  const dropped = new Set(drop);
  // A copy's bones can carry the kept character's below them, so lift those out before
  // unlinking, or removing a namesake takes the character with it.
  //
  // Lifted to the nearest ancestor that is itself staying. traverse is pre-order, so a
  // dropped bone is visited before any dropped bone under it: re-parenting onto b.parent
  // without this check can hand a kept bone to a bone that is removed a moment later,
  // taking it along. keep.bones still holds it, so the bind succeeds and nothing errors
  // — it is simply no longer reached by updateMatrixWorld, and whatever it drives freezes
  // at a stale world matrix while the rest of the body animates.
  for (const b of drop) {
    let host = b.parent;
    while (host && dropped.has(host)) host = host.parent;
    if (!host) continue;
    for (const child of b.children.slice()) if (!dropped.has(child)) host.add(child);
  }
  for (const b of drop) if (b.parent) b.parent.remove(b);
  root.traverse((n) => { if (n.isSkinnedMesh) n.bind(keep, n.bindMatrix); });
  return root;
}

// A file's skin carries a bind pose and its skeleton nodes carry a rest pose, and the two
// name the same thing: the pose in which the mesh is undeformed. Nearly every file has
// them agree, and for those alignBindToRest does nothing at all.
//
// BIND_REST_TOL is how far apart they may sit and still count as the same pose. Loaders
// and exporters round; a body a whole limb out of place does not.
const BIND_REST_TOL = THREE.MathUtils.degToRad(5);

// bindMatchesRest reports whether a skinned object's stored bind pose is the pose its
// skeleton nodes are actually in, comparing rotation only: bone lengths are shared between
// the two by construction, and it is the rotations that come apart.
function bindMatchesRest(rig) {
  const pos = new THREE.Vector3(), scl = new THREE.Vector3();
  const bindQ = new THREE.Quaternion(), restQ = new THREE.Quaternion();
  const bindWorld = new THREE.Matrix4();
  let match = true;
  rig.updateMatrixWorld(true);
  rig.traverse((m) => {
    if (!match || !m.isSkinnedMesh || !m.skeleton) return;
    const { bones, boneInverses } = m.skeleton;
    for (let i = 0; i < bones.length; i++) {
      if (!bones[i] || !boneInverses[i]) continue;
      bindWorld.copy(boneInverses[i]).invert().decompose(pos, bindQ, scl);
      bones[i].matrixWorld.decompose(pos, restQ, scl);
      if (restQ.angleTo(bindQ) > BIND_REST_TOL) { match = false; return; }
    }
  });
  return match;
}

// boneFitSamples measures, for each bone the skin gives whole vertices to, how far those
// vertices lie from the two candidate bind positions — the stored one and the pose the
// nodes are in. Only vertices a single bone owns outright are measured, since a blended
// one sits between its bones under either candidate and says nothing about which is right.
// Sampled rather than walked: a body carries hundreds of thousands of vertices and the
// median this feeds moves no further for reading all of them.
function boneFitSamples(rig) {
  const samples = new Map();
  const v = new THREE.Vector3(), bindWorld = new THREE.Vector3(), restWorld = new THREE.Vector3();
  const m = new THREE.Matrix4();
  rig.updateMatrixWorld(true);
  rig.traverse((mesh) => {
    if (!mesh.isSkinnedMesh || !mesh.skeleton) return;
    const { bones, boneInverses } = mesh.skeleton;
    const pos = mesh.geometry.attributes.position;
    const skinIndex = mesh.geometry.attributes.skinIndex, skinWeight = mesh.geometry.attributes.skinWeight;
    if (!pos || !skinIndex || !skinWeight) return;
    const step = Math.max(1, Math.floor(pos.count / 4000));
    for (let i = 0; i < pos.count; i += step) {
      let bone = skinIndex.getComponent(i, 0), weight = skinWeight.getComponent(i, 0);
      for (let k = 1; k < 4; k++) {
        if (skinWeight.getComponent(i, k) > weight) { weight = skinWeight.getComponent(i, k); bone = skinIndex.getComponent(i, k); }
      }
      if (weight < 0.98 || !bones[bone] || !boneInverses[bone]) continue;
      v.fromBufferAttribute(pos, i);
      mesh.localToWorld(v);
      bindWorld.setFromMatrixPosition(m.copy(boneInverses[bone]).invert());
      restWorld.setFromMatrixPosition(bones[bone].matrixWorld);
      const key = bones[bone].name;
      if (!samples.has(key)) samples.set(key, { stored: [], rest: [] });
      const s = samples.get(key);
      s.stored.push(v.distanceTo(bindWorld));
      s.rest.push(v.distanceTo(restWorld));
    }
  });
  return [...samples.values()];
}

// alignBindToRest settles which pose a borrowed rig is undeformed in, for the rigs whose
// skin and skeleton disagree about it. A rig borrowed for a mesh-less clip is only a
// coordinate frame for someone else's motion, and that motion is authored as node
// transforms on the rig's own family, so the nodes are usually the frame the clip means —
// left alone, such a clip poses into a shredded body. But not always: a rig can carry a
// bind that fits its mesh and nodes that merely rest elsewhere, and rebinding that one to
// its nodes drags every bone's vertices along at the distance between the two. The mesh
// itself is the tiebreak, through storedBindFits.
//
// The bind matrix moves either way. It carries the mesh into the space the bone inverses
// are written in, and a loader that leaves it identity while the model root carries a
// Z-up-to-Y-up rotation has the two describing different poses — which is the shredded
// body this exists to prevent, arrived at from the other side.
//
// Only for a borrowed rig. A file that ships its own mesh and its own clips exported
// together is authoritative about its own bind even when its nodes rest elsewhere, which is
// what a "Character@Animation" file is: its nodes hold frame one, not a reference pose.
function alignBindToRest(rig) {
  if (bindMatchesRest(rig)) return rig; // which left every world matrix current
  const keepStored = storedBindFits(boneFitSamples(rig));
  rig.traverse((m) => {
    if (!m.isSkinnedMesh || !m.skeleton) return;
    if (!keepStored) m.skeleton.calculateInverses();
    m.bindMatrix.copy(m.matrixWorld);
    m.bindMatrixInverse.copy(m.matrixWorld).invert();
  });
  return rig;
}

// cloneRig deep-clones a skinned character (three's Object3D.clone shares the skeleton, so
// posing one clone would move them all). Same algorithm as three's SkeletonUtils.clone: clone
// the hierarchy, then rebind each SkinnedMesh to a cloned skeleton whose bones point at the
// cloned nodes. Lets the thumbnails reuse one loaded body but pose each clip on a fresh
// skeleton, matching the lightbox's fresh-load pipeline.
// hideAlternates hides the versions a file stacks in one place, keeping the first of
// each. A prop ships as a variant sheet — Synty's bow rig carries eight complete bows
// at one origin — and borrowing that to play a clip draws all eight at once.
//
// Two meshes are versions of each other when they sit in the same place at the same
// level of detail. Position alone would be wrong: a bowstring sits inside the bow it
// belongs to. Detail alone would be wrong too: a character's sword and helmet are
// modelled as finely as each other. Together they separate the cases with room to
// spare — measured across that bow sheet and a character carrying a sword, shield and
// helmet, versions drift at most 0.21 of the smaller mesh's own extent while real
// parts start at 0.94, and a string carries 72 vertices against a bow's 16,000.
//
// Hidden rather than removed: a file's bone hierarchy can hang off a mesh, and
// removing that would take the rig with it.
function hideAlternates(root) {
  root.updateMatrixWorld(true);
  const kept = [];
  root.traverse((n) => {
    if (!n.isMesh || !n.visible) return;
    const box = new THREE.Box3().setFromObject(n);
    const verts = n.geometry && n.geometry.attributes.position ? n.geometry.attributes.position.count : 0;
    const version = (k) => centreDrift(k.box, box) < 0.5 && Math.max(k.verts, verts) <= 8 * Math.max(Math.min(k.verts, verts), 1);
    if (kept.some(version)) n.visible = false;
    else kept.push({ box, verts });
  });
  return root;
}

// centreDrift is how far apart two boxes sit, measured against the smaller one's own
// diagonal so it reads the same at any scale.
function centreDrift(a, b) {
  const span = Math.min(a.getSize(_posedV).length(), b.getSize(_posedV).length());
  return a.getCenter(new THREE.Vector3()).distanceTo(b.getCenter(new THREE.Vector3())) / Math.max(span, 1e-3);
}

function cloneRig(source) {
  const srcLookup = new Map(), cloneLookup = new Map();
  const clone = source.clone();
  (function walk(a, b) { srcLookup.set(b, a); cloneLookup.set(a, b); for (let i = 0; i < a.children.length; i++) walk(a.children[i], b.children[i]); })(source, clone);
  clone.traverse((node) => {
    if (!node.isSkinnedMesh) return;
    const src = srcLookup.get(node);
    node.skeleton = src.skeleton.clone();
    node.bindMatrix.copy(src.bindMatrix);
    node.skeleton.bones = src.skeleton.bones.map((b) => cloneLookup.get(b));
    node.bind(node.skeleton, node.bindMatrix);
  });
  return clone;
}

// poseAt drives root to a representative mid-clip frame and returns the mixer, so a
// still shows real motion instead of the bind (T-)pose. skeleton.pose() first clears
// any prior binding on a reused rig. A zero/NaN-duration clip falls back to frame 0.
function poseAt(root, clip) {
  root.traverse((o) => { if (o.isSkinnedMesh && o.skeleton) o.skeleton.pose(); });
  const mixer = new THREE.AnimationMixer(root);
  mixer.clipAction(clip).play();
  mixer.setTime(clip.duration > 0 ? clip.duration * 0.5 : 0);
  return mixer;
}

// ---- rig-agnostic clip retargeting for the mesh-less Synty animation clips ----
// The Synty animation clips are authored on one shared rig whose neutral is the T-pose
// clip A_TPose_Neut. Playing a clip's raw local rotations on a different character rig
// distorts it (their bind poses differ); rebasing each rotation through the shared
// neutral — rigBind · neutral⁻¹ · sourceFrame — makes any Synty T-pose character play any
// clip cleanly. syntyNeutral loads that neutral once (per-bone local quaternions).
let syntyNeutralPromise = null;
function syntyNeutral() {
  if (syntyNeutralPromise) return syntyNeutralPromise;
  syntyNeutralPromise = (async () => {
    try {
      // Searched by name, not filtered by vendor: the vendor facet is the user's own
      // directory name, so a library rooted at "Synty/" would match nothing here and
      // silently disable retargeting for the whole session.
      const r = await fetch('/api/assets?limit=8&group=0&q=' + encodeURIComponent('A_TPose_Neut'));
      const items = (await r.json()).items || [];
      const it = items.find((x) => x.name === 'A_TPose_Neut.fbx') || items[0];
      // Memoized either way, including the failure — one fetch decides retargeting for
      // the life of the page. So the one place it can go wrong says so: without the
      // neutral, retargetedFor hands back raw local rotations on a foreign rig, which is
      // the shredded pose this whole path exists to prevent, in the lightbox and in every
      // grid thumbnail, with nothing anywhere reporting it.
      if (!it) {
        console.warn('synty retargeting is off: no A_TPose_Neut.fbx in this library; clips from Synty packs will pose on their raw rotations');
        return null;
      }
      const o = await loadModel(contentURL(it.id), it.ext);
      const clip = (o.animations || [])[0];
      if (clip) { const m = new THREE.AnimationMixer(o); m.clipAction(clip).play(); m.setTime((clip.duration || 0) * 0.5); }
      const map = new Map();
      o.traverse((n) => { if (n.isBone) map.set(n.name, n.quaternion.clone()); });
      dispose(o);
      if (!map.size) console.warn('synty retargeting is off: A_TPose_Neut.fbx carries no bones');
      return map.size ? map : null;
    } catch (e) { console.warn('synty retargeting is off: the neutral pose could not be loaded', e); return null; }
  })();
  return syntyNeutralPromise;
}

// rebuiltClip is a clip with new tracks and everything else about the original kept.
// A bare AnimationClip constructor drops userData, and trimStaticTail records how much
// held pose it cut there — so a clip that was trimmed and then retargeted or stripped
// arrives at the viewer claiming it was not, and the marker that says so never appears
// for exactly the clips it was written for.
function rebuiltClip(clip, tracks) {
  const out = new THREE.AnimationClip(clip.name, clip.duration, tracks);
  out.userData = clip.userData;
  return out;
}

// retargetClip rebuilds clip's rotation tracks onto rig's rest pose through the neutral.
// Position/scale tracks pass through unchanged so vertical motion (a squat's hip drop, a
// jump) survives; horizontal locomotion is handled later by stripRootMotion. Returns the
// original clip when no rotation maps (so a native rig still plays).
function retargetClip(clip, neutral, rig) {
  rig.traverse((o) => { if (o.isSkinnedMesh && o.skeleton) o.skeleton.pose(); });
  // First of a repeated name wins, because that is the bone the mixer will drive:
  // three resolves a track to the first node carrying its name. Rebasing through a
  // later namesake's rest rotation poses every one of those bones from a rest it is
  // not in, which is the difference between a body and a shredded one.
  const bind = new Map();
  rig.traverse((n) => { if (n.isBone && !bind.has(n.name)) bind.set(n.name, n.quaternion.clone()); });
  const src = new THREE.Quaternion(), delta = new THREE.Quaternion(), inv = new THREE.Quaternion(), out = new THREE.Quaternion();
  const tracks = [];
  let rotated = 0;
  for (const tr of clip.tracks) {
    if (!tr.name.endsWith('.quaternion')) { tracks.push(tr); continue; }
    const bone = tr.name.slice(0, -'.quaternion'.length);
    const nq = neutral.get(bone), bq = bind.get(bone);
    if (!nq || !bq) continue;
    inv.copy(nq).invert();
    const v = tr.values, vv = new Float32Array(v.length);
    for (let i = 0; i < v.length; i += 4) {
      src.set(v[i], v[i + 1], v[i + 2], v[i + 3]);
      delta.copy(inv).multiply(src);
      out.copy(bq).multiply(delta);
      vv[i] = out.x; vv[i + 1] = out.y; vv[i + 2] = out.z; vv[i + 3] = out.w;
    }
    tracks.push(new THREE.QuaternionKeyframeTrack(tr.name, tr.times, vv));
    rotated++;
  }
  return rotated ? rebuiltClip(clip, tracks) : clip;
}

// isSynty tests the vendor facet, which is the first path segment of the user's own
// library — their directory name, verbatim, normalized nowhere. Comparing it
// case-sensitively means a library rooted at "Synty/" instead of "synty/" silently
// skips retargeting and poses every clip into the shredded output this exists to
// prevent, with no diagnostic anywhere.
function isSynty(vendor) {
  return (vendor || '').toLowerCase() === 'synty';
}

// retargetedFor returns a clip playable on rig: the Synty mesh-less clips are rebased
// through the shared neutral; everything else plays as-is.
async function retargetedFor(clip, vendor, rig) {
  if (!isSynty(vendor)) return clip;
  const neutral = await syntyNeutral();
  return neutral ? retargetClip(clip, neutral, rig) : clip;
}

// stripRootMotion zeroes the horizontal locomotion on the root bone's position track, so a
// walk/run/dash plays in place instead of drifting out of frame, while keeping the vertical
// axis (so squats, jumps, and hip bob still read). upAxis is the character's up in the clip's
// track space (from uprightRig); the two axes that aren't it are the horizontal ones. Defaults
// to Y-up — a Z-up file's forward travel is on Y, which the default would wrongly keep.
function stripRootMotion(clip, rootName, upAxis) {
  if (!rootName) return clip;
  const up = upAxis || new THREE.Vector3(0, 1, 0);
  const keep = [Math.abs(up.x) >= 0.5, Math.abs(up.y) >= 0.5, Math.abs(up.z) >= 0.5];
  let changed = false;
  const tracks = clip.tracks.map((tr) => {
    if (tr.name !== rootName + '.position') return tr;
    const v = tr.values.slice();
    for (let i = 0; i < v.length; i += 3) { if (!keep[0]) v[i] = 0; if (!keep[1]) v[i + 1] = 0; if (!keep[2]) v[i + 2] = 0; }
    changed = true;
    return new THREE.VectorKeyframeTrack(tr.name, tr.times, v);
  });
  return changed ? rebuiltClip(clip, tracks) : clip;
}

// dispose releases what an object owns. CLAY is deliberately exempt: it is one
// instance shared by every FBX mesh (and by every rig cloneRig hands out, since a
// clone shares its source's materials), and disposing a material evicts the renderer's
// compiled program for it. The thumbnail worker keeps one renderer for the life of the
// page, so disposing CLAY per model made it recompile that shader for every FBX
// thumbnail after the first.
function dispose(object) {
  object.traverse((o) => {
    if (o.geometry) o.geometry.dispose();
    if (o.material) {
      const mats = Array.isArray(o.material) ? o.material : [o.material];
      for (const m of mats) {
        if (m === CLAY) continue;
        for (const k in m) { if (m[k] && m[k].isTexture) m[k].dispose(); }
        m.dispose();
      }
    }
  });
  disposeSkeletons(object);
}

// disposeSkeletons releases the bone texture three allocates lazily on a skeleton's
// first render. It hangs off the Skeleton rather than off any material or geometry, so
// nothing in the walk above reaches it, and it is only freed by name. One per skinned
// model — and per clone, since each clone gets its own skeleton — retained for the life
// of a worker that never restarts.
function disposeSkeletons(object) {
  object.traverse((o) => {
    if (o.isSkinnedMesh && o.skeleton) o.skeleton.dispose();
  });
}

// disposeClone tears down a rig produced by cloneRig. Object3D.clone copies geometry
// and materials by reference, so the full dispose would free the buffers and compiled
// programs belonging to the cached template the clone came from — re-uploading the mesh
// and recompiling its shaders for every thumbnail, which is the cost caching the
// template exists to avoid. A clone owns only its skeletons.
function disposeClone(object) {
  disposeSkeletons(object);
}

// rigEntry describes an asset as a rig a clip can play on, or null when it is not one.
// Several skeletons carrying the same bone names is one rig replicated: a character
// whose props (a sword, a helmet) each ship on a copy of it, or a pack's whole cast
// stacked in one file. Either way oneCharacter reduces the file to a single body before
// anything poses on it. Several *different* skeletons is a file packing unrelated rigs
// together, where no one of them stands in for the rest, so it never becomes a rig —
// there the duplicate names bind ambiguously and shred the pose. Bone names are recorded
// deduped: a replicated skeleton would otherwise count its bones once per copy, which
// reads as a showcase mesh to match().
function rigEntry(item, root) {
  const skels = new Set();
  root.traverse((n) => { if (n.isSkinnedMesh && n.skeleton) skels.add(n.skeleton); });
  const families = new Set([...skels].map((s) => s.bones.map((b) => b.name).sort().join('|')));
  if (families.size > 1) return null;
  const bones = [...new Set(boneNames(root))];
  if (!isRenderable(root) || bones.length < 10) return null;
  return { id: item.id, name: item.name, ext: item.ext, bones, vendor: item.vendor };
}

// rigCandidates searches the library for assets that might serve as a rig. types is
// what the search will accept. A body shipped inside an animation pack is classified
// as an animation itself — nothing in its name marks it as the rig — so a caller that
// can rank what comes back asks for animations too; one that takes results in name
// order must not, or a pack's hundreds of clips crowd out its single body.
// Null, not [], when the search itself failed. The two are not the same answer: the
// callers memoize a scope as searched, and recording that against a request that never
// completed retires the scope for the session on the strength of a dropped connection.
async function rigCandidates({ q, vendor, limit, types, sort }) {
  const p = new URLSearchParams({ limit: String(limit), q });
  for (const t of types) p.append('type', t);
  if (vendor) p.set('vendor', vendor);
  if (sort) p.set('sort', sort);
  try {
    const res = await fetch('/api/assets?' + p);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    return (await res.json()).items || [];
  } catch (e) {
    console.warn('rig search failed', q, vendor || '', e);
    return null;
  }
}

// resolveRig finds a rig a clip can play on: the best registry match, the next one if
// that fails to load, then vendor discovery, then whatever that turns up. A cached
// entry goes stale when a re-index changes its id, so a failed load evicts the entry
// and the search continues rather than ending there.
//
// tryLoad is handed a registry entry and returns whatever the caller wants to keep —
// a loaded rig, or true for a caller that only cares that it played — or a falsy value
// when that entry could not be loaded. cancelled lets a caller that can be torn down
// mid-await stop between attempts.
//
// The lightbox and the thumbnail worker both search this way, and the order matters to
// what each of them shows: written out twice, one of them fell through to discovery
// once every known entry had failed and the other gave up there.
async function resolveRig(bones, asset, tryLoad, cancelled = () => false) {
  const attempt = async () => {
    for (let m = CharRegistry.match(bones, asset.vendor, asset.name); m && !cancelled(); m = CharRegistry.match(bones, asset.vendor, asset.name)) {
      const got = await tryLoad(m);
      if (got) return got;
      CharRegistry.remove(m.id);
    }
    return null;
  };
  await CharRegistry.seed();
  if (cancelled()) return null;
  // A registry holding any body that fits settles the clip here, and the pack search that
  // would turn up the body it is named after only runs when nothing fits at all — so a
  // registry written before this session preferred the named one would go on answering
  // with the other body for as long as it survives. Fetching the named body first is one
  // search and at most one load, once per vendor and series, and leaves the ranking to it.
  if (!CharRegistry.hasNamed(asset)) await CharRegistry.registerNamed(asset);
  if (cancelled()) return null;
  const known = await attempt();
  if (known || cancelled()) return known;
  await CharRegistry.discoverForVendor(asset, bones);
  if (cancelled()) return null;
  return attempt();
}

// packRigs picks what to try from the pack shipping a clip, heaviest first. Weight
// alone finds a character body, which outweighs every clip beside it — but not a prop
// rig: a bow its own clips animate is lighter than the character animations it ships
// with, and hundreds of those outrank it. Dropping the series the clip itself belongs
// to leaves the things a pack animates rather than the animations, and the heaviest of
// those is the rig. Only extensions loadModel can open are worth a load; a pack's
// heaviest file is often a .blend the preview cannot read at all.
async function packRigs(pack, vendor, name) {
  const page = await rigCandidates({ q: pack, vendor, limit: 500, types: ['model', 'animation'], sort: 'size' });
  return page && packRigCandidates(page, name);
}

// ---- character registry: match a clip-only animation to a rig it can play on ----
// A skinned character mesh whose bone names cover a clip's tracks can play that clip
// directly (proven for the Synty rig: the native body and the clips share a rest
// pose). Different rigs (e.g. the goblin A-pose rig) match a different body, or none
// — in-browser retargeting of these mesh-less clips is unreliable, so a non-matching
// rig falls back to the manual picker rather than a distorted pose.
const CharRegistry = {
  key: 'browsePreviewChars',
  seeded: false,
  list() { try { return JSON.parse(store.get(this.key)) || []; } catch { return []; } },
  save(l) { try { store.set(this.key, JSON.stringify(l.slice(0, 40))); } catch { /* quota */ } },
  // add records a character's rig, most recent first, and reports whether what the
  // matcher can pick actually changed — the order is refreshed on every lightbox open
  // and nothing downstream reads it.
  add(entry) {
    if (!entry.bones || entry.bones.length < 10) return false;
    const l = this.list();
    const prev = l.find((e) => e.id === entry.id);
    // rigEntry rebuilds an entry from the model and knows nothing about pinning, so the
    // flag is carried across here. Without it, opening a pinned character un-pins it.
    if (prev && prev.pinned) entry = { ...entry, pinned: true };
    const rest = l.filter((e) => e.id !== entry.id);
    rest.unshift(entry);
    this.save(rest);
    return !prev || JSON.stringify(prev) !== JSON.stringify(entry);
  },
  remove(id) { this.save(this.list().filter((e) => e.id !== id)); },
  // match picks the registered character whose skeleton best covers a clip's bones.
  // A pinned character that covers the clip wins over a higher-coverage unpinned one,
  // so pinning a body for a rig makes it the default for every clip on that rig.
  // clipName settles the bodies coverage cannot separate — a pack's variants share one
  // skeleton, so the one named for the clip's own series is the one it belongs on.
  // Auto-match is scoped to the clip's own vendor: cross-vendor skeletons share enough
  // bone names to pass the coverage bar but differ in rest pose, posing a clip into a
  // shredded/T-posed garbage still. A legacy entry with no recorded vendor is a wildcard
  // until it is re-registered (see register), so old caches keep working. The ranking
  // itself is matchRig, in rigmatch.js, where it is checked without a GL context.
  match(bones, vendor, clipName) { return matchRig(this.list(), bones, vendor, clipName); },
  // pin reports whether the flag moved, so a caller can tell a real change from a
  // click that re-asserted what was already true.
  pin(id, on) {
    const l = this.list();
    const e = l.find((x) => x.id === id);
    if (!e || !!e.pinned === !!on) return false;
    e.pinned = on;
    this.save(l);
    return true;
  },
  isPinned(id) { return !!(this.list().find((x) => x.id === id) || {}).pinned; },
  async register(item) {
    const known = this.list().find((e) => e.id === item.id);
    // Trust a known entry only when its bones were recorded one name per bone; an entry
    // holding a name per bone *instance* predates rigEntry and reads as a showcase mesh
    // to match(), so reload it rather than keeping it permanently unmatchable.
    if (known && known.bones.length === new Set(known.bones).size) {
      if (item.vendor && known.vendor !== item.vendor) { known.vendor = item.vendor; this.save(this.list().map((e) => e.id === item.id ? known : e)); }
      return true;
    }
    let root;
    try { root = await loadModel(contentURL(item.id), item.ext); } catch { return false; }
    const entry = rigEntry(item, root);
    dispose(root);
    if (entry) this.add(entry);
    return !!entry;
  },
  // hasNamed reports whether the body this clip is named after is already registered, so
  // registerNamed is only paid for when it is not. The ranking in match() does the rest.
  hasNamed(asset) { return hasNamedBody(this.list(), asset && asset.vendor, asset && asset.name); },
  // registerNamed looks up the one body a clip's own name points at — HumanM_Model for
  // HumanM@CombatIdle1H01 — and registers it so match() has it to rank. A name search
  // rather than discoverForVendor's sweep: this is asking for a specific file, not
  // establishing what a pack ships, and the sweep costs up to fourteen model loads where
  // this costs one query and at most one.
  //
  // The search is by series alone, so it also returns files that merely begin with those
  // letters; only an exact series match is the body meant. Preferring the clip's own pack
  // among them keeps a character to the pack it was indexed under, since a vendor ships
  // the same body in every pack. Once per vendor and series, whether or not it found
  // anything: a vendor that names its bodies nothing like its clips must not re-ask for
  // every clip in the library.
  namedTried: new Set(),
  async registerNamed(asset) {
    const series = nameSeries(asset && asset.name);
    if (!series) return;
    const scope = (asset.vendor || '') + '\x00' + series;
    if (this.namedTried.has(scope)) return;
    // Recorded before the search so a second clip in the series does not repeat it, and
    // taken back below if the search never completed: a dropped request is not evidence
    // that this vendor names no body after its clips.
    this.namedTried.add(scope);
    const items = await rigCandidates({ q: series, vendor: asset.vendor, limit: 8, types: ['model'] });
    if (!items) {
      this.namedTried.delete(scope);
      return;
    }
    const named = items.filter((it) => nameSeries(it.name) === series);
    const pick = named.find((it) => it.pack === asset.pack) || named[0];
    if (pick) await this.register(pick);
  },
  // Lazily discover a few character bodies by name so auto-match works before the
  // user has opened a matching character. Runs once per session; bounded; add()
  // dedups. It does NOT early-out on a non-empty registry — an already-registered
  // character may be the wrong rig for this clip, so the known bodies still get
  // discovered to cover their rigs.
  async seed() {
    if (this.seeded) return;
    this.seeded = true;
    const terms = ['PolygonSyntyCharacter', 'Character', 'SK_Mannequin', '_Model', 'Base_Mesh', 'SM_Chr'];
    let added = 0;
    let failed = false;
    for (const t of terms) {
      if (added >= 3) return;
      const items = await rigCandidates({ q: t, limit: 4, types: ['model'] });
      if (!items) { failed = true; continue; }
      for (const it of items.slice(0, 2)) { if (await this.register(it)) added++; if (added >= 3) return; }
    }
    // Registered nothing and at least one search never completed. "Seeded" has to mean
    // the question was asked, not that it was attempted: left set, a page that lost its
    // connection for a moment gives every mesh-less clip the category icon for the rest
    // of the session.
    if (failed && added === 0) this.seeded = false;
  },
  // When the seed didn't cover a clip's rig, look for a body in the clip's own vendor
  // (a clip's native character usually ships in the same vendor's packs).
  discovered: new Map(),
  async discoverForVendor(asset, want) {
    const { vendor, pack, name } = asset || {};
    if (!vendor) return;
    // Once per vendor+pack per skeleton, remembering the skeletons already searched for.
    // Establishing that a pack ships no body costs up to fourteen model downloads and
    // parses, and a candidate that turns out not to be a rig is never registered — so the
    // seen set below cannot remember it and every clip card in the pack would pay the
    // whole search again, on every grid rebuild. The skeleton belongs in the scope
    // because the search stops at the first rig covering the clip that reached it: a
    // second skeleton in the same pack would be left with nothing registered to play it.
    const scope = vendor + '\u0000' + (pack || '');
    const tried = this.discovered.get(scope);
    if (tried && searchedSkeleton(tried, want)) return;
    // Recorded before the search, because it is also what stops a second clip on this
    // skeleton repeating fourteen model loads while the first is still running. Rolled
    // back at the end if a search never completed: establishing that a pack ships no
    // body is expensive and worth remembering, but only when it was actually
    // established, and a failed request looks exactly like an empty result otherwise.
    this.discovered.set(scope, (tried || []).concat([want]));
    const rollback = () => {
      if (tried) this.discovered.set(scope, tried);
      else this.discovered.delete(scope);
    };
    // Load candidate bodies until one actually covers this clip (a single showcase mesh can
    // win by bone count yet be a multi-skeleton mesh register now rejects, and some bodies
    // ship a different skeleton family that shares no bone names). Stop at the first covering
    // rig; bound the loads so a vendor without a match doesn't scan the whole library.
    const seen = new Set(this.list().map((e) => e.id));
    let loaded = 0;
    let failed = false;
    // Reports whether the search is over — either a rig now covers the clip, the
    // per-vendor load budget is spent, or a search did not complete.
    const consider = async (items) => {
      if (!items) { failed = true; return true; }
      for (const it of items) {
        if (loaded >= 14) return true;
        if (seen.has(it.id)) continue;
        seen.add(it.id); loaded++;
        await this.register(it);
        if (want && this.match(want, vendor, name)) return true;
      }
      return false;
    };
    const search = async () => {
      // The pack shipping the clips usually ships what they animate, under whatever name
      // its vendor happens to use — a name search cannot find it, but weight can.
      if (pack && await consider(await packRigs(pack, vendor, name))) return;
      for (const t of ['Character', 'Hero', 'Human', 'Knight', 'Warrior', 'Body', 'Model', 'Base']) {
        if (await consider(await rigCandidates({ q: t, vendor, limit: 8, types: ['model'] }))) return;
      }
    };
    await search();
    if (failed) rollback();
  },
};

export {
  clipsForAsset, coversBones,
  loadModel, loadSidekick, normalizeClip, boneNames, clipBones, loadRMClips, isSynty,
  resolveRig, posedBox, frameBox, isRenderable, captureRootRest, uprightRig, prepareClipRig,
  cloneRig, oneCharacter, alignBindToRest, hideAlternates, poseAt, retargetedFor, stripRootMotion, dispose, disposeClone, CharRegistry, rigEntry, rigCandidates, CLAY, _posedV,
  rootBone, rootBoneName,
};
