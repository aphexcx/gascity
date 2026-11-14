package beads

import "context"

// QueuedListReader can cancel a List waiting to start. Once a read starts, the
// provider retains its normal retry budget and single-flight ownership; caller
// cancellation must not release that ownership while a statement may still run.
type QueuedListReader interface {
	ListWithQueueContext(context.Context, ListQuery) ([]Bead, error)
}

// ListWithQueueContext forwards cancellation through a reader's optional queue
// capability. Readers without that capability retain their normal List behavior.
func ListWithQueueContext(ctx context.Context, reader interface {
	List(ListQuery) ([]Bead, error)
}, query ListQuery,
) ([]Bead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queued, ok := reader.(QueuedListReader); ok {
		return queued.ListWithQueueContext(ctx, query)
	}
	return reader.List(query)
}

// WithListQueueContext binds cancellation to queued list reads for a scoped
// operation. Other operations and providers without a read queue keep their
// existing behavior. Explicit cached/live handles retain their read semantics.
func WithListQueueContext(ctx context.Context, store Store) Store {
	if store == nil {
		return nil
	}
	return queuedListStore{Store: store, ctx: ctx}
}

type queuedListStore struct {
	Store
	ctx context.Context
}

func (s queuedListStore) List(q ListQuery) ([]Bead, error) {
	return ListWithQueueContext(s.ctx, s.Store, q)
}
func (s queuedListStore) BoundsListPages(q ListQuery) bool { return ReaderBoundsListPages(s.Store, q) }
func (s queuedListStore) Handles() StoreHandles {
	handles := HandlesFor(s.Store)
	handles.Live = queuedLiveReader{LiveReader: handles.Live, ctx: s.ctx}
	return handles
}

type queuedLiveReader struct {
	LiveReader
	ctx context.Context
}

func (r queuedLiveReader) List(q ListQuery) ([]Bead, error) {
	return ListWithQueueContext(r.ctx, r.LiveReader, q)
}

func (r queuedLiveReader) BoundsListPages(q ListQuery) bool {
	return ReaderBoundsListPages(r.LiveReader, q)
}

func (r logicalLiveStoreReader) ListWithQueueContext(ctx context.Context, q ListQuery) ([]Bead, error) {
	q.Live, q.TierMode = true, TierBoth
	return ListWithQueueContext(ctx, r.store, q)
}

func (r liveStoreReader) ListWithQueueContext(ctx context.Context, q ListQuery) ([]Bead, error) {
	q.Live, q.TierMode = true, TierBoth
	return ListWithQueueContext(ctx, r.store.backing, q)
}
