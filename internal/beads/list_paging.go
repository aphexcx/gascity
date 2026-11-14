package beads

import "errors"

// BoundedListPager is an optional backing-fetch capability. A true result means
// each backing result honors the query's limit and compound seek boundary,
// including exclusion filters. A logical slice limit alone does not qualify.
type BoundedListPager interface {
	BoundsListPages(ListQuery) bool
}

// ReaderBoundsListPages reports whether paging avoids repeated whole reads.
// Unknown readers conservatively use one complete read instead.
func ReaderBoundsListPages(reader interface {
	List(ListQuery) ([]Bead, error)
}, query ListQuery,
) bool {
	pager, ok := reader.(BoundedListPager)
	return ok && pager.BoundsListPages(query)
}

// BoundsListPages reports the native store's exact backing-page support.
func (s *NativeDoltStore) BoundsListPages(query ListQuery) bool {
	return query.Sort == SortCreatedDescIDAsc && query.Limit > 0 && nativeCreatedLimitPushdown(query) == query.Limit
}

// BoundsListPages forwards authoritative reads to the backing capability.
// Cache-only scans do not have bounded backing pagination.
func (c *CachingStore) BoundsListPages(query ListQuery) bool {
	return query.Live && ReaderBoundsListPages(c.backing, query)
}

func (r logicalLiveStoreReader) BoundsListPages(query ListQuery) bool {
	query.Live, query.TierMode = true, TierBoth
	return ReaderBoundsListPages(r.store, query)
}

func (r liveStoreReader) BoundsListPages(query ListQuery) bool {
	query.Live, query.TierMode = true, TierBoth
	return ReaderBoundsListPages(r.store.backing, query)
}

// ListAllPages reads a complete result with exact pages only when the reader
// bounds its backing fetch. Any partial result is an error: safety checks must
// never interpret an omitted row as proof that no live work exists.
func ListAllPages(reader LiveReader, query ListQuery) ([]Bead, error) {
	query.Sort = SortCreatedDescIDAsc
	query.Limit = 1000
	if !ReaderBoundsListPages(reader, query) {
		query.Limit = 0
		return reader.List(query)
	}
	var rows []Bead
	for {
		page, err := reader.List(query)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
		if len(page) < query.Limit {
			return rows, nil
		}
		last := page[len(page)-1]
		next := &SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		if query.SeekAfter != nil && !query.SeekAfter.After(last, query.Sort) {
			return nil, errors.New("list page did not advance")
		}
		query.SeekAfter = next
	}
}
