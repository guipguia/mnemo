package store

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// leaderRetry is how often a standby hub tries to become leader, and how often the leader
// checks it still is.
var leaderRetry = 5 * time.Second

// Leader runs f while this hub is the leader, and again each time it becomes leader after
// losing it, until ctx ends. Work that must happen once per database, not once per hub
// (webhook delivery, retention), runs in f; f's context ends when leadership is lost.
//
// With SQLite there is only one hub, so f just runs. On PostgreSQL the leader holds a
// session advisory lock on a connection of its own; the lock goes when that connection does
// (hub crash, network loss), and a standby takes over within leaderRetry. A hub cut off from
// the database may keep running f until it notices, so for a few seconds two leaders can
// overlap: webhook delivery is at-least-once anyway.
func (s *Store) Leader(ctx context.Context, f func(context.Context)) {
	if !s.db.pg {
		f(ctx)
		return
	}
	for ctx.Err() == nil {
		if held, conn := s.tryLead(ctx); held {
			slog.Info("this hub is now the leader: running webhooks and retention")
			lctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { f(lctx); close(done) }()
			for lctx.Err() == nil {
				sleepCtx(lctx, leaderRetry)
				if err := conn.PingContext(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("lost the leader lock's connection; standing by", "err", err)
					break
				}
			}
			cancel()
			<-done
			conn.Close() // releases the lock if the connection still works
			continue
		}
		sleepCtx(ctx, leaderRetry)
	}
}

// tryLead takes the leader lock on a dedicated connection, outside the store's single shared
// one, which the lock would otherwise hold hostage.
func (s *Store) tryLead(ctx context.Context) (bool, *sql.Conn) {
	conn, err := s.lockDB.Conn(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("leader election", "err", err)
		}
		return false, nil
	}
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", leaderLock).Scan(&got); err != nil || !got {
		if err != nil && ctx.Err() == nil {
			slog.Warn("leader election", "err", err)
		}
		conn.Close()
		return false, nil
	}
	return true, conn
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
