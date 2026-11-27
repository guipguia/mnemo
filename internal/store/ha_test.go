package store

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// twoHubs opens the same PostgreSQL database twice, as two hubs would.
func twoHubs(t *testing.T) (*Store, *Store) {
	dsn := testDSN(t)
	if !IsPostgres(dsn) {
		t.Skip("several hubs need PostgreSQL ($MNEMO_TEST_POSTGRES)")
	}
	a, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// An agent's retried push can land on two hubs at once: the object must still get one version.
func TestHubsRecordSameObject(t *testing.T) {
	a, b := twoHubs(t)
	ctx := t.Context()
	for round := range 20 {
		var wg sync.WaitGroup
		for i := range 8 {
			s := []*Store{a, b}[i%2]
			wg.Go(func() {
				c := &Change{Ref: Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "n", Name: fmt.Sprint("x", round)},
					Time: time.Now(), Object: []byte(`{"v":1}`)}
				if _, err := s.Record(ctx, c, false, nil); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if h, _ := a.History(ctx, Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "n", Name: fmt.Sprint("x", round)}, 10); len(h) != 1 {
			t.Fatalf("round %d: %d versions, want 1", round, len(h))
		}
	}
}

// A reader following IDs, as a webhook does, must never see a change before every change
// with a lower ID: while hub b's insert is uncommitted, hub a's later one must wait.
func TestHubsIDsCommitInOrder(t *testing.T) {
	a, b := twoHubs(t)
	ctx := t.Context()
	change := func(name string) *Change {
		return &Change{Ref: Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "n", Name: name}, Time: time.Now(), Object: []byte(`{}`)}
	}
	tx, err := b.db.BeginTx(ctx, nil) // what Record does, held open
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO changes (`+cols[len("id, "):]+`)
		VALUES ('prod', '', 'v1', 'ConfigMap', 'n', 'slow', '', '', 1, 'added', '', '', false, NULL)`); err != nil {
		t.Fatal(err)
	}
	recorded := make(chan error)
	go func() { _, err := a.Record(ctx, change("fast"), false, nil); recorded <- err }()
	select {
	case err := <-recorded:
		t.Fatalf("hub a committed a change while a lower ID was still open (err %v): a cursor would skip it", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-recorded; err != nil {
		t.Fatal(err)
	}
	cs, _ := a.After(ctx, 0, 1<<62, 10)
	if len(cs) != 2 || cs[0].Name != "slow" || cs[1].Name != "fast" {
		t.Errorf("ID order: %+v", cs)
	}
}

func TestLeader(t *testing.T) {
	leaderRetry = 20 * time.Millisecond
	defer func() { leaderRetry = 5 * time.Second }()
	a, b := twoHubs(t)
	var leading atomic.Int32
	var which atomic.Value
	var strict atomic.Bool // no overlap allowed: a leader that loses its session may overlap briefly
	strict.Store(true)
	run := func(s *Store, name string) (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			s.Leader(ctx, func(ctx context.Context) {
				if leading.Add(1) != 1 && strict.Load() {
					t.Error("two leaders at once")
				}
				which.Store(name)
				<-ctx.Done()
				leading.Add(-1)
			})
			close(done)
		}()
		return func() { cancel(); <-done }
	}
	waitLeader := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); which.Load() != want; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("leader = %v, want %s", which.Load(), want)
			}
		}
	}
	stopA := run(a, "a")
	waitLeader("a")
	stopB := run(b, "b")
	defer stopB()
	time.Sleep(100 * time.Millisecond) // b keeps trying, and must not lead while a does
	if which.Load() != "a" {
		t.Fatal("b took over from a live leader")
	}
	stopA() // a shuts down: b takes over
	waitLeader("b")

	// b's connection dies (as when its node does): a new hub leads, and b stands down.
	strict.Store(false)
	c, d := twoHubs(t)
	stopC := run(c, "c")
	defer stopC()
	b.lockDB.Close() // a dead node doesn't come back: b keeps its held connection, but can't reconnect
	// Only the leader lock's holder: other runs on the same database hold the write lock.
	if _, err := d.db.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid != pg_backend_pid()
		AND classid = ? AND objid = ? AND objsubid = 1`, leaderLock>>32, leaderLock&0xffffffff); err != nil {
		t.Fatal(err)
	}
	waitLeader("c")
	for deadline := time.Now().Add(5 * time.Second); leading.Load() != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d leaders: b never stood down", leading.Load())
		}
	}
}
