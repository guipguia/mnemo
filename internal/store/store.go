// Package store persists object history for the hub.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ponytail: one schema applied with IF NOT EXISTS; add versioned migrations on the first schema change.
const schema = `
CREATE TABLE IF NOT EXISTS changes (
	id               INTEGER PRIMARY KEY,
	cluster          TEXT    NOT NULL,
	api_group        TEXT    NOT NULL,
	version          TEXT    NOT NULL,
	kind             TEXT    NOT NULL,
	namespace        TEXT    NOT NULL,
	name             TEXT    NOT NULL,
	uid              TEXT    NOT NULL,
	resource_version TEXT    NOT NULL,
	ts               INTEGER NOT NULL,
	type             TEXT    NOT NULL,
	author           TEXT    NOT NULL,
	source           TEXT    NOT NULL,
	paths            TEXT,
	object           BLOB
);
CREATE INDEX IF NOT EXISTS changes_cluster_ts   ON changes (cluster, ts);
CREATE INDEX IF NOT EXISTS changes_namespace_ts ON changes (cluster, namespace, ts);
CREATE INDEX IF NOT EXISTS changes_object       ON changes (cluster, api_group, kind, namespace, name, ts);
`

const cols = `id, cluster, api_group, version, kind, namespace, name, uid, resource_version, ts, type, author, source, paths`

// ErrNotFound means the object did not exist at the requested time.
var ErrNotFound = errors.New("not found")

type Type string

const (
	Baseline Type = "baseline" // first seen during a list: existed before mnemo was watching
	Added    Type = "added"
	Modified Type = "modified"
	Deleted  Type = "deleted"
)

// Ref identifies an object by name, not UID: a delete and re-create is one history.
type Ref struct {
	Cluster, Group, Kind, Namespace, Name string
}

type Change struct {
	ID int64
	Ref
	Version, UID, ResourceVersion string
	Time                          time.Time
	Type                          Type
	Author                        string          // field manager, or user when audit attribution is available
	Source                        string          // gitops | controller | manual | unknown
	Paths                         []string        // JSON pointers changed since the previous version (Modified only)
	Object                        json.RawMessage // normalized object; nil when Deleted or not loaded
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// ponytail: one connection serializes reads and writes; fine for one hub, revisit with Postgres/HA.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Record stores c unless it matches the object's latest version, and reports whether it did.
// c.Object is the current normalized object, or nil if the object was deleted.
// listed is true when the object came from a list rather than a watch event.
// Record fills in c.ID, c.Type and c.Paths.
func (s *Store) Record(ctx context.Context, c *Change, listed bool) (bool, error) {
	if c.Cluster == "" || c.Kind == "" || c.Name == "" || c.Time.IsZero() {
		return false, errors.New("record: cluster, kind, name and time are required")
	}
	if c.Object != nil {
		var err error
		if c.Object, err = canonical(c.Object); err != nil {
			return false, fmt.Errorf("record: invalid object: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var prev []byte // nil if never seen or latest version is a deletion
	err = tx.QueryRowContext(ctx, `SELECT object FROM changes
		WHERE cluster = ? AND api_group = ? AND kind = ? AND namespace = ? AND name = ?
		ORDER BY id DESC LIMIT 1`, c.Cluster, c.Group, c.Kind, c.Namespace, c.Name).Scan(&prev)
	seen := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	c.Paths = nil
	switch {
	case c.Object == nil && prev == nil:
		return false, nil // already deleted, or never seen
	case c.Object == nil:
		c.Type = Deleted
	case !seen && listed:
		c.Type = Baseline
	case prev == nil:
		c.Type = Added
	case bytes.Equal(prev, c.Object):
		return false, nil
	default:
		c.Type = Modified
		if c.Paths, err = changedPaths(prev, c.Object); err != nil {
			return false, err
		}
	}

	var paths, object any // stay NULL unless set
	if len(c.Paths) > 0 {
		b, _ := json.Marshal(c.Paths)
		paths = string(b)
	}
	if c.Object != nil {
		object = []byte(c.Object)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO changes (`+cols[len("id, "):]+`, object)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Cluster, c.Group, c.Version, c.Kind, c.Namespace, c.Name, c.UID, c.ResourceVersion,
		c.Time.UnixNano(), string(c.Type), c.Author, c.Source, paths, object)
	if err != nil {
		return false, err
	}
	if c.ID, err = res.LastInsertId(); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Filter narrows the change feed. Empty fields match everything.
type Filter struct {
	Cluster, Namespace, Kind, Author, Source string
	Since, Until                             time.Time // Since inclusive, Until exclusive
	Limit                                    int       // default 100, max 1000
}

// Changes is the change feed, newest first, without object bodies.
func (s *Store) Changes(ctx context.Context, f Filter) ([]Change, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	for col, v := range map[string]string{
		"cluster": f.Cluster, "namespace": f.Namespace, "kind": f.Kind, "author": f.Author, "source": f.Source,
	} {
		if v != "" {
			add(col+" = ?", v)
		}
	}
	if !f.Since.IsZero() {
		add("ts >= ?", f.Since.UnixNano())
	}
	if !f.Until.IsZero() {
		add("ts < ?", f.Until.UnixNano())
	}
	q := "SELECT " + cols + " FROM changes"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	return s.list(ctx, q+" ORDER BY ts DESC, id DESC LIMIT ?", append(args, min(limit, 1000))...)
}

// History lists an object's versions, newest first, without object bodies.
func (s *Store) History(ctx context.Context, r Ref, limit int) ([]Change, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.list(ctx, "SELECT "+cols+` FROM changes
		WHERE cluster = ? AND api_group = ? AND kind = ? AND namespace = ? AND name = ?
		ORDER BY ts DESC, id DESC LIMIT ?`,
		r.Cluster, r.Group, r.Kind, r.Namespace, r.Name, min(limit, 1000))
}

// Get returns one change with its object body.
func (s *Store) Get(ctx context.Context, id int64) (Change, error) {
	c, err := scan(s.db.QueryRowContext(ctx, "SELECT "+cols+", object FROM changes WHERE id = ?", id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// At returns the object as it was at t, or ErrNotFound if it did not exist then.
func (s *Store) At(ctx context.Context, r Ref, t time.Time) (Change, error) {
	c, err := scan(s.db.QueryRowContext(ctx, "SELECT "+cols+`, object FROM changes
		WHERE cluster = ? AND api_group = ? AND kind = ? AND namespace = ? AND name = ? AND ts <= ?
		ORDER BY ts DESC, id DESC LIMIT 1`,
		r.Cluster, r.Group, r.Kind, r.Namespace, r.Name, t.UnixNano()), true)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && c.Type == Deleted) {
		return Change{}, ErrNotFound
	}
	return c, err
}

func (s *Store) list(ctx context.Context, q string, args ...any) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scan(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scan(row interface{ Scan(...any) error }, withObject bool) (Change, error) {
	var c Change
	var ts int64
	var paths sql.NullString
	var object []byte
	dest := []any{&c.ID, &c.Cluster, &c.Group, &c.Version, &c.Kind, &c.Namespace, &c.Name, &c.UID,
		&c.ResourceVersion, &ts, &c.Type, &c.Author, &c.Source, &paths}
	if withObject {
		dest = append(dest, &object)
	}
	if err := row.Scan(dest...); err != nil {
		return c, err
	}
	c.Time = time.Unix(0, ts).UTC()
	c.Object = object
	if paths.Valid {
		if err := json.Unmarshal([]byte(paths.String), &c.Paths); err != nil {
			return c, err
		}
	}
	return c, nil
}

// canonical re-encodes JSON with sorted keys so equal objects compare equal byte-for-byte.
func canonical(b []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// changedPaths lists JSON pointers (RFC 6901) whose values differ between a and b.
func changedPaths(a, b []byte) ([]string, error) {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return nil, err
	}
	var out []string
	diff("", x, y, &out)
	slices.Sort(out)
	return out, nil
}

var pointerEscape = strings.NewReplacer("~", "~0", "/", "~1")

func diff(path string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		for k, v := range am {
			diff(path+"/"+pointerEscape.Replace(k), v, bm[k], out)
		}
		for k, v := range bm {
			if _, ok := am[k]; !ok {
				diff(path+"/"+pointerEscape.Replace(k), nil, v, out)
			}
		}
		return
	}
	// Equal-length arrays diff per element; a length change reports the whole array.
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok && len(al) == len(bl) {
		for i := range al {
			diff(path+"/"+strconv.Itoa(i), al[i], bl[i], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
}
