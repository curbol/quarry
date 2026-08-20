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
