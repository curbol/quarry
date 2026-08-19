package browse

import (
	"sort"
	"strconv"
	"strings"

	"github.com/curbol/quarry/internal/assetindex"
)

// Cards are what the grid shows: one per distinct asset, with byte-identical copies
// folded into it. This file is the shaping — index Assets in, sorted and grouped
// assetDTOs and the facet counts beside them out — with no HTTP anywhere in it, so
// grouping and counting can be read and reasoned about apart from request handling.
//
// groupKey is the single notion of "one card", used both per query and once at
// startup for the facets. Counting cards one way and filtering them another is what
// made a facet advertise a count no filter could reach.

// assetDTO is the client-facing view of an asset: the representative's display
// fields plus every identical copy (same file across variants/packs) so the UI can
// show one card and expose all paths. Count/Copies are 1/[self] when ungrouped.
type assetDTO struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	RelPath  string            `json:"relPath"`
	CopyPath string            `json:"copyPath"`
	Category string            `json:"category"`
	Ext      string            `json:"ext"`
	Vendor   string            `json:"vendor"`
	Pack     string            `json:"pack"`
	Variant  string            `json:"variant"`
	Size     int64             `json:"size"`
	Width    int               `json:"width,omitempty"`
	Height   int               `json:"height,omitempty"`
	Thumb    string            `json:"thumb"`
	Source   assetindex.Source `json:"source"`
	Count    int               `json:"count"`
	Copies   []copyDTO         `json:"copies"`
	// Fingerprints are the group's distinct content identities; tag operations on
	// the card target this whole set. Tags is the union of tag ids over them.
	Fingerprints []string `json:"fingerprints"`
	Tags         []string `json:"tags"`
	// Related are the content fingerprints of assets linked to this card (its
	// companions), for API consumers and the lightbox "parts of this set" strip.
	Related []string `json:"related,omitempty"`
	// RootMotionID is the id of this animation's root-motion (travel) sibling, when
	// it ships one; the lightbox's toggle loads that file to show the travel.
	RootMotionID string `json:"rootMotionId,omitempty"`
	// BakedMotion marks a card whose own file carries baked root motion with no
	// in-place sibling to pair; the lightbox strips it algorithmically instead.
	BakedMotion bool `json:"bakedMotion,omitempty"`
}

// copyDTO is one occurrence of an asset (its variant/pack, the path to copy, and its
// structured source locator so a consumer can resolve it without parsing copyPath).
type copyDTO struct {
	ID          string            `json:"id"`
	Variant     string            `json:"variant"`
	Vendor      string            `json:"vendor"`
	Pack        string            `json:"pack"`
	CopyPath    string            `json:"copyPath"`
	Source      assetindex.Source `json:"source"`
	Fingerprint string            `json:"fingerprint"`
}

func copyOf(a assetindex.Asset) copyDTO {
	return copyDTO{ID: a.ID, Variant: a.Variant, Vendor: a.Vendor, Pack: a.Pack, CopyPath: a.CopyPath, Source: a.Source, Fingerprint: a.Fingerprint}
}

func toDTO(a assetindex.Asset) assetDTO {
	d := assetDTO{
		ID: a.ID, Name: a.Name, RelPath: a.RelPath, CopyPath: a.CopyPath,
		Category: string(a.Category), Ext: a.Ext, Vendor: a.Vendor, Pack: a.Pack,
		Variant: a.Variant, Size: a.Size, Width: a.Width, Height: a.Height,
		Thumb: string(a.Thumb), Source: a.Source,
		Count: 1, Copies: []copyDTO{copyOf(a)},
	}
	if a.Fingerprint != "" {
		d.Fingerprints = []string{a.Fingerprint}
	}
	return d
}

// thumbRank ranks thumbnail kinds so a group picks the copy with the best preview
// (a Unity copy with a preview.png beats a SourceFiles copy with only geometry).
func thumbRank(t assetindex.ThumbKind) int {
	switch t {
	case assetindex.ThumbImage, assetindex.ThumbPreview:
		return 4
	case assetindex.ThumbGLB:
		return 3
	case assetindex.ThumbFBX:
		return 2
	}
	return 0
}

type facets struct {
	Categories []facetValue `json:"categories"`
	Vendors    []facetValue `json:"vendors"`
	Variants   []facetValue `json:"variants"`
}

type facetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// buildFacets counts the distinct facet values for the filter UI over what a query
// can actually return. Two things make that different from counting every asset.
// Root-motion siblings are suppressed from results, so counting them leaves a number
// no combination of filters could reach. And results are cards, not assets: a pack
// shipping one file in both a zip and a unitypackage produces two assets and one
// card, so counting assets would advertise a vendor total roughly double what
// clicking that vendor returns.
//
// A card contributes once per distinct value across its copies, which is exactly
// reachable — every facet filter runs over assets, before grouping, so a card
// survives if any of its copies carries the value. Category is no exception: it is
// derived from a file's path, so one card's copies can classify differently (the
// same sprite under Source_Sprites/ in a zip and under Textures/ in a unitypackage),
// and counting only the representative's would advertise a zero that ?type= returns
// results for.
//
// Grouped over positions rather than through groupItems: this runs over the whole
// library, and materializing every card as a DTO — with its copies slice and its
// fingerprint set — to throw them all away is a large transient spike at startup.
func buildFacets(assets []assetindex.Asset, hidden map[string]bool) facets {
	byKey := map[string][]int32{}
	var order []string
	for i := range assets {
		if hidden[assets[i].ID] {
			continue
		}
		k := groupKey(assets[i])
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], int32(i))
	}
	categories, vendors, variants := map[string]int{}, map[string]int{}, map[string]int{}
	bump := func(m map[string]int, seen map[string]bool, v string) {
		if !seen[v] {
			seen[v] = true
			m[v]++
		}
	}
	for _, k := range order {
		cs, vs, vrs := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, i := range byKey[k] {
			a := &assets[i]
			bump(categories, cs, string(a.Category))
			bump(vendors, vs, a.Vendor)
			bump(variants, vrs, a.Variant)
		}
	}
	return facets{
		Categories: sortedFacet(categories),
		Vendors:    sortedFacet(vendors),
		Variants:   sortedFacet(variants),
	}
}

// sortedFacet turns a value->count map into a slice sorted alphabetically by value
// (case-insensitive), so a specific option is easy to find in the filter UI.
func sortedFacet(m map[string]int) []facetValue {
	out := make([]facetValue, 0, len(m))
	for v, c := range m {
		out = append(out, facetValue{Value: v, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Value), strings.ToLower(out[j].Value)
		if li != lj {
			return li < lj
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// sortItems orders results: "path" keeps assets grouped by their location
// (vendor/pack/folder), "size" puts the heaviest first — which is how a rig search
// finds the one mesh among a pack of clips; the default is case-insensitive by name.
func sortItems(items []assetDTO, mode string) {
	switch mode {
	case "path":
		sort.Slice(items, func(i, j int) bool { return items[i].RelPath < items[j].RelPath })
	case "size":
		sort.Slice(items, func(i, j int) bool {
			if items[i].Size != items[j].Size {
				return items[i].Size > items[j].Size
			}
			return items[i].RelPath < items[j].RelPath
		})
	default:
		sort.Slice(items, func(i, j int) bool {
			ni, nj := strings.ToLower(items[i].Name), strings.ToLower(items[j].Name)
			if ni != nj {
				return ni < nj
			}
			return items[i].RelPath < items[j].RelPath
		})
	}
}

// groupNameKey folds file names that differ only by separators/case, so the same
// file collapses even when a variant renamed it. Synty's Unity export inserts an
// underscore before a trailing number (SPR_..._Gem09.png -> ..._Gem_09.png), which
// otherwise leaves the identical sprite showing as two cards. Pairing this with the
// byte size in the group key keeps genuinely different files apart.
// groupKey is the identity a result card is grouped on: same normalized name, same
// size. Shared with the facet counts so the two cannot disagree about what one card
// is — a divergence there advertises a total no filter can reach.
func groupKey(a assetindex.Asset) string {
	return groupNameKey(a.Name) + "\x00" + strconv.FormatInt(a.Size, 10)
}

func groupNameKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// groupItems collapses assets that are the same file (normalized name + size) into
// one DTO, keeping first-seen order, choosing the best-thumbnail copy as the
// representative, and listing every copy. It takes positions into assets rather than
// a slice of them, so grouping a library-sized result set does not copy it again.
func groupItems(assets []assetindex.Asset, sel []int32) []assetDTO {
	type group struct {
		rep    int32
		copies []int32
	}
	byKey := map[string]*group{}
	var order []string
	for _, ai := range sel {
		a := &assets[ai]
		key := groupKey(*a)
		g := byKey[key]
		if g == nil {
			g = &group{rep: ai}
			byKey[key] = g
			order = append(order, key)
		} else if thumbRank(a.Thumb) > thumbRank(assets[g.rep].Thumb) {
			g.rep = ai
		}
		g.copies = append(g.copies, ai)
	}
	out := make([]assetDTO, 0, len(order))
	for _, key := range order {
		g := byKey[key]
		d := toDTO(assets[g.rep])
		d.Count = len(g.copies)
		d.Copies = make([]copyDTO, len(g.copies))
		fps := map[string]bool{}
		for i, ci := range g.copies {
			c := &assets[ci]
			d.Copies[i] = copyOf(*c)
			if c.Fingerprint != "" {
				fps[c.Fingerprint] = true
			}
		}
		d.Fingerprints = sortedSet(fps)
		out = append(out, d)
	}
	return out
}

// sortedSet returns the set's keys sorted.
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
