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

// Stored reports whether r already has a version recorded at or after t, or one at
// resourceVersion rv: a backfill's write the hub has, or one older than what it has.
func (w *Writer) Stored(ctx context.Context, r Ref, rv string, t time.Time) (bool, error) {
	var one int
	err := w.tx.QueryRowContext(ctx, `SELECT 1 FROM changes WHERE cluster = ? AND api_group = ? AND kind = ? AND namespace = ? AND name = ?
		AND (ts >= ? OR (? != '' AND resource_version = ? AND type != ?)) LIMIT 1`,
		r.Cluster, r.Group, r.Kind, r.Namespace, r.Name, t.UnixNano(), rv, rv, string(Deleted)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// HeldAudit is who made a write, kept until the change it names is recorded.
type HeldAudit struct {
	Namespace, Name, ResourceVersion, UID, User, UserAgent string
}

// heldFor is how long an audit event waits for its change before it's dropped.
const heldFor = 10 * time.Minute

// HoldAudit keeps a for when its change arrives (see HeldAudits), and drops the cluster's
// held audits older than heldFor.
func (w *Writer) HoldAudit(ctx context.Context, cluster string, a HeldAudit) error {
	now := time.Now()
	if _, err := w.tx.ExecContext(ctx, "DELETE FROM held_audits WHERE cluster = ? AND ts < ?",
		cluster, now.Add(-heldFor).UnixNano()); err != nil {
		return err
	}
	_, err := w.tx.ExecContext(ctx, `INSERT INTO held_audits
		(cluster, namespace, name, resource_version, uid, author, user_agent, ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		cluster, a.Namespace, a.Name, a.ResourceVersion, a.UID, a.User, a.UserAgent, now.UnixNano())
	return err
}

// HasHeldAudits reports whether cluster has any, so ingest can skip looking per change.
func (w *Writer) HasHeldAudits(ctx context.Context, cluster string) (bool, error) {
	var one int
	err := w.tx.QueryRowContext(ctx, "SELECT 1 FROM held_audits WHERE cluster = ? LIMIT 1", cluster).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// TakeHeldAudits removes and returns the held audits for c: its resourceVersion, or for a
// deletion, its UID.
func (w *Writer) TakeHeldAudits(ctx context.Context, c *Change) ([]HeldAudit, error) {
	where := `cluster = ? AND namespace = ? AND name = ? AND resource_version = ? AND resource_version != ''`
	args := []any{c.Cluster, c.Namespace, c.Name, c.ResourceVersion}
	if c.Type == Deleted {
		where = `cluster = ? AND namespace = ? AND name = ? AND uid = ? AND uid != ''`
		args = []any{c.Cluster, c.Namespace, c.Name, c.UID}
	}
	rows, err := w.tx.QueryContext(ctx, "SELECT namespace, name, resource_version, uid, author, user_agent FROM held_audits WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	var out []HeldAudit
	for rows.Next() {
		var a HeldAudit
		if err := rows.Scan(&a.Namespace, &a.Name, &a.ResourceVersion, &a.UID, &a.User, &a.UserAgent); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	_, err = w.tx.ExecContext(ctx, "DELETE FROM held_audits WHERE "+where, args...)
	return out, err
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
