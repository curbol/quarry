// Thumbnail worker: parses and renders every grid thumbnail off the main thread onto
// an OffscreenCanvas, returning a PNG blob. The main thread never parses a model or
// runs WebGL for the grid, so a 65 MB file can't freeze the page. The build logic
// mirrors the lightbox pipeline via the shared scene.js module.
// FBXLoader decodes embedded textures through DOM <img> elements, which a worker has
// no document for. Thumbnails render textureless clay (loadModel overrides every
// material), so stand in a no-op image that reports "loaded" immediately — letting such
// FBX parse in the worker. Runs at module init, before any job parses an FBX.
if (typeof document === 'undefined') {
  const fakeImage = () => {
    const on = {};
    return {
      style: {}, width: 1, height: 1, complete: true,
      addEventListener(t, f) { on[t] = f; },
      removeEventListener(t) { delete on[t]; },
      set src(v) { this._src = v; queueMicrotask(() => on.load && on.load()); },
      get src() { return this._src; },
    };
  };
  globalThis.document = { createElementNS: () => fakeImage(), createElement: () => fakeImage() };
}

// An FBX that embeds its textures as binary content hands them to the renderer as blob
// URLs minted through window.URL, and a worker has a global URL but no window at all —
// without this the whole parse throws and the card keeps its category icon.
if (typeof window === 'undefined') {
  globalThis.window = { URL };
}

import * as THREE from '/static/vendor/three/three.module.min.js';
import {
  loadModel, loadSidekick, clipsForAsset, prepareClipRig, poseAt, stripRootMotion, isRenderable, isSynty,
  captureRootRest, cloneRig, oneCharacter, alignBindToRest, hideAlternates, retargetedFor, dispose, disposeClone, CharRegistry, frameBox, contentURL, clipBones,
  rootBoneName, resolveRig,
} from '/static/scene.js';
import { JobTracker } from '/static/jobtracker.js';

const SIZE = 220;
const canvas = new OffscreenCanvas(SIZE, SIZE);
let renderer, scene, camera;

// Create the GL context lazily on the first job, not at worker load: creating it while
// the page is still initializing races the main thread and can fail (swiftshader's
// "BindToCurrentSequence failed"). Retry a few times to ride out a transient failure.
async function ensureRenderer() {
  if (renderer) return;
  for (let attempt = 0; ; attempt++) {
    try {
      renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
      renderer.setSize(SIZE, SIZE, false);
      break;
    } catch (e) {
      if (attempt >= 4) throw e;
      await new Promise((r) => setTimeout(r, 150));
    }
  }
  // A lost context renders nothing but still resolves convertToBlob, so without this
  // every later thumbnail would come back as a blank image with no error anywhere.
  // Dropping the reference makes the next job build a fresh renderer.
  canvas.addEventListener?.('webglcontextlost', (e) => {
    e.preventDefault();
    renderer = null;
  });
  scene = new THREE.Scene();
  scene.add(new THREE.HemisphereLight(0xffffff, 0x33343a, 2.6));
  const dir = new THREE.DirectionalLight(0xffffff, 2.2);
  dir.position.set(4, 6, 5);
  scene.add(dir);
  camera = new THREE.PerspectiveCamera(45, 1, 0.1, 1000);
}

const files = new Map(); // file path -> parsed+oriented file, reused across its split clips
const rigs = new Map();  // matched character id -> loaded rig, cloned per clip

// snap draws one object into the shared canvas, or declines when the job asking for it
// has been abandoned. current is threaded all the way down here rather than checked
// only around build(), because the deadline stops a job from being *waited on* while
// its work keeps running: every await below is a place an abandoned job can resume,
// and the canvas, the camera and the two caches are the very things the queue exists
// to give each job alone.
function snap(object, box, current) {
  if (!current()) return false;
  scene.add(object);
  frameBox(box, camera, null);
  renderer.render(scene, camera);
  scene.remove(object);
  return true;
}

// build renders one thumbnail to the canvas and resolves true, or false when there is
// nothing to draw (a mesh-less clip with no matching rig) or the job was abandoned.
async function build(asset, current) {
  if (asset.thumb === 'sidekick') return await buildSidekick(asset, current);
  const key = asset.source && asset.source.clip && asset.source.filePath;
  return key ? await buildShared(asset, key, current) : await buildStandalone(asset, current);
}

async function buildSidekick(asset, current) {
  const root = await loadSidekick(asset.source && asset.source.parts);
  if (!root) return false;
  const refBox = prepareClipRig(root, null);
  const ok = snap(root, refBox, current);
  dispose(root);
  return ok;
}

async function buildStandalone(asset, current) {
  const obj = await loadModel(contentURL(asset.id), asset.ext);
  const rootRest = captureRootRest(obj);
  if (isRenderable(obj)) {
    const cs = clipsForAsset(obj.animations, asset);
    const refBox = prepareClipRig(obj, null);
    if (cs.length) poseAt(obj, stripRootMotion(cs[0], rootBoneName(obj), obj.userData.upAxis));
    const ok = snap(obj, refBox, current);
    dispose(obj);
    return ok;
  }
  const clips = clipsForAsset(obj.animations, asset);
  dispose(obj);
  return clips.length ? await buildPosed(clips[0], asset, rootRest, current) : false;
}

async function buildShared(asset, key, current) {
  let pending = files.get(key);
  if (!pending) {
    // Checked before the cache is touched, not only before the draw: evictFiles
    // disposes the oldest entry outright, and an abandoned job seeding a new one can
    // push out the file the running job is holding.
    if (!current()) return false;
    pending = loadSharedFile(asset);
    files.set(key, pending);
    evictFiles();
  }
  const ctx = await pending;
  if (!ctx) return false;
  if (ctx.renderable) {
    const cs = clipsForAsset(ctx.obj.animations, asset);
    const mixer = cs.length ? poseAt(ctx.obj, stripRootMotion(cs[0], ctx.rootBoneName, ctx.upAxis)) : null;
    const ok = snap(ctx.obj, ctx.refBox, current);
    if (mixer) mixer.stopAllAction();
    return ok;
  }
  const cs = clipsForAsset(ctx.obj.animations, asset);
  return cs.length ? await buildPosed(cs[0], asset, ctx.rootRest, current) : false;
}

async function loadSharedFile(asset) {
  const obj = await loadModel(contentURL(asset.id), asset.ext);
  if (isRenderable(obj)) {
    const refBox = prepareClipRig(obj, null);
    return { renderable: true, obj, refBox, upAxis: obj.userData.upAxis, rootBoneName: rootBoneName(obj) };
  }
  return { renderable: false, obj, rootRest: captureRootRest(obj) };
}

function evictFiles() {
  const CAP = 6;
  while (files.size > CAP) {
    const oldest = files.keys().next().value;
    const pending = files.get(oldest);
    files.delete(oldest);
    Promise.resolve(pending).then((ctx) => { if (ctx && ctx.obj) dispose(ctx.obj); }).catch(() => {});
  }
}

// A rig template is a whole character — geometry, materials, textures — kept so every
// clip on that skeleton clones it instead of reloading it. A library spanning several
// vendors' animation packs supplies a distinct one per character matched, so the set
// is bounded like the file cache above. The template owns what it holds (a clone
// shares it), so an evicted one is disposed outright rather than as a clone. Jobs are
// serialized and each disposes its clone before returning, so nothing evicted here is
// still on screen — which holds only because an abandoned job, the one thing that does
// run alongside another, is stopped from reaching rigs.set below.
function evictRigs() {
  const CAP = 4;
  while (rigs.size > CAP) {
    const oldest = rigs.keys().next().value;
    const rig = rigs.get(oldest);
    rigs.delete(oldest);
    dispose(rig);
  }
}

// rigFor finds a character mesh that can wear this clip. A registry entry that fails
// to load is dropped and the search continues, the same recovery the lightbox makes:
// entries are cached across page loads, so a re-index leaves stale ids behind, and
// remembering the failure instead would make every clip thumbnail for that vendor fail
// for the rest of the session.
async function rigFor(clip, asset, current) {
  return resolveRig(clipBones(clip), asset, async (m) => {
    // An abandoned job stops loading candidates rather than working through the
    // remaining thirteen, and above all stops writing to the template cache.
    if (!current()) return null;
    const cached = rigs.get(m.id);
    if (cached) return cached;
    // A candidate that will not load is not a failure of this thumbnail — rig
    // discovery tries up to fourteen of them — but a whole vendor failing to load is,
    // and silently it looks identical to a vendor with no rig at all.
    const rig = await loadModel(contentURL(m.id), m.ext)
      .then((r) => (isRenderable(r) ? alignBindToRest(oneCharacter(r)) : (dispose(r), null)))
      .catch((e) => { console.warn('rig candidate failed to load', m.id, m.name, e); return null; });
    if (!rig) return null;
    if (!current()) {
      // Loaded after the job was abandoned: caching it would evict a template the
      // running job may already have cloned, and a clone shares what it holds.
      dispose(rig);
      return null;
    }
    rigs.set(m.id, rig);
    evictRigs();
    return rig;
  });
}

async function buildPosed(clip, asset, rootRest, current) {
  const vendor = asset.vendor;
  const template = await rigFor(clip, asset, current);
  if (!template) return false;
  const rig = hideAlternates(cloneRig(template));
  const refBox = prepareClipRig(rig, isSynty(vendor) ? null : rootRest);
  const posed = stripRootMotion(await retargetedFor(clip, vendor, rig), rootBoneName(rig), rig.userData.upAxis);
  const mixer = poseAt(rig, posed);
  const ok = snap(rig, refBox, current);
  mixer.stopAllAction();
  disposeClone(rig);
  return ok;
}

// downscale turns a source image into a grid-sized PNG. createImageBitmap resizes
// during decode, so the full-resolution bitmap is never resident on the main thread —
// which is the whole point, since a 4096² texture atlas is ~67MB decoded and a page of
// them is measured in gigabytes.
async function downscale(asset, signal) {
  const res = await fetch(contentURL(asset.id), { signal });
  if (!res.ok) throw new Error('HTTP ' + res.status);
  const src = await createImageBitmap(await res.blob(), { resizeWidth: SIZE, resizeQuality: 'medium' });
  // Its own canvas, not the shared 3D one: this runs off the queue, so drawing onto
  // that canvas would race whatever render is in flight.
  const c = new OffscreenCanvas(src.width, src.height);
  c.getContext('2d').drawImage(src, 0, 0);
  src.close();
  return c.convertToBlob({ type: 'image/png' });
}

// Serialize jobs: one render on the shared canvas at a time, converted to a blob before
// the next job overwrites the canvas.
let queue = Promise.resolve();

// Which queued jobs are still wanted. Without this a filter change leaves the queue
// working through a backlog for cards nobody is looking at before it renders what is
// on screen. See jobtracker.js for why the decision is per request rather than per id.
const jobs = new JobTracker();

// JOB_TIMEOUT_MS bounds one render. A stalled fetch or a pathological parse would
// otherwise hold the single queue forever, and every card behind it keeps its spinner
// for the rest of the session. Giving up draws the category icon instead.
const JOB_TIMEOUT_MS = 30_000;

// reportFailure names a thumbnail that will not appear. Without it the three cases a
// user sees as one — this clip has no rig, this file is corrupt, the server answered
// 500 — are all a category icon and an empty console, and the page then remembers the
// answer, so a transport failure looks exactly like a settled one.
function reportFailure(asset, e) {
  console.error('thumbnail failed', asset && asset.id, asset && asset.name, e);
}

function withTimeout(promise, onExpire) {
  let timer;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => {
      if (onExpire) onExpire();
      reject(new Error('thumbnail timed out'));
    }, JOB_TIMEOUT_MS);
  });
  return Promise.race([promise, deadline]).finally(() => clearTimeout(timer));
}

self.onmessage = (e) => {
  if (e.data.type === 'seed') {
    // Merged, not saved over. The worker's registry is its own in-memory store (there
    // is no localStorage here), and discoverForVendor fills it with bodies the main
    // thread has never seen. Replacing it wholesale dropped those — permanently, since
    // the memos that record a scope as already searched live on CharRegistry rather
    // than in the store and so survive the wipe, short-circuiting every re-search.
    // add() unshifts, so the incoming list is applied backwards to keep its order.
    const list = e.data.list || [];
    for (let i = list.length - 1; i >= 0; i--) CharRegistry.add(list[i]);
    return;
  }
  if (e.data.type === 'cancel') {
    jobs.cancel(e.data.id);
    return;
  }
  const { id, seq, asset } = e.data;
  jobs.note(id, seq);
  const current = () => jobs.isCurrent(id, seq);
  // settle posts a result and retires the job. Retiring here rather than waiting for a
  // cancel from the page is what keeps the tracker one entry per *wanted* asset: the
  // page drops its pending entry when a result lands, so the cancel it would otherwise
  // send is skipped, and every job that succeeded would sit in the map for the life of
  // the worker. Only a current job retires — a superseded one's id already belongs to
  // the newer request.
  const settle = (blob) => {
    if (!current()) return;
    jobs.cancel(id);
    self.postMessage({ id, seq, blob });
  };
  // Images bypass the queue: they never touch the shared GL canvas the queue exists to
  // serialize, and making them wait behind a 65MB model parse is what a grid of
  // textures would spend all its time doing.
  if (asset.thumb === 'image') {
    // Off the queue, but not off the deadline. A fetch that never settles posts
    // nothing at all — not even the null — so the card keeps its spinner and the main
    // thread's pending entry is never cleared for a later holder to re-ask.
    const ac = new AbortController();
    withTimeout(downscale(asset, ac.signal), () => ac.abort())
      .then((blob) => settle(blob))
      .catch((e) => { reportFailure(asset, e); settle(null); });
    return;
  }
  queue = queue.then(async () => {
    if (!current()) return; // superseded or cancelled while it waited its turn
    try {
      // The timeout covers the whole job, not just build(): creating the context and
      // encoding the blob can stall too, and either one hanging outside the deadline
      // would wedge this single queue and leave every card behind it spinning.
      const ok = await withTimeout((async () => {
        await ensureRenderer();
        // Re-checked here and threaded into build, because a deadline only stops this
        // job from being waited on — the work itself keeps running, and everything
        // below it touches the one canvas, camera and two caches the queue exists to
        // give each job alone. Bracketing build() alone left every await inside it
        // unguarded: an abandoned job reaching snap() renders into the canvas the next
        // one is about to encode, and its model is then cached under that card's id.
        if (!current()) return null;
        if (!(await build(asset, current))) return null;
        if (!current()) return null;
        return canvas.convertToBlob({ type: 'image/png' });
      })());
      settle(ok || null);
    } catch (e) {
      reportFailure(asset, e);
      settle(null);
    }
  });
};
