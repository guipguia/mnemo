package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
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
// session advisory lock on a connection of its own; the lock goes when that session does.
// The session asks the server to drop it within about 30 seconds of the hub going silent
// (TCP keepalives and user timeout), so a crashed or cut-off leader is replaced in under a
// minute, not after the server's default two hours. The cut-off leader notices on its next
// ping and stops; until then two leaders can overlap, which webhook delivery (at-least-once)
// tolerates.
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
				pctx, pcancel := context.WithTimeout(ctx, leaderRetry)
				err := conn.PingContext(pctx)
				pcancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("lost the leader lock's connection; standing by", "err", err)
					break
				}
			}
			cancel()
			<-done
			discard(conn) // ends the session, and with it the lock
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
	// If this hub vanishes without closing the connection, have the server notice in ~30s.
	if _, err := conn.ExecContext(ctx, `SET tcp_keepalives_idle = 5; SET tcp_keepalives_interval = 5;
		SET tcp_keepalives_count = 3; SET tcp_user_timeout = 15000`); err != nil {
		if ctx.Err() == nil {
			slog.Warn("leader election", "err", err)
		}
		discard(conn)
		return false, nil
	}
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", leaderLock).Scan(&got); err != nil || !got {
		if err != nil && ctx.Err() == nil {
			slog.Warn("leader election", "err", err)
		}
		discard(conn)
		return false, nil
	}
	return true, conn
}

// discard closes conn's session instead of returning it to the pool, where a held lock
// would stay held.
func discard(conn *sql.Conn) {
	conn.Raw(func(any) error { return driver.ErrBadConn })
	conn.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
