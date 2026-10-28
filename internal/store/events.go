package store

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// Event is a Kubernetes Event, kept after the API server expires it (~1h). Ref is the
// object the event is about (involvedObject), so access rules apply to it like to a change.
type Event struct {
	Ref
	UID       string    `json:"uid"`  // the Event's own UID
	Type      string    `json:"type"` // Normal | Warning
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Source    string    `json:"source"` // reporting component, e.g. deployment-controller
	Count     int       `json:"count"`
	FirstTime time.Time `json:"firstTime"`
	LastTime  time.Time `json:"lastTime"`
}

// RecordEvents stores events, or updates them as the API server bumps their count.
func (s *Store) RecordEvents(ctx context.Context, cluster string, evs []Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range evs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events
			(cluster, uid, api_group, kind, namespace, name, type, reason, message, source, count, first_ts, last_ts)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (cluster, uid) DO UPDATE SET message = excluded.message, count = excluded.count,
				last_ts = MAX(last_ts, excluded.last_ts)`,
			cluster, e.UID, e.Group, e.Kind, e.Namespace, e.Name, e.Type, e.Reason, e.Message, e.Source,
			e.Count, e.FirstTime.UnixNano(), e.LastTime.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EventFilter narrows Events. Empty fields match everything. With Kind and Name set, it
// also matches the object's rollout: a Deployment's ReplicaSets and Pods, a StatefulSet's
// or DaemonSet's Pods.
type EventFilter struct {
	Access                                Access // required: the zero value returns nothing
	Cluster, Group, Kind, Namespace, Name string
	Type                                  string
	Since, Until                          time.Time // on LastTime; Since inclusive, Until exclusive
	Limit                                 int       // default 100, max 1000
}

// Events lists events, most recent first.
func (s *Store) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	cond, args := f.Access.sql()
	where := []string{cond}
	add := func(cond string, v ...any) {
		where = append(where, cond)
		args = append(args, v...)
	}
	for col, v := range map[string]string{"cluster": f.Cluster, "namespace": f.Namespace, "type": f.Type} {
		if v != "" {
			add(col+" = ?", v)
		}
	}
	var kids map[string]*regexp.Regexp
	switch {
	case f.Name != "":
		if kids = rollout(f.Group, f.Kind, f.Name); kids != nil {
			// Children's names start with the object's; the regexps below keep the real ones.
			add("((api_group = ? AND kind = ? AND name = ?) OR substr(name, 1, ?) = ?)",
				f.Group, f.Kind, f.Name, len(f.Name)+1, f.Name+"-")
		} else {
			add("api_group = ? AND kind = ? AND name = ?", f.Group, f.Kind, f.Name)
		}
	case f.Kind != "":
		add("api_group = ? AND kind = ?", f.Group, f.Kind)
	}
	if !f.Since.IsZero() {
		add("last_ts >= ?", f.Since.UnixNano())
	}
	if !f.Until.IsZero() {
		add("last_ts < ?", f.Until.UnixNano())
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	q := `SELECT cluster, uid, api_group, kind, namespace, name, type, reason, message, source, count, first_ts, last_ts
		FROM events WHERE ` + strings.Join(where, " AND ") + " ORDER BY last_ts DESC, uid"
	if kids == nil {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	// With kids, rows are filtered here, so read until there are enough.
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for len(out) < limit && rows.Next() {
		var e Event
		var first, last int64
		if err := rows.Scan(&e.Cluster, &e.UID, &e.Group, &e.Kind, &e.Namespace, &e.Name, &e.Type,
			&e.Reason, &e.Message, &e.Source, &e.Count, &first, &last); err != nil {
			return nil, err
		}
		self := e.Group == f.Group && e.Kind == f.Kind && e.Name == f.Name
		if re := kids[e.Group+"/"+e.Kind]; kids != nil && !self && (re == nil || !re.MatchString(e.Name)) {
			continue
		}
		e.FirstTime, e.LastTime = time.Unix(0, first).UTC(), time.Unix(0, last).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExpireEvents deletes events last seen before t and reports how many.
func (s *Store) ExpireEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE last_ts < ?", before.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// hash is the alphabet of pod-template-hash and generated name suffixes (rand.SafeEncodeString).
const hash = "[bcdfghjklmnpqrstvwxz2456789]"

// rollout maps "group/Kind" of a workload's children to the names they get from the
// controllers, or returns nil if the kind has none. Names are all an Event carries.
// ponytail: name patterns, not ownerReferences; "web" claims "web-7d9f8" even if another
// Deployment named "web-7d9f8" exists. Watch ReplicaSets and Pods if that shows up.
func rollout(group, kind, name string) map[string]*regexp.Regexp {
	n := regexp.QuoteMeta(name)
	if group != "apps" {
		return nil
	}
	switch kind {
	case "Deployment":
		return map[string]*regexp.Regexp{
			"apps/ReplicaSet": regexp.MustCompile("^" + n + "-" + hash + "{1,10}$"),
			"/Pod":            regexp.MustCompile("^" + n + "-" + hash + "{1,10}-" + hash + "{5}$"),
		}
	case "StatefulSet":
		return map[string]*regexp.Regexp{"/Pod": regexp.MustCompile("^" + n + "-[0-9]+$")}
	case "DaemonSet":
		return map[string]*regexp.Regexp{"/Pod": regexp.MustCompile("^" + n + "-" + hash + "{5}$")}
	}
	return nil
}
