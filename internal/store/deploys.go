package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Deploy is one sync of a GitOps app (an Argo CD Application or a Flux Kustomization):
// the objects it applied between Start and End came from Revision.
type Deploy struct {
	Kind      string    `json:"kind"` // Application | Kustomization
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Key       string    `json:"key,omitempty"` // Argo CD history ID; empty: append when Revision changes
	Revision  string    `json:"revision"`
	Repo      string    `json:"repo,omitempty"`
	Start     time.Time `json:"start,omitzero"` // zero when the tool doesn't report it
	End       time.Time `json:"end"`
}

// RecordDeploys stores deploys of cluster's apps. A deploy with a Key replaces the one with the
// same Key; without one, it is added only if the app's latest deploy has another revision.
func (s *Store) RecordDeploys(ctx context.Context, cluster string, ds []Deploy) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, d := range ds {
		if d.Key == "" {
			var latest string
			err := tx.QueryRowContext(ctx, `SELECT revision FROM deploys
				WHERE cluster = ? AND kind = ? AND namespace = ? AND name = ? ORDER BY end_ts DESC LIMIT 1`,
				cluster, d.Kind, d.Namespace, d.Name).Scan(&latest)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if latest == d.Revision {
				continue
			}
			d.Key = strconv.FormatInt(d.End.UnixNano(), 10)
		}
		var start int64
		if !d.Start.IsZero() {
			start = d.Start.UnixNano()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deploys
			(cluster, kind, namespace, name, key, revision, repo, start_ts, end_ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (cluster, kind, namespace, name, key) DO UPDATE SET
				revision = excluded.revision, repo = excluded.repo, start_ts = excluded.start_ts, end_ts = excluded.end_ts`,
			cluster, d.Kind, d.Namespace, d.Name, d.Key, d.Revision, d.Repo, start, d.End.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// deploySlack absorbs the 1s precision of managedFields times and agent processing delays.
const deploySlack = 5 * time.Second

// DeployAt returns the deploy of app kind/namespace/name in cluster that applied a change made
// at t: the first one in progress at t, or else the newest one that ended before t, whose
// revision a GitOps tool re-applies when it reverts drift. In progress means started by t
// and ended at most 10 minutes later; without a start (Flux), ended at most 2 minutes later,
// the lag between applying and reporting the revision.
// An empty namespace matches the app in any namespace.
// ponytail: fixed windows; a slower sync (or Flux health checks over 2 minutes) falls back
// to the previous revision.
func (s *Store) DeployAt(ctx context.Context, cluster, kind, namespace, name string, t time.Time) (Deploy, error) {
	q := `SELECT kind, namespace, name, key, revision, repo, start_ts, end_ts FROM deploys
		WHERE cluster = ? AND kind = ? AND name = ? AND (? = '' OR namespace = ?) AND `
	args := []any{cluster, kind, name, namespace, namespace}
	ts, slack := t.UnixNano(), int64(deploySlack)
	d, err := scanDeploy(s.db.QueryRowContext(ctx, q+`end_ts >= ? AND start_ts <= ?
		AND end_ts <= CASE WHEN start_ts = 0 THEN ? ELSE ? END
		ORDER BY end_ts LIMIT 1`, append(args, ts-slack, ts+slack, ts+int64(2*time.Minute), ts+int64(10*time.Minute))...))
	if errors.Is(err, sql.ErrNoRows) {
		d, err = scanDeploy(s.db.QueryRowContext(ctx, q+"end_ts < ? ORDER BY end_ts DESC LIMIT 1", append(args, ts)...))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func scanDeploy(row *sql.Row) (Deploy, error) {
	var d Deploy
	var start, end int64
	err := row.Scan(&d.Kind, &d.Namespace, &d.Name, &d.Key, &d.Revision, &d.Repo, &start, &end)
	if start != 0 {
		d.Start = time.Unix(0, start).UTC()
	}
	d.End = time.Unix(0, end).UTC()
	return d, err
}
