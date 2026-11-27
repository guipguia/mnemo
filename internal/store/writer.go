package store

import (
	"context"
	"time"
)

// Writer makes changes inside one transaction (see Store.Write).
type Writer struct{ tx *tx }

// Write runs f in one transaction: all of its changes are stored, or none. On PostgreSQL it
// holds the write lock throughout (see db.BeginTx).
func (s *Store) Write(ctx context.Context, f func(*Writer) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(&Writer{tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// The Store methods below each write in a transaction of their own.

func (s *Store) Record(ctx context.Context, c *Change, listed bool, attribute func(*Change)) (ok bool, err error) {
	err = s.Write(ctx, func(w *Writer) (err error) { ok, err = w.Record(ctx, c, listed, attribute); return })
	return
}

func (s *Store) Reattribute(ctx context.Context, cluster, namespace, name, rv, uid string, f func(*Change)) (n int, err error) {
	err = s.Write(ctx, func(w *Writer) (err error) { n, err = w.Reattribute(ctx, cluster, namespace, name, rv, uid, f); return })
	return
}

func (s *Store) Prune(ctx context.Context, cluster, group, kind string, keep map[string]bool, t time.Time) (n int, err error) {
	err = s.Write(ctx, func(w *Writer) (err error) { n, err = w.Prune(ctx, cluster, group, kind, keep, t); return })
	return
}

func (s *Store) RegisterCluster(ctx context.Context, name, uid string) error {
	return s.Write(ctx, func(w *Writer) error { return w.RegisterCluster(ctx, name, uid) })
}

func (s *Store) SetCheckpoint(ctx context.Context, cluster, resource, rv string) error {
	return s.Write(ctx, func(w *Writer) error { return w.SetCheckpoint(ctx, cluster, resource, rv) })
}

func (s *Store) RecordEvents(ctx context.Context, cluster string, evs []Event) error {
	return s.Write(ctx, func(w *Writer) error { return w.RecordEvents(ctx, cluster, evs) })
}

func (s *Store) RecordDeploys(ctx context.Context, cluster string, ds []Deploy) error {
	return s.Write(ctx, func(w *Writer) error { return w.RecordDeploys(ctx, cluster, ds) })
}
