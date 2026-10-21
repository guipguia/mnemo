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
	drift            INTEGER NOT NULL DEFAULT 0,
	paths            TEXT,
	object           BLOB
);
CREATE INDEX IF NOT EXISTS changes_cluster_ts   ON changes (cluster, ts);
CREATE INDEX IF NOT EXISTS changes_namespace_ts ON changes (cluster, namespace, ts);
CREATE INDEX IF NOT EXISTS changes_object       ON changes (cluster, api_group, kind, namespace, name, ts);
CREATE TABLE IF NOT EXISTS clusters (
	name      TEXT PRIMARY KEY,
	uid       TEXT    NOT NULL,
	last_seen INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoints (
	cluster          TEXT NOT NULL,
	resource         TEXT NOT NULL,
	resource_version TEXT NOT NULL,
	PRIMARY KEY (cluster, resource)
);
`

const cols = `id, cluster, api_group, version, kind, namespace, name, uid, resource_version, ts, type, author, source, drift, paths`

var (
	// ErrNotFound means the object did not exist at the requested time.
	ErrNotFound = errors.New("not found")
	// ErrClusterConflict means a cluster name is already registered with a different UID.
	ErrClusterConflict = errors.New("cluster name already registered with a different UID")
)

type Type string

const (
	Baseline Type = "baseline" // first seen during a list: existed before mnemo was watching
	Added    Type = "added"
	Modified Type = "modified"
	Deleted  Type = "deleted"
)

// Ref identifies an object by name, not UID: a delete and re-create is one history.
type Ref struct {
	Cluster   string `json:"cluster"`
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type Change struct {
	ID int64 `json:"id"`
	Ref
	Version         string          `json:"version"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Time            time.Time       `json:"time"`
	Type            Type            `json:"type"`
	Author          string          `json:"author"`           // field manager, or user when audit attribution is available
	Source          string          `json:"source"`           // gitops | controller | manual | unknown
	Drift           bool            `json:"drift"`            // manual change to a GitOps-managed object
	Paths           []string        `json:"paths,omitempty"`  // JSON pointers changed since the previous version (Modified only)
	Object          json.RawMessage `json:"object,omitempty"` // normalized object; nil when Deleted or not loaded
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

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Record stores c unless it matches the object's latest version, and reports whether it did.
// c.Object is the current normalized object, or nil if the object was deleted.
// listed is true when the object came from a list rather than a watch event.
// Record fills in c.ID, c.Type and c.Paths, then calls attribute (if non-nil) so the
// caller can set authorship from the changed paths before the row is written.
func (s *Store) Record(ctx context.Context, c *Change, listed bool, attribute func(*Change)) (bool, error) {
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
	var prevTS int64
	err = tx.QueryRowContext(ctx, `SELECT object, ts FROM changes
		WHERE cluster = ? AND api_group = ? AND kind = ? AND namespace = ? AND name = ?
		ORDER BY id DESC LIMIT 1`, c.Cluster, c.Group, c.Kind, c.Namespace, c.Name).Scan(&prev, &prevTS)
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

	if attribute != nil {
		attribute(c)
	}
	// Corrected times have 1s precision; never let a version sort before the one it follows.
	if seen && c.Time.UnixNano() < prevTS {
		c.Time = time.Unix(0, prevTS).UTC()
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Cluster, c.Group, c.Version, c.Kind, c.Namespace, c.Name, c.UID, c.ResourceVersion,
		c.Time.UnixNano(), string(c.Type), c.Author, c.Source, c.Drift, paths, object)
	if err != nil {
		return false, err
	}
	if c.ID, err = res.LastInsertId(); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Access is what a caller may see: everything, or the union of Scopes. The zero value sees nothing.
type Access struct {
	All    bool
	Scopes []Scope
}

// Scope grants clusters × namespaces. Patterns are a name, "*", or "prefix*".
// Empty Clusters means every cluster. Empty Namespaces means every namespace and
// cluster-scoped objects; with Namespaces set, "*" matches any namespace but never
// cluster-scoped objects (ClusterRoles, ClusterRoleBindings, ...).
type Scope struct {
	Clusters   []string `json:"clusters,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

func (a Access) Allows(cluster, namespace string) bool {
	if a.All {
		return true
	}
	for _, s := range a.Scopes {
		if matchAny(s.Clusters, cluster, false) && matchAny(s.Namespaces, namespace, true) {
			return true
		}
	}
	return false
}

// AllowsCluster reports whether anything in cluster is visible.
func (a Access) AllowsCluster(cluster string) bool {
	if a.All {
		return true
	}
	for _, s := range a.Scopes {
		if matchAny(s.Clusters, cluster, false) {
			return true
		}
	}
	return false
}

func matchAny(patterns []string, v string, namespace bool) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if namespace && v == "" {
			continue // cluster-scoped: only visible without namespace restrictions
		}
		if p == "*" || p == v || (strings.HasSuffix(p, "*") && strings.HasPrefix(v, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

// sql renders Access as a WHERE condition with the same semantics as Allows.
func (a Access) sql() (string, []any) {
	if a.All {
		return "1 = 1", nil
	}
	if len(a.Scopes) == 0 {
		return "1 = 0", nil
	}
	var ors []string
	var args []any
	for _, s := range a.Scopes {
		c, ca := matchSQL("cluster", s.Clusters)
		n, na := matchSQL("namespace", s.Namespaces)
		if len(s.Namespaces) > 0 {
			n = "(namespace != '' AND " + n + ")"
		}
		ors = append(ors, "("+c+" AND "+n+")")
		args = append(append(args, ca...), na...)
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

func matchSQL(col string, patterns []string) (string, []any) {
	if len(patterns) == 0 {
		return "1 = 1", nil
	}
	var ors []string
	var args []any
	for _, p := range patterns {
		switch {
		case p == "*":
			return "1 = 1", nil
		case strings.HasSuffix(p, "*"):
			// substr, not LIKE: case-sensitive and portable to Postgres and MySQL.
			pre := strings.TrimSuffix(p, "*")
			ors = append(ors, "substr("+col+", 1, ?) = ?")
			args = append(args, len(pre), pre)
		default:
			ors = append(ors, col+" = ?")
			args = append(args, p)
		}
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// Filter narrows the change feed. Empty fields match everything.
type Filter struct {
	Access                                   Access // required: the zero value returns nothing
	Cluster, Namespace, Kind, Author, Source string
	DriftOnly                                bool
	Since, Until                             time.Time // Since inclusive, Until exclusive
	Limit                                    int       // default 100, max 1000
}

// Changes is the change feed, newest first, without object bodies.
func (s *Store) Changes(ctx context.Context, f Filter) ([]Change, error) {
	cond, args := f.Access.sql()
	where := []string{cond}
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
	if f.DriftOnly {
		add("drift = ?", true)
	}
	if !f.Since.IsZero() {
		add("ts >= ?", f.Since.UnixNano())
	}
	if !f.Until.IsZero() {
		add("ts < ?", f.Until.UnixNano())
	}
	q := "SELECT " + cols + " FROM changes WHERE " + strings.Join(where, " AND ")
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

// Prune records a deletion for every live object of cluster/group/kind not in keep
// (keyed "namespace/name"). Called after a relist to catch deletions missed while offline.
func (s *Store) Prune(ctx context.Context, cluster, group, kind string, keep map[string]bool, t time.Time) (int, error) {
	// Collect first: with one connection, Record can't run while rows are open.
	rows, err := s.db.QueryContext(ctx, `SELECT namespace, name FROM changes c
		WHERE cluster = ? AND api_group = ? AND kind = ? AND type != ?
		AND id = (SELECT MAX(id) FROM changes
			WHERE cluster = c.cluster AND api_group = c.api_group AND kind = c.kind
			AND namespace = c.namespace AND name = c.name)`, cluster, group, kind, string(Deleted))
	if err != nil {
		return 0, err
	}
	var gone []Ref
	for rows.Next() {
		r := Ref{Cluster: cluster, Group: group, Kind: kind}
		if err := rows.Scan(&r.Namespace, &r.Name); err != nil {
			rows.Close()
			return 0, err
		}
		if !keep[r.Namespace+"/"+r.Name] {
			gone = append(gone, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range gone {
		if _, err := s.Record(ctx, &Change{Ref: r, Time: t, Source: "unknown"}, true, nil); err != nil {
			return 0, err
		}
	}
	return len(gone), nil
}

// RegisterCluster records that an agent for cluster name (identified by uid) checked in.
// It fails with ErrClusterConflict if another cluster already uses the name, so two
// clusters can never mix their histories.
func (s *Store) RegisterCluster(ctx context.Context, name, uid string) error {
	var existing string
	err := s.db.QueryRowContext(ctx, "SELECT uid FROM clusters WHERE name = ?", name).Scan(&existing)
	if err == nil && existing != uid {
		return ErrClusterConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO clusters (name, uid, last_seen) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET last_seen = excluded.last_seen`, name, uid, time.Now().UnixNano())
	return err
}

type Cluster struct {
	Name     string    `json:"name"`
	UID      string    `json:"uid"`
	LastSeen time.Time `json:"lastSeen"`
}

func (s *Store) Clusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, uid, last_seen FROM clusters ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cluster{}
	for rows.Next() {
		var c Cluster
		var ts int64
		if err := rows.Scan(&c.Name, &c.UID, &ts); err != nil {
			return nil, err
		}
		c.LastSeen = time.Unix(0, ts).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// Checkpoints returns the last resourceVersion stored per resource, so a restarted agent
// resumes its watches without keeping local state.
func (s *Store) Checkpoints(ctx context.Context, cluster string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT resource, resource_version FROM checkpoints WHERE cluster = ?", cluster)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var res, rv string
		if err := rows.Scan(&res, &rv); err != nil {
			return nil, err
		}
		out[res] = rv
	}
	return out, rows.Err()
}

func (s *Store) SetCheckpoint(ctx context.Context, cluster, resource, rv string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO checkpoints (cluster, resource, resource_version) VALUES (?, ?, ?)
		ON CONFLICT (cluster, resource) DO UPDATE SET resource_version = excluded.resource_version`, cluster, resource, rv)
	return err
}

func (s *Store) list(ctx context.Context, q string, args ...any) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{} // [] not null in JSON
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
		&c.ResourceVersion, &ts, &c.Type, &c.Author, &c.Source, &c.Drift, &paths}
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
