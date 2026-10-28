// Command mnemo records Kubernetes object history.
//
//	mnemo hub   --db /data/mnemo.db --token $MNEMO_TOKEN
//	mnemo agent --cluster prod --hub https://mnemo.example.com --token $MNEMO_TOKEN
//	mnemo all   --cluster prod --db /data/mnemo.db
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/guipguia/mnemo/internal/agent"
	"github.com/guipguia/mnemo/internal/auth"
	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

// version is set at release build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mnemo hub|agent|all|version [flags]")
		os.Exit(2)
	}
	if os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, mode string, args []string) error {
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	token := fs.String("token", os.Getenv("MNEMO_TOKEN"), "shared agent/hub token (default $MNEMO_TOKEN)")
	var db, listen, cluster, hubURL, kubeconfig, resources *string
	var auditListen, auditToken, auditCert, auditKey *string
	var retentionDays, retentionVersions, eventRetentionDays *int
	var newAuth func(context.Context) (*auth.Auth, error)
	if mode == "hub" || mode == "all" {
		db = fs.String("db", "mnemo.db", "SQLite database path")
		listen = fs.String("listen", ":8080", "HTTP listen address")
		retentionDays = fs.Int("retention-days", 0, "delete versions older than this many days (0: keep forever); each object's current state is kept")
		retentionVersions = fs.Int("retention-versions", 0, "keep at most this many versions per object (0: no limit)")
		eventRetentionDays = fs.Int("event-retention-days", 30, "delete archived Kubernetes Events last seen this many days ago (0: keep forever)")
		newAuth = authFlags(fs)
	}
	if mode == "agent" || mode == "all" {
		cluster = fs.String("cluster", "", "cluster name shown in mnemo (required)")
		kubeconfig = fs.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
		resources = fs.String("resources", strings.Join(agent.DefaultResources, ","), "comma-separated group/version/resource list")
		auditListen = fs.String("audit-listen", "", "receive the API server's audit webhook here, e.g. :8443 (default: off); see docs/audit.md")
		auditToken = fs.String("audit-token", os.Getenv("MNEMO_AUDIT_TOKEN"), "bearer token the audit webhook must send (default $MNEMO_AUDIT_TOKEN)")
		auditCert = fs.String("audit-tls-cert", "", "TLS certificate for the audit webhook (required with --audit-listen)")
		auditKey = fs.String("audit-tls-key", "", "TLS key for the audit webhook (required with --audit-listen)")
	}
	if mode == "agent" {
		hubURL = fs.String("hub", "", "hub URL (required)")
	}
	if mode != "hub" && mode != "agent" && mode != "all" {
		return fmt.Errorf("unknown mode %q: want hub, agent, or all", mode)
	}
	fs.Parse(args)

	var st *store.Store
	var au *auth.Auth
	if db != nil {
		if mode == "hub" && *token == "" {
			return errors.New("hub mode needs --token, or agents can't push")
		}
		var err error
		if au, err = newAuth(ctx); err != nil {
			return err
		}
		if st, err = store.Open(*db); err != nil {
			return err
		}
		defer st.Close()
	}

	var ag *agent.Agent
	if cluster != nil {
		if *cluster == "" {
			return errors.New("--cluster is required")
		}
		var gvrs []schema.GroupVersionResource
		for _, r := range strings.Split(*resources, ",") {
			gvr, err := agent.ParseResource(strings.TrimSpace(r))
			if err != nil {
				return err
			}
			gvrs = append(gvrs, gvr)
		}
		// No ~/.kube/config fallback: without --kubeconfig, only in-cluster config.
		cfg, err := rest.InClusterConfig()
		if *kubeconfig != "" {
			cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		}
		if err != nil {
			return err
		}
		client, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return err
		}
		if *auditListen != "" && *auditToken == "" {
			return errors.New("--audit-listen needs --audit-token, or anyone could claim to be any user")
		}
		// The API server only sends a kubeconfig's token over HTTPS, so there's no plain mode.
		if *auditListen != "" && (*auditCert == "" || *auditKey == "") {
			return errors.New("--audit-listen needs --audit-tls-cert and --audit-tls-key")
		}
		ag = &agent.Agent{Client: client, Cluster: *cluster, Resources: gvrs,
			AuditAddr: *auditListen, AuditToken: *auditToken, AuditCert: *auditCert, AuditKey: *auditKey}
		if st != nil {
			ag.Push = func(ctx context.Context, b hub.Batch) error { return hub.Ingest(ctx, st, b) }
			ag.Resume = func(ctx context.Context, cluster, uid string) (map[string]string, error) {
				return hub.Resume(ctx, st, cluster, uid)
			}
		} else {
			if *hubURL == "" || *token == "" {
				return errors.New("agent mode needs --hub and --token")
			}
			c := hub.NewClient(strings.TrimSuffix(*hubURL, "/"), *token)
			ag.Push, ag.Resume = c.Push, c.Resume
		}
	}

	errc := make(chan error, 2)
	if st != nil {
		srv := &http.Server{Addr: *listen, Handler: routes(st, *token, au), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- srv.ListenAndServe() }()
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.Shutdown(shutdown)
		}()
		slog.Info("hub listening", "version", version, "addr", *listen, "db", *db, "ingest", *token != "", "auth", au.Mode(),
			"retentionDays", *retentionDays, "retentionVersions", *retentionVersions, "eventRetentionDays", *eventRetentionDays)
		if *retentionDays > 0 || *retentionVersions > 0 || *eventRetentionDays > 0 {
			go expire(ctx, st, *retentionDays, *retentionVersions, *eventRetentionDays)
		}
		if au.Mode() == "none" && !loopback(*listen) {
			slog.Warn("--auth none on a non-loopback address: anyone who can reach the hub sees all history")
		}
	}
	if ag != nil {
		go func() { errc <- ag.Run(ctx) }()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// expire applies the retention policy at startup and then hourly.
func expire(ctx context.Context, st *store.Store, days, versions, eventDays int) {
	for {
		var before time.Time
		if days > 0 {
			before = time.Now().AddDate(0, 0, -days)
		}
		if n, err := st.Expire(ctx, before, versions); err != nil {
			slog.Error("retention", "err", err)
		} else if n > 0 {
			slog.Info("retention: deleted old versions", "rows", n)
		}
		if eventDays > 0 {
			if n, err := st.ExpireEvents(ctx, time.Now().AddDate(0, 0, -eventDays)); err != nil {
				slog.Error("event retention", "err", err)
			} else if n > 0 {
				slog.Info("retention: deleted old events", "rows", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

func loopback(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
