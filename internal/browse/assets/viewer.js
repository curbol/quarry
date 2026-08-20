// The lightbox's 3D preview: one WebGL context shared by every open, and the viewer
// built over it — camera, lights, ground, orbit controls, the clip playback bar, the
// root-motion toggle, and the character chooser a mesh-less clip needs.
//
// Its own module because it is the only three.js consumer on the page. Keeping it in
// app.js left the grid, the search, the filters and the tagging sharing a file with a
// renderer none of them touch. What it needs from the page is one element, passed in
// rather than reached for.

import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import {
  contentURL, thumbURL, loadModel, loadSidekick, normalizeClip, clipBones, clipsForAsset,
  loadRMClips, isSynty, coversBones, resolveRig, posedBox, frameBox, isRenderable, captureRootRest,
  uprightRig, prepareClipRig, poseAt, retargetedFor, stripRootMotion, dispose, CharRegistry,
  rigEntry, rigCandidates, rootBoneName, hideAlternates, CLAY, _posedV,
} from '/static/scene.js';
import { iconEl } from '/static/icons.js';
import { modelThumbs } from '/static/thumbs.js';

// One WebGL context for every preview, created on first use and reused for the life
// of the page.
//
// A renderer per lightbox open would leak a context each time: three's dispose()
// releases its own resources but not the GL context (forceContextLoss is a separate
// call), and the canvas is only collected whenever GC gets to it. Browsers cap
// contexts per renderer process (~16) and evict the *oldest* when the cap is hit —
// which is the thumbnail worker's, created once and reused for the page's life. So
// arrowing through a couple of dozen models would silently blank every 3D thumbnail
// in the grid for the rest of the session, with nothing logged anywhere.
let sharedRenderer = null;
function acquireRenderer() {
  if (sharedRenderer) return sharedRenderer;
  const r = new THREE.WebGLRenderer({ antialias: true });
  r.setClearColor(0x14161d, 1);
  r.shadowMap.enabled = true;
  r.shadowMap.type = THREE.PCFSoftShadowMap;
  // A context can still be lost for reasons outside our control (GPU reset, tab
  // backgrounded too long). Dropping the reference is what lets the next open build a
  // working one instead of rendering forever into a dead canvas.
  r.domElement.addEventListener('webglcontextlost', (e) => {
    e.preventDefault();
    if (sharedRenderer === r) sharedRenderer = null;
  });
  sharedRenderer = r;
  return r;
}

export function startViewer(container, asset, panels) {
  const w = container.clientWidth || 600, h = container.clientHeight || 500;
  const renderer = acquireRenderer();
  renderer.setSize(w, h);
  renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
  container.appendChild(renderer.domElement);
  const scene = new THREE.Scene();
  scene.add(new THREE.HemisphereLight(0xffffff, 0x2a2c33, 3.0));
  const dir = new THREE.DirectionalLight(0xffffff, 2.4); dir.position.set(4, 6, 5); dir.castShadow = true; scene.add(dir); scene.add(dir.target);
  dir.shadow.mapSize.set(2048, 2048); dir.shadow.bias = -0.0005;
  const fill = new THREE.DirectionalLight(0xffffff, 1.0); fill.position.set(-4, 2, -3); scene.add(fill);
  const camera = new THREE.PerspectiveCamera(45, w / h, 0.1, 5000);
  const controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;
  controls.enablePan = false; // the lock toggle (applyLock, below) sets the turntable constraint

  // A ground grid under the character's feet gives the pose a floor to read against, plus
  // a transparent shadow-catcher plane so the directional light drops a soft shadow, and
  // the light's shadow frustum is sized to the character (Synty rigs are ~100+ units tall).
  let ground = null, shadowPlane = null;
  const placeGround = (object, box) => {
    if (!box || box.isEmpty()) return;
    const size = box.getSize(new THREE.Vector3()), center = box.getCenter(new THREE.Vector3());
    const span = Math.max(size.x, size.y, size.z) || 1;
    // Ground at the lowest bone (feet), not box.min.y — posedBox pads its bounds, which
    // would float the plane ~0.1 below the feet and leave the character hovering.
    let footY = Infinity;
    object.traverse((n) => { if (n.isBone) footY = Math.min(footY, n.getWorldPosition(_posedV).y); });
    if (!isFinite(footY)) footY = box.min.y;
    if (ground) { scene.remove(ground); ground.geometry.dispose(); ground.material.dispose(); }
    ground = new THREE.GridHelper(Math.max(size.x, size.z) * 3 || 1, 16, 0x40444f, 0x2b2e37);
    ground.position.set(center.x, footY, center.z);
    scene.add(ground);
    if (shadowPlane) { scene.remove(shadowPlane); shadowPlane.geometry.dispose(); shadowPlane.material.dispose(); }
    shadowPlane = new THREE.Mesh(new THREE.PlaneGeometry(span * 4, span * 4), new THREE.ShadowMaterial({ opacity: 0.32 }));
    shadowPlane.rotation.x = -Math.PI / 2;
    shadowPlane.position.set(center.x, footY, center.z);
    shadowPlane.receiveShadow = true;
    scene.add(shadowPlane);
    dir.position.set(center.x + span, box.max.y + span * 1.5, center.z + span * 0.6);
    dir.target.position.copy(center); dir.target.updateMatrixWorld();
    const sc = dir.shadow.camera;
    sc.near = span * 0.1; sc.far = span * 6; sc.left = -span; sc.right = span; sc.top = span; sc.bottom = -span;
    sc.updateProjectionMatrix();
  };
  // Aim slightly above the vertical centre so the character sits centred in the viewport
  // (a touch of lift reads more naturally than dead-centre without wasting the top third).
  const eyeLevel = (box) => {
    if (!box || box.isEmpty()) return;
    const dy = (box.min.y + (box.max.y - box.min.y) * 0.55) - controls.target.y;
    controls.target.y += dy;
    camera.position.y += dy;
    controls.update();
  };
  // A small corner gizmo showing the world axes from the current view, so orientation is
  // legible while turntable-spinning (X red, Y green/up, Z blue).
  const gizmoScene = new THREE.Scene();
  gizmoScene.add(new THREE.AxesHelper(1));
  const gizmoCam = new THREE.OrthographicCamera(-1.5, 1.5, 1.5, -1.5, 0.1, 10);
  const viewSize = new THREE.Vector2();

  const clock = new THREE.Clock();
  let raf = 0, obj = null, stopped = false;
  let mixer = null, action = null, clips = [], soloClips = null, soloRootRest = null, clipDur = 0, playing = true, ctrls = null, curTrimmedFrom = 0;
  let rawClips = [], playRootName = null, playUpAxis = null, motionOn = false, curClip = 0;
  let playInPlace = [], playMotion = []; // the two clip sets the root-motion toggle swaps between

  // View controls overlaid on the canvas: three view modes (isometric default / flat
  // eye-level / free rotation), and — for a root-motion clip — show the travel or in place.
  const ISO_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2 3 7v10l9 5 9-5V7z"/><path d="M3 7l9 5 9-5M12 12v10"/></svg>';
  const FLAT_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 14h18"/></svg>';
  const FREE_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v5h-5"/></svg>';
  const MOVE_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 9l-3 3 3 3M9 5l3-3 3 3M15 19l-3 3-3-3M19 9l3 3-3 3M2 12h20M12 2v20"/></svg>';
  const DEFAULT_POLAR = Math.PI * 0.36; // elevated 3/4 (isometric-ish) angle
  const toolbar = document.createElement('div'); toolbar.className = 'lb-viewtools';
  const mkBtn = (cls, svg, title, onClick) => {
    const btn = document.createElement('button'); btn.type = 'button'; btn.className = 'lb-viewbtn ' + cls;
    btn.innerHTML = svg; btn.title = title; btn.addEventListener('click', onClick); toolbar.appendChild(btn); return btn;
  };
  const viewBtns = {};
  const setViewMode = (mode) => {
    if (mode === 'iso') { controls.minPolarAngle = controls.maxPolarAngle = DEFAULT_POLAR; }
    else if (mode === 'flat') { controls.minPolarAngle = controls.maxPolarAngle = Math.PI / 2; }
    else { controls.minPolarAngle = 0.0001; controls.maxPolarAngle = Math.PI - 0.0001; }
    for (const m in viewBtns) viewBtns[m].classList.toggle('on', m === mode);
    controls.update();
  };
  viewBtns.iso = mkBtn('', ISO_ICON, 'Isometric view', () => setViewMode('iso'));
  viewBtns.flat = mkBtn('', FLAT_ICON, 'Flat (eye-level) view', () => setViewMode('flat'));
  viewBtns.free = mkBtn('', FREE_ICON, 'Free rotation', () => setViewMode('free'));
  const moveBtn = mkBtn('lb-move', MOVE_ICON, '', () => {
    motionOn = !motionOn;
    moveBtn.classList.toggle('on', motionOn);
    moveBtn.title = motionOn ? 'Showing root motion — click to play in place' : 'Playing in place — click to show root motion';
    clips = motionOn ? playMotion : playInPlace;
    // The picker lists whichever set is live; the two can differ in both length and
    // names, so leaving it alone would label these clips with the other set's.
    if (ctrls) ctrls.setClips(clips);
    playClip(curClip);
  });
  moveBtn.hidden = true;
  container.appendChild(toolbar);
  setViewMode('iso');

  // The container and the renderer are shared with every other preview, and a load
  // that fails can resolve long after the user moved on. Without this a dead viewer
  // detaches the live one's canvas, deletes its controls, and writes its own error
  // over whatever the user is now looking at — and nothing reattaches the canvas, so
  // the live viewer renders into a detached one for the rest of the session.
  const clearOverlays = () => {
    if (stopped) return;
    container.querySelectorAll('.lb-placeholder,.lb-controls').forEach((e) => e.remove());
  };
  const ensureCanvas = () => {
    if (stopped) return;
    if (!renderer.domElement.isConnected) container.appendChild(renderer.domElement);
    // The loop stops itself when the canvas leaves the DOM, so put it back.
    if (!raf) loop();
  };
  const showPlaceholder = (text) => {
    if (stopped) return;
    if (obj) { scene.remove(obj); dispose(obj); obj = null; }
    // The canvas goes, and with it the reason to keep rendering: a placeholder lightbox
    // would otherwise drive a full scene pass plus a gizmo pass every frame into a
    // canvas nobody can see, for as long as it stays open.
    cancelAnimationFrame(raf);
    raf = 0;
    renderer.domElement.remove();
    clearOverlays();
    const box = document.createElement('div');
    box.className = 'lb-placeholder';
    // Show the same Unity-rendered preview the grid card uses, when there is one, so
    // the viewer matches the card instead of dropping to a bare icon.
    if (asset.thumb === 'preview') {
      const img = new Image(); img.className = 'lb-preview'; img.src = thumbURL(asset.id);
      img.onerror = () => img.replaceWith(iconEl(asset.category));
      box.appendChild(img);
    } else {
      box.appendChild(iconEl(asset.category));
    }
    if (text) { const p = document.createElement('p'); p.textContent = text; box.appendChild(p); }
    container.appendChild(box);
  };

  const playClip = (i) => {
    // Guarded because the root-motion toggle swaps `clips` between two sets that need
    // not be the same length: a whole-file RM sibling holds one clip where the in-place
    // file holds many, so an index valid a moment ago can now be past the end, and
    // clipAction(undefined) throws out of the click handler and leaves the toggle stuck.
    const clip = clips[i] || clips[0];
    if (!clip) return;
    if (action) action.stop();
    curClip = clips.indexOf(clip);
    action = mixer.clipAction(clip);
    action.reset(); action.play();
    clipDur = clip.duration || 0;
    curTrimmedFrom = (clip.userData && clip.userData.trimmedFrom) || 0;
    playing = true;
    if (ctrls) ctrls.setClip(curClip);
  };

  const buildPlayback = (mixerRoot, cs, charInfo, rootRest, rmCs) => {
    const rootName = rootBoneName(mixerRoot);
    mixerRoot.traverse((o) => { if (o.isMesh) o.castShadow = true; });
    // Correct orientation and measure the framing box first, from the character's constant
    // reference (bind) pose — the shared prepareClipRig, so thumbnail and lightbox stay in
    // lockstep. It records the up axis (for in-place stripping); then the clip plays inside
    // the fixed frame. scale, centering and the ground stay fixed no matter what the clip does.
    const refBox = prepareClipRig(mixerRoot, rootRest);
    rawClips = cs; playRootName = rootName; playUpAxis = mixerRoot.userData.upAxis;
    const rmClips = rmCs || [];
    // With a paired RM sibling, in-place is the native (non-RM) clips and the travel view is
    // the RM clips — both play on this same skeleton. Without one, fall back to stripping a
    // baked-motion clip in place algorithmically.
    playInPlace = rmClips.length ? cs : cs.map((c) => stripRootMotion(c, rootName, playUpAxis));
    playMotion = rmClips.length ? rmClips : cs;
    clips = motionOn ? playMotion : playInPlace;
    moveBtn.hidden = !(rmClips.length || asset.bakedMotion);
    mixer = new THREE.AnimationMixer(mixerRoot);
    ctrls = makeControls();
    renderCharacter(charInfo);
    frameBox(refBox, camera, controls);
    placeGround(mixerRoot, refBox);
    eyeLevel(refBox);
    playClip(0);
  };

  // charSeq orders character selections. Loading and retargeting a body takes long
  // enough to pick another one meanwhile, and without this the slower of the two wins
  // whichever the user chose last. A superseded load reports success so its caller
  // leaves the viewer alone for the selection that replaced it.
  let charSeq = 0;

  // Load a chosen character and play the pending clip-only clips on it. Resolves
  // true on success, false if the character couldn't load — so callers can fall
  // back to another rig or the picker instead of leaving an empty viewer.
  const useCharacter = (item) => {
    const mine = ++charSeq;
    const superseded = () => stopped || mine !== charSeq;
    return loadModel(contentURL(item.id), item.ext).then(async (char) => {
      if (superseded()) { dispose(char); return true; }
      const entry = rigEntry(item, char);
      if (entry && CharRegistry.add(entry)) modelThumbs.reseed();
      hideAlternates(char);
      const clips = await Promise.all(soloClips.map((c) => retargetedFor(c, asset.vendor, char)));
      let rmCs = null;
      const rmRaw = await loadRMClips(asset); // travel sibling, retargeted onto the same body
      if (rmRaw) rmCs = await Promise.all(rmRaw.map((c) => retargetedFor(c, asset.vendor, char)));
      if (superseded()) { dispose(char); return true; }
      clearOverlays(); ensureCanvas();
      if (obj) { scene.remove(obj); dispose(obj); }
      obj = char; scene.add(char);
      mixer = null; action = null;
      buildPlayback(char, clips, { id: item.id, name: item.name }, isSynty(asset.vendor) ? null : soloRootRest, rmCs);
      return true;
    }).catch(() => false);
  };

  // ---- character sidebar panel ----

  const SAVE_SVG = '<svg viewBox="0 0 24 24" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"><path d="M6 3h12v18l-6-4-6 4z"/></svg>';

  const useAndFallback = async (item, forBones) => {
    const ok = await useCharacter(item);
    if (stopped) return;
    if (!ok) showCharacterChooser(forBones, 'That model could not be loaded. Try another.');
  };

  // characterSearch is a single autocomplete combobox: a text input whose dropdown of
  // matches appears only on focus/typing. On empty focus it suggests the rigs already
  // known to fit this clip (not a random dump); typing searches all models.
  function characterSearch(forBones, current) {
    const box = document.createElement('div'); box.className = 'lb-combo';
    const input = document.createElement('input');
    input.type = 'search'; input.className = 'lb-comboinput'; input.autocomplete = 'off';
    input.placeholder = current ? 'change character…' : 'search characters…';
    const drop = document.createElement('div'); drop.className = 'lb-drop'; drop.hidden = true;
    box.append(input, drop);

    let seq = 0, items = [], active = -1;
    const choose = (it) => { drop.hidden = true; useAndFallback(it, forBones); };
    const render = () => {
      drop.replaceChildren();
      if (!items.length) { drop.hidden = true; return; }
      items.forEach((it, i) => {
        const r = document.createElement('button'); r.type = 'button'; r.className = 'lb-result' + (i === active ? ' active' : '');
        const nm = document.createElement('span'); nm.className = 'lb-result-name'; nm.textContent = it.name.replace(/\.[^.]+$/, '');
        const sub = document.createElement('span'); sub.className = 'lb-result-sub'; sub.textContent = it.sub || [it.vendor, it.pack].filter(Boolean).join(' · ');
        r.append(nm, sub); r.title = it.name;
        r.addEventListener('mousedown', (e) => { e.preventDefault(); choose(it); }); // fire before blur
        drop.appendChild(r);
      });
      drop.hidden = false;
    };
    const suggestKnown = () => {
      const out = [], seen = new Set();
      if (current && current.name) seen.add(current.name);
      for (const e of CharRegistry.list()) {
        if ((current && e.id === current.id) || seen.has(e.name)) continue;
        if (forBones && !coversBones(e.bones, forBones)) continue;
        seen.add(e.name); out.push({ id: e.id, name: e.name, ext: e.ext, sub: 'fits this animation' });
        if (out.length >= 6) break;
      }
      return out;
    };
    const run = async (q) => {
      const my = ++seq;
      if (!q) { items = suggestKnown(); active = -1; render(); return; }
      const found = await rigCandidates({ q, limit: 8, types: ['model', 'animation'] });
      if (my === seq) { items = found; active = -1; render(); }
    };
    let t;
    input.addEventListener('input', () => { clearTimeout(t); t = setTimeout(() => run(input.value.trim()), 180); });
    input.addEventListener('focus', () => run(input.value.trim()));
    input.addEventListener('blur', () => setTimeout(() => { drop.hidden = true; }, 120));
    input.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown') { e.preventDefault(); if (items.length) { active = Math.min(active + 1, items.length - 1); render(); } }
      else if (e.key === 'ArrowUp') { e.preventDefault(); if (items.length) { active = Math.max(active - 1, 0); render(); } }
      else if (e.key === 'Enter') { e.preventDefault(); const it = items[active] || items[0]; if (it) choose(it); }
      // Stops here: Escape in an open dropdown dismisses the dropdown, not the lightbox
      // the dropdown lives in.
      else if (e.key === 'Escape') { drop.hidden = true; e.stopPropagation(); }
    });
    return box;
  }

  // renderCharacter shows the active character (charInfo) in the sidebar, or clears
  // it for self-contained models. The no-match chooser is shown separately.
  const renderCharacter = (charInfo) => { if (charInfo) showCharacterPanel(charInfo); else panels.character.replaceChildren(); };

  function showCharacterPanel(charInfo) {
    panels.character.replaceChildren();
    const bones = soloClips ? clipBones(soloClips[0]) : null;
    const panel = document.createElement('div'); panel.className = 'lb-charpanel';
    const label = document.createElement('div'); label.className = 'lb-charpanel-label'; label.textContent = 'Playing on';
    const name = document.createElement('div'); name.className = 'lb-charname';
    name.textContent = charInfo.name.replace(/\.[^.]+$/, ''); name.title = charInfo.name;

    const row = document.createElement('div'); row.className = 'lb-charrow';
    row.appendChild(characterSearch(bones, charInfo));
    // Save-as-default: an icon button beside the search; filled when this is the rig's default.
    const def = document.createElement('button'); def.type = 'button'; def.className = 'lb-savedefault'; def.innerHTML = SAVE_SVG;
    const refreshDef = () => {
      const on = CharRegistry.isPinned(charInfo.id);
      def.classList.toggle('on', on);
      def.title = on
        ? 'Default character for this rig — click to unset'
        : 'Save as the default character for this rig';
    };
    def.addEventListener('click', () => {
      if (CharRegistry.pin(charInfo.id, !CharRegistry.isPinned(charInfo.id))) modelThumbs.reseed();
      refreshDef();
    });
    refreshDef();
    row.appendChild(def);

    panel.append(label, name, row);
    panels.character.appendChild(panel);
  }

  function showCharacterChooser(forBones, note) {
    // panels.character is shared with every other preview, like the canvas and the
    // container: a chooser built by a viewer the user already navigated away from
    // would replace the sidebar of the one on screen.
    if (stopped) return;
    panels.character.replaceChildren();
    const panel = document.createElement('div'); panel.className = 'lb-charpanel';
    const label = document.createElement('div'); label.className = 'lb-charpanel-label'; label.textContent = 'Character';
    const hint = document.createElement('div'); hint.className = 'lb-charhint';
    hint.textContent = note || 'This animation has no mesh — search for a character to play it on:';
    panel.append(label, hint, characterSearch(forBones, null));
    panels.character.appendChild(panel);
  }

  function makeControls() {
    clearOverlays();
    const bar = document.createElement('div'); bar.className = 'lb-controls';
    const play = document.createElement('button'); play.type = 'button'; play.className = 'lb-play'; play.textContent = '⏸';
    const scrub = document.createElement('input'); scrub.type = 'range'; scrub.min = '0'; scrub.max = '1000'; scrub.value = '0'; scrub.className = 'lb-scrub';
    const time = document.createElement('span'); time.className = 'lb-time';
    bar.append(play, scrub, time);
    const sel = document.createElement('select'); sel.className = 'lb-clipsel';
    sel.addEventListener('change', () => playClip(+sel.value));
    const fillClips = (cs) => {
      sel.replaceChildren();
      cs.forEach((c, i) => sel.appendChild(new Option(c.name || 'clip ' + (i + 1), String(i))));
      sel.hidden = cs.length < 2;
    };
    fillClips(clips);
    bar.appendChild(sel);
    const showTime = (t) => {
      time.textContent = `${t.toFixed(2)} / ${clipDur.toFixed(2)}s`;
      if (curTrimmedFrom) { time.textContent += ' ✂'; time.title = `Held-pose tail trimmed (was ${curTrimmedFrom.toFixed(2)}s)`; }
      else time.title = '';
    };
    play.addEventListener('click', () => { playing = !playing; play.textContent = playing ? '⏸' : '▶'; clock.getDelta(); });
    scrub.addEventListener('input', () => {
      if (!action) return;
      playing = false; play.textContent = '▶';
      const t = (+scrub.value / 1000) * clipDur;
      action.paused = false; action.time = t; mixer.update(0);
      showTime(t);
    });
    container.appendChild(bar);
    return {
      sync(t) { if (document.activeElement !== scrub) scrub.value = String(clipDur ? (t / clipDur) * 1000 : 0); showTime(t); },
      setClip(i) { sel.value = String(i); },
      setClips: fillClips,
    };
  }

  (async () => {
    let root;
    if (asset.thumb === 'sidekick') {
      root = await loadSidekick(asset.source && asset.source.parts);
      if (!root) { showPlaceholder('Could not assemble this character.'); return; }
    } else {
      try { root = await loadModel(contentURL(asset.id), asset.ext); }
      catch { showPlaceholder('Could not load this model.'); return; }
    }
    if (stopped) { dispose(root); return; }
    const cs = clipsForAsset(root.animations, asset);
    if (isRenderable(root)) {
      obj = root; scene.add(root);
      // A sidekick group is many part meshes each on its own copy of the skeleton;
      // registering it as a rig would pollute the registry with duplicate bone names.
      if (asset.thumb !== 'sidekick') {
        const entry = rigEntry(asset, root);
        if (entry && CharRegistry.add(entry)) modelThumbs.reseed();
      }
      // buildPlayback corrects orientation and frames from the reference box; do the same
      // for a static (clip-less) renderable so a Z-up model still stands upright and framed.
      if (cs.length) {
        const rmCs = await loadRMClips(asset); // travel sibling, if this animation ships one
        if (stopped) return;
        buildPlayback(root, cs, null, null, rmCs);
      } else frameBox(prepareClipRig(root, null), camera, controls);
      return;
    }
    if (!cs.length) { dispose(root); showPlaceholder('No mesh to preview (data file).'); return; }
    // clip-only: play on a rig it matches (AnimationClips survive disposing the source).
    soloClips = cs;
    soloRootRest = captureRootRest(root); // the clip file's root axis, for uprightRig
    const bones = clipBones(cs[0]);
    dispose(root);
    // Play on the best-matching rig, falling through to the manual picker rather than
    // to a blank viewer when the registry and vendor discovery both come up empty.
    if (await resolveRig(bones, asset, useCharacter, () => stopped)) return;
    if (stopped) return;
    showPlaceholder('Animation clip — pick a character in the sidebar →');
    showCharacterChooser(bones);
  })().catch((e) => {
    // Anything thrown past the handled load failures — a malformed clip, a rig that
    // cannot be prepared — would otherwise be an unhandled rejection that leaves the
    // viewer blank with nothing said. showPlaceholder is a no-op once this viewer has
    // been torn down.
    console.error('preview failed', e);
    showPlaceholder('Could not display this asset.');
  });

  const onResize = () => {
    const nw = container.clientWidth, nh = container.clientHeight;
    if (nw && nh) { renderer.setSize(nw, nh); camera.aspect = nw / nh; camera.updateProjectionMatrix(); }
  };
  // A ResizeObserver (not a one-shot clientWidth read) so the canvas fills the viewer
  // whether it was created while the lightbox was still hidden (first open) or already
  // visible (navigating) — otherwise the initial size flip-flops between the two.
  const ro = new ResizeObserver(onResize);
  ro.observe(container);
  const loop = () => {
    if (stopped || !renderer.domElement.isConnected) { raf = 0; return; }
    raf = requestAnimationFrame(loop);
    if (mixer) { const dt = clock.getDelta(); if (playing) { mixer.update(dt); if (action && ctrls) ctrls.sync(action.time); } }
    controls.update();
    renderer.render(scene, camera);
    // orientation gizmo, top-left corner (in a row with the view toolbar)
    renderer.getSize(viewSize);
    renderer.autoClear = false;
    renderer.clearDepth();
    const g = 52, gx = 10, gy = viewSize.y - g - 8;
    renderer.setViewport(gx, gy, g, g);
    renderer.setScissor(gx, gy, g, g);
    renderer.setScissorTest(true);
    gizmoCam.position.copy(camera.position).sub(controls.target).normalize().multiplyScalar(3);
    gizmoCam.lookAt(0, 0, 0);
    renderer.render(gizmoScene, gizmoCam);
    renderer.setScissorTest(false);
    renderer.setViewport(0, 0, viewSize.x, viewSize.y);
    renderer.autoClear = true;
  };
  loop();

  return {
    stop() {
      stopped = true;
      cancelAnimationFrame(raf);
      ro.disconnect();
      controls.dispose();
      if (mixer) mixer.stopAllAction();
      if (obj) dispose(obj);
      if (ground) { ground.geometry.dispose(); ground.material.dispose(); }
      if (shadowPlane) { shadowPlane.geometry.dispose(); shadowPlane.material.dispose(); }
      gizmoScene.traverse((o) => { o.geometry?.dispose?.(); o.material?.dispose?.(); });
      // The light's 2048² depth target is per-viewer and is not reachable from the
      // scene graph walk above, so it has to be released by name.
      dir.shadow.dispose();
      // The renderer outlives the viewer; only its canvas leaves the DOM. Detaching
      // the scene is what actually frees this preview's GPU memory.
      scene.clear();
      renderer.domElement.remove();
    },
  };
}
