// The category glyphs the grid and the lightbox both fall back to when an asset has no
// preview of its own. Their own module because the viewer needs them too, and otherwise
// the viewer would have to import from the page that owns it.

export const ICONS = {
  model: '<path d="M12 2 3 7v10l9 5 9-5V7z"/><path d="M3 7l9 5 9-5M12 12v10"/>',
  image: '<rect x="3" y="4" width="18" height="16" rx="2"/><circle cx="8.5" cy="9.5" r="1.8"/><path d="M4 18l5-5 4 3 3-2 4 4"/>',
  ui: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M9 9v11"/>',
  texture: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M3 15h18M9 3v18M15 3v18"/>',
  material: '<circle cx="12" cy="12" r="9"/><path d="M4 12a8 8 0 0 1 16 0"/>',
  data: '<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  scene: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 9h18"/>',
  animation: '<circle cx="12" cy="12" r="9"/><path d="M10 8l6 4-6 4z"/>',
  audio: '<path d="M4 9v6h4l5 4V5L8 9z"/><path d="M16 8a5 5 0 0 1 0 8"/>',
  script: '<path d="M8 4h9l3 3v13H8z"/><path d="M4 8v12h11"/>',
  doc: '<path d="M6 2h8l4 4v16H6z"/><path d="M14 2v4h4"/>',
  font: '<path d="M5 20 12 4l7 16"/><path d="M8.3 14h7.4"/>',
  other: '<circle cx="12" cy="12" r="9"/><path d="M12 8v4l3 2"/>',
};

// Each glyph is parsed once and cloned per use. The grid draws one icon per card and
// rebuilds its live window as the user scrolls, so building the markup and handing it
// to the HTML parser per call runs the parser thousands of times over a long scroll.
const parsed = new Map();

export function iconEl(category) {
  const key = ICONS[category] ? category : 'other';
  let proto = parsed.get(key);
  if (!proto) {
    const wrap = document.createElement('div');
    wrap.innerHTML = `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round">${ICONS[key]}</svg>`;
    proto = wrap.firstElementChild;
    parsed.set(key, proto);
  }
  return proto.cloneNode(true);
}
