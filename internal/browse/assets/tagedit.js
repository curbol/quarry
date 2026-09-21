// nextTags is what one tag edit does to one card's displayed tags. It is separated
// from the repaint plumbing so the decision can be checked without a DOM.
//
// A card's tags are the union over its fingerprints, and that asymmetry is the whole
// rule: gaining the tag is certain as soon as any of the card's fingerprints is in the
// edit, but losing it is only certain when every one of them was — otherwise another
// copy folded into the same card may still carry it, and dropping it would blank a tag
// that is still true.
//
// Getting this wrong leaves the grid showing tags that are not real, which nothing
// reports and nobody eyeballs across 150k cards.
//
// The caller has already established that this card shares a fingerprint with the
// edit; that is what selects the cards to fold it into.
export function nextTags({ cardFingerprints, cardTags, edited, tag, on }) {
  const tags = new Set(cardTags || []);
  if (on) {
    tags.add(tag);
  } else if (cardFingerprints && cardFingerprints.length &&
             cardFingerprints.every((f) => (edited || []).includes(f))) {
    // The length test is not redundant: every() on an empty list is true, so a card with
    // no fingerprints would have the tag taken off it by an edit that cannot have
    // reached it.
    tags.delete(tag);
  }
  return [...tags].sort();
}

// foldTagEdit is the other half of the same decision: nextTags says what an edit does
// to one card, this says which cards it reaches and how often. Pure over two
// fingerprint indexes — the result set, and the cards currently drawn — so it can be
// checked without a DOM.
//
// Folding over the result set rather than over the drawn cards is what reaches an entry
// whose card the grid window has recycled out, or never built: two byte-identical files
// more than a window apart, edit one, and without this the other keeps the tags its page
// arrived with until the query is re-run.
//
// Each side is visited exactly once. An asset carrying several of the edited
// fingerprints appears under each of them, and nextTags is not idempotent to apply
// twice in the way that matters: the second pass sees the tags the first wrote, so a
// removal that needed *every* fingerprint of the card would be decided against a set
// that has already changed.
export function foldTagEdit({ holders, watchers, fingerprints, tag, on }) {
  const folded = new Set();
  for (const fp of fingerprints) {
    for (const a of holders.get(fp) || []) {
      if (folded.has(a)) continue;
      folded.add(a);
      a.tags = nextTags({
        cardFingerprints: a.fingerprints,
        cardTags: a.tags,
        edited: fingerprints,
        tag,
        on,
      });
    }
  }
  const repaint = [];
  const seen = new Set();
  for (const fp of fingerprints) {
    for (const e of watchers.get(fp) || []) {
      if (seen.has(e)) continue;
      seen.add(e);
      repaint.push(e);
    }
  }
  return repaint;
}
