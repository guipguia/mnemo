package store

import (
	"context"
	"database/sql"
	"errors"
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

// SetSession makes session the cluster's current agent run: batches from any other run are
// stale from now on (see Fresh). An agent calls it once, when it starts.
func (w *Writer) SetSession(ctx context.Context, cluster, session string) error {
	_, err := w.tx.ExecContext(ctx, `INSERT INTO streams (cluster, stream, session, seq) VALUES (?, '', ?, 0)
		ON CONFLICT (cluster, stream) DO UPDATE SET session = excluded.session`, cluster, session)
	return err
}

// Fresh reports whether batch seq of stream, from agent run session, comes after everything
// stored for that stream, and if so records it. A stale batch (an older one, or a resend of
// one already stored) must be dropped: applying it would store old versions as new ones and
// move checkpoints back. A batch from an agent run that has since been replaced fails with
// ErrSuperseded, so that agent learns it and stops, rather than silently writing nothing. It runs in the
// batch's own transaction, so checking and storing are atomic. Batches without a session
// (agents from before sessions) or a stream are always fresh.
func (w *Writer) Fresh(ctx context.Context, cluster, session, stream string, seq int64) (bool, error) {
	if session == "" {
		return true, nil
	}
	var current string
	err := w.tx.QueryRowContext(ctx, "SELECT session FROM streams WHERE cluster = ? AND stream = ''", cluster).Scan(&current)
	if err == nil && current != session {
		return false, ErrSuperseded
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if stream == "" {
		return true, nil
	}
	var last int64
	var lastSession string
	err = w.tx.QueryRowContext(ctx, "SELECT session, seq FROM streams WHERE cluster = ? AND stream = ?", cluster, stream).Scan(&lastSession, &last)
	if err == nil && lastSession == session && seq <= last {
		return false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = w.tx.ExecContext(ctx, `INSERT INTO streams (cluster, stream, session, seq) VALUES (?, ?, ?, ?)
		ON CONFLICT (cluster, stream) DO UPDATE SET session = excluded.session, seq = excluded.seq`, cluster, stream, session, seq)
	return err == nil, err
}

// HasVersion reports whether r's version at resourceVersion rv is already stored.
func (w *Writer) HasVersion(ctx context.Context, r Ref, rv string) (bool, error) {
	var one int
	err := w.tx.QueryRowContext(ctx, `SELECT 1 FROM changes WHERE cluster = ? AND resource_version = ?
		AND api_group = ? AND kind = ? AND namespace = ? AND name = ? AND type != ? LIMIT 1`,
		r.Cluster, rv, r.Group, r.Kind, r.Namespace, r.Name, string(Deleted)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
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
