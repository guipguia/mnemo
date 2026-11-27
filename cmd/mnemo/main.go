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
	"net/url"
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
	"github.com/guipguia/mnemo/internal/webhook"
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
	// Secrets default from the environment after parsing, never as flag defaults: usage
	// output (-h, or any flag mistake) prints defaults.
	token := fs.String("token", "", "shared agent/hub token (default $MNEMO_TOKEN)")
	var db, listen, metricsListen, webhooks, cluster, hubURL, kubeconfig, resources *string
	var auditListen, auditToken, auditCert, auditKey *string
	var agentListen, agentCert, agentKey, agentCA, hubCA, clientCert, clientKey *string
	var retentionDays, retentionVersions, eventRetentionDays *int
	var newAuth func(context.Context) (*auth.Auth, error)
	if mode == "hub" || mode == "all" {
		db = fs.String("db", "", "SQLite database path, or a postgres:// URL (default $MNEMO_DB, then mnemo.db); see docs/postgres.md")
		listen = fs.String("listen", ":8080", "HTTP listen address")
		metricsListen = fs.String("metrics-listen", "", "serve Prometheus /metrics here, e.g. :9090 (default: off); see docs/metrics.md")
		retentionDays = fs.Int("retention-days", 0, "delete versions older than this many days (0: keep forever); each object's current state is kept")
		retentionVersions = fs.Int("retention-versions", 0, "keep at most this many versions per object (0: no limit)")
		webhooks = fs.String("webhooks", "", "outbound webhooks file (default: none); see docs/webhooks.md")
		eventRetentionDays = fs.Int("event-retention-days", 30, "delete archived Kubernetes Events last seen this many days ago (0: keep forever)")
		agentListen = fs.String("agent-listen", "", "serve agents over mutual TLS here, e.g. :8443, and stop taking pushes on --listen (default: off); see docs/mtls.md")
		agentCert = fs.String("agent-tls-cert", "", "the hub's TLS certificate for --agent-listen")
		agentKey = fs.String("agent-tls-key", "", "the hub's TLS key for --agent-listen")
		agentCA = fs.String("agent-ca", "", "CA that signs agent client certificates, for --agent-listen")
		newAuth = authFlags(fs)
	}
	if mode == "agent" || mode == "all" {
		cluster = fs.String("cluster", "", "cluster name shown in mnemo (required)")
		kubeconfig = fs.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
		resources = fs.String("resources", strings.Join(agent.DefaultResources, ","), "comma-separated group/version/resource list")
		auditListen = fs.String("audit-listen", "", "receive the API server's audit webhook here, e.g. :8443 (default: off); see docs/audit.md")
		auditToken = fs.String("audit-token", "", "bearer token the audit webhook must send (default $MNEMO_AUDIT_TOKEN)")
		auditCert = fs.String("audit-tls-cert", "", "TLS certificate for the audit webhook (required with --audit-listen)")
		auditKey = fs.String("audit-tls-key", "", "TLS key for the audit webhook (required with --audit-listen)")
	}
	if mode == "agent" {
		hubURL = fs.String("hub", "", "hub URL (required)")
		hubCA = fs.String("hub-ca", "", "CA to trust for the hub's certificate (default: the system's)")
		clientCert = fs.String("client-cert", "", "client certificate for a hub with --agent-listen; must name --cluster")
		clientKey = fs.String("client-key", "", "client key for --client-cert")
	}
	if mode != "hub" && mode != "agent" && mode != "all" {
		return fmt.Errorf("unknown mode %q: want hub, agent, or all", mode)
	}
	fs.Parse(args)
	fromEnv(token, "MNEMO_TOKEN", "")
	fromEnv(db, "MNEMO_DB", "mnemo.db")
	fromEnv(auditToken, "MNEMO_AUDIT_TOKEN", "")

	var st *store.Store
	var au *auth.Auth
	var hooks *webhook.Config
	if db != nil {
		if mode == "hub" && *token == "" {
			return errors.New("hub mode needs --token, or agents can't push")
		}
		var err error
		if au, err = newAuth(ctx); err != nil {
			return err
		}
		if *webhooks != "" {
			if hooks, err = webhook.Load(*webhooks); err != nil {
				return err
			}
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
			tc, err := hub.ClientTLS(*hubCA, *clientCert, *clientKey)
			if err != nil {
				return fmt.Errorf("agent TLS: %w", err)
			}
			c := hub.NewClient(strings.TrimSuffix(*hubURL, "/"), *token, tc)
			ag.Push, ag.Resume = c.Push, c.Resume
		}
	}

	errc := make(chan error, 4)
	if st != nil {
		pushToken := *token // with an agent listener, pushes go there only: the token alone isn't enough
		if *agentListen != "" {
			if *agentCert == "" || *agentKey == "" || *agentCA == "" {
				return errors.New("--agent-listen needs --agent-tls-cert, --agent-tls-key and --agent-ca")
			}
			if *token == "" {
				return errors.New("--agent-listen needs --token: agents send it too")
			}
			tc, err := hub.AgentTLS(*agentCert, *agentKey, *agentCA)
			if err != nil {
				return fmt.Errorf("agent listener: %w", err)
			}
			asrv := &http.Server{Addr: *agentListen, Handler: hub.AgentHandler(st, *token), TLSConfig: tc, ReadHeaderTimeout: 10 * time.Second}
			go func() { errc <- asrv.ListenAndServeTLS("", "") }()
			go func() { <-ctx.Done(); asrv.Close() }()
			pushToken = ""
		}
		srv := &http.Server{Addr: *listen, Handler: routes(st, pushToken, au), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- srv.ListenAndServe() }()
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.Shutdown(shutdown)
		}()
		slog.Info("hub listening", "version", version, "addr", *listen, "db", redact(*db), "ingest", pushToken != "", "agentListen", *agentListen, "auth", au.Mode(),
			"webhooks", *webhooks, "retentionDays", *retentionDays, "retentionVersions", *retentionVersions, "eventRetentionDays", *eventRetentionDays)
		if *metricsListen != "" {
			// Separate listener: metrics name clusters and webhooks, and skip the access policy.
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", metrics(st, hooks))
			msrv := &http.Server{Addr: *metricsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			go func() { errc <- msrv.ListenAndServe() }()
			go func() { <-ctx.Done(); msrv.Close() }()
		}
		if hooks != nil {
			go webhook.Run(ctx, st, hooks)
		}
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

// fromEnv fills an unset flag (p may be nil: not in this mode) from $key, then def.
func fromEnv(p *string, key, def string) {
	if p == nil || *p != "" {
		return
	}
	if *p = os.Getenv(key); *p == "" {
		*p = def
	}
}

// redact hides a database URL's password, in the user info or as ?password=, for logging.
func redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || !store.IsPostgres(dsn) {
		return dsn
	}
	if q := u.Query(); q.Has("password") {
		q.Set("password", "xxxxx")
		u.RawQuery = q.Encode()
	}
	return u.Redacted()
}

func loopback(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
