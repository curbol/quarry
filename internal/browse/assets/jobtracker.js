// JobTracker decides whether a thumbnail job the worker is holding is still the one
// the page wants. Separated from the worker's message plumbing so the decision can be
// checked without an OffscreenCanvas or a GL context.
//
// The worker renders on a single shared canvas, so jobs are serialized: a request can
// sit in the queue while the page changes its mind about the card that asked for it.
// Deciding by asset id alone cannot express that, because the same id is legitimately
// requested again — a grid rebuild cancels every pending id and the observer that
// replaces it immediately re-requests whatever is still on screen. Under an id-keyed
// cancel set, that re-request cleared the cancel for the job already queued, so both
// ran; the second result then landed on a cache entry the first had already filled,
// displacing an object URL the visible image was still using with nothing left to
// revoke it.
//
// A per-request sequence number makes "the same asset, asked for again" distinct from
// "this exact request", which is the distinction the queue needs.
export class JobTracker {
  constructor() {
    // id -> the sequence number of the newest request for it. One entry per asset
    // currently wanted, rather than one per id ever asked for: the worker retires a job
    // when it posts its result, and the page cancels the ones it stops wanting first.
    this.wanted = new Map();
  }

  // note records a new request, superseding any earlier one for the same asset.
  note(id, seq) {
    this.wanted.set(id, seq);
  }

  // cancel drops the asset entirely, so every job already holding one of its sequence
  // numbers becomes stale.
  cancel(id) {
    this.wanted.delete(id);
  }

  // isCurrent reports whether this exact request is still the newest for its asset.
  isCurrent(id, seq) {
    return this.wanted.get(id) === seq;
  }

  // size is how many assets are currently wanted. Exposed so a test can show the map
  // does not grow with every cancelled id.
  get size() {
    return this.wanted.size;
  }
}
