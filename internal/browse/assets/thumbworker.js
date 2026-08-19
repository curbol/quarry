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
  captureRootRest, cloneRig, retargetedFor, dispose, disposeClone, CharRegistry, frameBox, contentURL, clipBones,
  rootBoneName,
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

function snap(object, box) {
  scene.add(object);
  frameBox(box, camera, null);
  renderer.render(scene, camera);
  scene.remove(object);
}

// build renders one thumbnail to the canvas and resolves true, or false when there is
// nothing to draw (a mesh-less clip with no matching rig).
async function build(asset) {
  if (asset.thumb === 'sidekick') return await buildSidekick(asset);
  const key = asset.source && asset.source.clip && asset.source.filePath;
  return key ? await buildShared(asset, key) : await buildStandalone(asset);
}

async function buildSidekick(asset) {
  const root = await loadSidekick(asset.source && asset.source.parts);
  if (!root) return false;
  const refBox = prepareClipRig(root, null);
  snap(root, refBox);
  dispose(root);
  return true;
}

async function buildStandalone(asset) {
  const obj = await loadModel(contentURL(asset.id), asset.ext);
  const rootRest = captureRootRest(obj);
  if (isRenderable(obj)) {
    const cs = clipsForAsset(obj, asset);
    const refBox = prepareClipRig(obj, null);
    if (cs.length) poseAt(obj, stripRootMotion(cs[0], rootBoneName(obj), obj.userData.upAxis));
    snap(obj, refBox);
    dispose(obj);
    return true;
  }
  const clips = clipsForAsset(obj, asset);
  dispose(obj);
  return clips.length ? await buildPosed(clips[0], asset, rootRest) : false;
}

async function buildShared(asset, key) {
  let pending = files.get(key);
  if (!pending) {
    pending = loadSharedFile(asset);
    files.set(key, pending);
    evictFiles();
  }
  const ctx = await pending;
  if (!ctx) return false;
  if (ctx.renderable) {
    const cs = clipsForAsset(ctx.obj, asset);
    const mixer = cs.length ? poseAt(ctx.obj, stripRootMotion(cs[0], ctx.rootBoneName, ctx.upAxis)) : null;
    snap(ctx.obj, ctx.refBox);
    if (mixer) mixer.stopAllAction();
    return true;
  }
  const cs = clipsForAsset(ctx.obj, asset);
  return cs.length ? await buildPosed(cs[0], asset, ctx.rootRest) : false;
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

// rigFor finds a character mesh that can wear this clip. A registry entry that fails
// to load is dropped and the search continues, the same recovery the lightbox makes:
// entries are cached across page loads, so a re-index leaves stale ids behind, and
// remembering the failure instead would make every clip thumbnail for that vendor fail
// for the rest of the session.
async function rigFor(clip, asset) {
  const vendor = asset.vendor;
  const bones = clipBones(clip);
  await CharRegistry.seed();
  let m = CharRegistry.match(bones, vendor);
  if (!m) { await CharRegistry.discoverForVendor(asset, bones); m = CharRegistry.match(bones, vendor); }
  while (m) {
    if (rigs.has(m.id)) return rigs.get(m.id);
    const rig = await loadModel(contentURL(m.id), m.ext)
      .then((r) => (isRenderable(r) ? r : (dispose(r), null)))
      .catch(() => null);
    if (rig) {
      rigs.set(m.id, rig);
      return rig;
    }
    CharRegistry.remove(m.id);
    m = CharRegistry.match(bones, vendor);
  }
  return null;
}

async function buildPosed(clip, asset, rootRest) {
  const vendor = asset.vendor;
  const template = await rigFor(clip, asset);
  if (!template) return false;
  const rig = cloneRig(template);
  const refBox = prepareClipRig(rig, isSynty(vendor) ? null : rootRest);
  const posed = stripRootMotion(await retargetedFor(clip, vendor, rig), rootBoneName(rig), rig.userData.upAxis);
  const mixer = poseAt(rig, posed);
  snap(rig, refBox);
  mixer.stopAllAction();
  disposeClone(rig);
  return true;
}

// downscale turns a source image into a grid-sized PNG. createImageBitmap resizes
// during decode, so the full-resolution bitmap is never resident on the main thread —
// which is the whole point, since a 4096² texture atlas is ~67MB decoded and a page of
// them is measured in gigabytes.
async function downscale(asset) {
  const res = await fetch(contentURL(asset.id));
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

function withTimeout(promise) {
  let timer;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error('thumbnail timed out')), JOB_TIMEOUT_MS);
  });
  return Promise.race([promise, deadline]).finally(() => clearTimeout(timer));
}

self.onmessage = (e) => {
  if (e.data.type === 'seed') {
    if (e.data.list && e.data.list.length) CharRegistry.save(e.data.list);
    return;
  }
  if (e.data.type === 'cancel') {
    jobs.cancel(e.data.id);
    return;
  }
  const { id, seq, asset } = e.data;
  jobs.note(id, seq);
  const current = () => jobs.isCurrent(id, seq);
  // Images bypass the queue: they never touch the shared GL canvas the queue exists to
  // serialize, and making them wait behind a 65MB model parse is what a grid of
  // textures would spend all its time doing.
  if (asset.thumb === 'image') {
    downscale(asset)
      .then((blob) => { if (current()) self.postMessage({ id, seq, blob }); })
      .catch(() => { if (current()) self.postMessage({ id, seq, blob: null }); });
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
        if (!(await build(asset))) return null;
        return canvas.convertToBlob({ type: 'image/png' });
      })());
      self.postMessage({ id, seq, blob: ok || null });
    } catch {
      self.postMessage({ id, seq, blob: null });
    }
  });
};
