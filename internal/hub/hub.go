// Package hub receives changes from agents and serves the history API.
package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guipguia/mnemo/internal/store"
)

// Batch is what an agent pushes. The hub stores all of it in one transaction, or none of it.
// Session, Stream and Seq order a stream's batches: the hub drops one that comes after a
// later one, such as a slow original that finishes after its own retry (see store.Fresh).
type Batch struct {
	Cluster     string            `json:"cluster"`
	ClusterUID  string            `json:"clusterUID"`        // kube-system namespace UID
	Session     string            `json:"session,omitempty"` // this agent run, from Resume
	Stream      string            `json:"stream,omitempty"`  // e.g. a watched resource; empty: unordered
	Seq         int64             `json:"seq,omitempty"`     // increases within the stream
	Items       []Item            `json:"items,omitempty"`
	Synced      []Synced          `json:"synced,omitempty"`
	Checkpoints map[string]string `json:"checkpoints,omitempty"` // "group/version/resource" -> resourceVersion
	Audits      []Audit           `json:"audits,omitempty"`
	Events      []store.Event     `json:"events,omitempty"`
	Deploys     []store.Deploy    `json:"deploys,omitempty"`
}

// Audit is a write seen by the cluster's audit webhook: who wrote namespace/name at
// ResourceVersion, or who deleted the object with UID (most deletes return no resourceVersion).
type Audit struct {
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	UID             string `json:"uid,omitempty"` // deletes only
	User            string `json:"user"`
	UserAgent       string `json:"userAgent"`
}

// Item is one observed object. The hub, not the agent, decides authorship: only the hub
// knows which paths changed, and the author is whoever owns them in Managers.
type Item struct {
	store.Change
	Listed   bool                        `json:"listed"` // from a list, not a watch event
	Managers []metav1.ManagedFieldsEntry `json:"managers,omitempty"`
}

// Synced reports a completed relist: every live object of Group/Kind not in Keep was deleted.
type Synced struct {
	Group string    `json:"group"`
	Kind  string    `json:"kind"`
	Keep  []string  `json:"keep"` // "namespace/name"
	Time  time.Time `json:"time"`
}

// Resume registers the cluster and returns its checkpoints. Registering first means an
// agent can never resume from checkpoints that belong to another cluster with the same name.
// session, if set, becomes the cluster's current agent run: batches from earlier runs are
// dropped from now on, so an old agent pod still shutting down can't write over a new one.
func Resume(ctx context.Context, s *store.Store, cluster, uid, session string) (map[string]string, error) {
	if cluster == "" || uid == "" {
		return nil, errors.New("cluster and uid are required")
	}
	if err := s.Write(ctx, func(w *store.Writer) error {
		if err := w.RegisterCluster(ctx, cluster, uid); err != nil {
			return err
		}
		if session != "" {
			return w.SetSession(ctx, cluster, session)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s.Checkpoints(ctx, cluster)
}

// IngestFailures counts batches Ingest rejected or failed to store, for /metrics.
var IngestFailures atomic.Int64

func Ingest(ctx context.Context, s *store.Store, b Batch) error {
	err := ingest(ctx, s, b)
	if err != nil && ctx.Err() == nil {
		IngestFailures.Add(1)
	}
	return err
}

func ingest(ctx context.Context, s *store.Store, b Batch) error {
	if b.Cluster == "" || b.ClusterUID == "" {
		return errors.New("cluster and clusterUID are required")
	}
	return s.Write(ctx, func(w *store.Writer) error { return ingestTx(ctx, w, b) })
}

func ingestTx(ctx context.Context, s *store.Writer, b Batch) error {
	if err := s.RegisterCluster(ctx, b.Cluster, b.ClusterUID); err != nil {
		return err
	}
	if fresh, err := s.Fresh(ctx, b.Cluster, b.Session, b.Stream, b.Seq); err != nil || !fresh {
		if err == nil {
			slog.Info("dropped a stale batch", "cluster", b.Cluster, "stream", b.Stream, "seq", b.Seq)
		}
		return err // stale: already stored, or superseded; the agent moves on
	}
	for _, it := range b.Items {
		it.Cluster = b.Cluster
		if _, err := s.Record(ctx, &it.Change, it.Listed, func(c *store.Change) { attribute(c, it.Managers) }); err != nil {
			return err
		}
	}
	for _, sy := range b.Synced {
		keep := make(map[string]bool, len(sy.Keep))
		for _, k := range sy.Keep {
			keep[k] = true
		}
		if _, err := s.Prune(ctx, b.Cluster, sy.Group, sy.Kind, keep, sy.Time); err != nil {
			return err
		}
	}
	// After Items: audit events trail the watch, often by seconds, so the change is usually
	// recorded already, possibly in this same batch.
	// ponytail: an audit event that beats its watch event is dropped and the field manager stays
	// the author; hold unmatched ones for a few minutes if that shows up in practice.
	for _, a := range b.Audits {
		if _, err := s.Reattribute(ctx, b.Cluster, a.Namespace, a.Name, a.ResourceVersion, a.UID,
			func(c *store.Change) { attributeUser(c, a) }); err != nil {
			return err
		}
	}
	if len(b.Deploys) > 0 {
		if err := s.RecordDeploys(ctx, b.Cluster, b.Deploys); err != nil {
			return err
		}
	}
	if len(b.Events) > 0 {
		if err := s.RecordEvents(ctx, b.Cluster, b.Events); err != nil {
			return err
		}
	}
	for res, rv := range b.Checkpoints {
		if err := s.SetCheckpoint(ctx, b.Cluster, res, rv); err != nil {
			return err
		}
	}
	return nil
}

// Handler serves the read API, limited to what access(r) allows; objects the caller may
// not see look exactly like objects that don't exist. Agent endpoints (ingest,
// checkpoints) use the token instead, and are only registered when it is set.
func Handler(s *store.Store, token string, access func(*http.Request) store.Access) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/clusters", func(w http.ResponseWriter, r *http.Request) {
		all, err := s.Clusters(r.Context())
		a := access(r)
		visible := []store.Cluster{}
		for _, c := range all {
			if a.AllowsCluster(c.Name) {
				visible = append(visible, c)
			}
		}
		reply(w)(visible, err)
	})

	mux.HandleFunc("GET /api/v1/changes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := store.Filter{
			Access:  access(r),
			Cluster: q.Get("cluster"), Namespace: q.Get("namespace"), Kind: q.Get("kind"),
			Author: q.Get("author"), Source: q.Get("source"), DriftOnly: q.Get("drift") == "true",
		}
		var err error
		if f.Since, err = parseTime(q, "since"); err == nil {
			if f.Until, err = parseTime(q, "until"); err == nil {
				f.Limit, err = parseInt(q, "limit")
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply(w)(s.Changes(r.Context(), f))
	})

	mux.HandleFunc("GET /api/v1/changes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		c, err := s.Get(r.Context(), id)
		if err == nil && !access(r).Allows(c.Cluster, c.Namespace) {
			err = store.ErrNotFound
		}
		reply(w)(struct {
			store.Change
			Commit *Commit `json:"commit,omitempty"`
		}{c, commitFor(r.Context(), s, c)}, err)
	})

	mux.HandleFunc("GET /api/v1/history", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, err := parseInt(q, "limit")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rf := ref(q); access(r).Allows(rf.Cluster, rf.Namespace) {
			reply(w)(s.History(r.Context(), rf, limit))
		} else {
			reply(w)([]store.Change{}, nil) // same as an object with no history
		}
	})

	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := store.EventFilter{
			Access:  access(r),
			Cluster: q.Get("cluster"), Group: q.Get("group"), Kind: q.Get("kind"),
			Namespace: q.Get("namespace"), Name: q.Get("name"), Type: q.Get("type"),
		}
		var err error
		if f.Since, err = parseTime(q, "since"); err == nil {
			if f.Until, err = parseTime(q, "until"); err == nil {
				f.Limit, err = parseInt(q, "limit")
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply(w)(s.Events(r.Context(), f))
	})

	mux.HandleFunc("GET /api/v1/at", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		t, err := parseTime(q, "time")
		if err == nil && t.IsZero() {
			t = time.Now()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rf := ref(q); access(r).Allows(rf.Cluster, rf.Namespace) {
			reply(w)(s.At(r.Context(), rf, t))
		} else {
			reply(w)(nil, store.ErrNotFound)
		}
	})

	if token != "" {
		agentRoutes(mux, s, token, false)
	}
	return mux
}

// AgentHandler serves only the agent endpoints, for the mTLS agent listener: besides the
// token, each request's client certificate must name the cluster it pushes for, so one
// cluster's agent can't write another's history.
func AgentHandler(s *store.Store, token string) http.Handler {
	mux := http.NewServeMux()
	agentRoutes(mux, s, token, true)
	return mux
}

func agentRoutes(mux *http.ServeMux, s *store.Store, token string, clusterCert bool) {
	forbidden := func(w http.ResponseWriter, cluster string) {
		http.Error(w, fmt.Sprintf("client certificate does not name cluster %q (common name or DNS name)", cluster), http.StatusForbidden)
	}
	mux.Handle("POST /api/v1/ingest", RequireToken(token, func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&b); err != nil {
			IngestFailures.Add(1)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if clusterCert && !certNames(r, b.Cluster) {
			IngestFailures.Add(1)
			forbidden(w, b.Cluster)
			return
		}
		reply(w)(struct{}{}, Ingest(r.Context(), s, b))
	}))
	mux.Handle("GET /api/v1/checkpoints", RequireToken(token, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if clusterCert && !certNames(r, q.Get("cluster")) {
			forbidden(w, q.Get("cluster"))
			return
		}
		reply(w)(Resume(r.Context(), s, q.Get("cluster"), q.Get("uid"), q.Get("session")))
	}))
}

// RequireToken rejects requests without "Authorization: Bearer <token>".
func RequireToken(token string, next http.HandlerFunc) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}

// reply writes v as JSON, or maps err to a status code.
func reply(w http.ResponseWriter) func(any, error) {
	return func(v any, err error) {
		switch {
		case errors.Is(err, store.ErrClusterConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, store.ErrSuperseded):
			http.Error(w, err.Error(), http.StatusGone)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v)
		}
	}
}

func ref(q url.Values) store.Ref {
	return store.Ref{Cluster: q.Get("cluster"), Group: q.Get("group"), Kind: q.Get("kind"),
		Namespace: q.Get("namespace"), Name: q.Get("name")}
}

func parseTime(q url.Values, key string) (time.Time, error) {
	v := q.Get(key)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return t, fmt.Errorf("%s: want RFC 3339, e.g. 2026-10-20T14:32:00Z", key)
	}
	return t, nil
}

func parseInt(q url.Values, key string) (int, error) {
	v := q.Get(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: not a number", key)
	}
	return n, nil
}
