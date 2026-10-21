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
	"k8s.io/client-go/tools/clientcmd"

	"github.com/guipguia/mnemo/internal/agent"
	"github.com/guipguia/mnemo/internal/auth"
	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
	"github.com/guipguia/mnemo/ui"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mnemo hub|agent|all [flags]")
		os.Exit(2)
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
	var newAuth func(context.Context) (*auth.Auth, error)
	if mode == "hub" || mode == "all" {
		db = fs.String("db", "mnemo.db", "SQLite database path")
		listen = fs.String("listen", ":8080", "HTTP listen address")
		newAuth = authFlags(fs)
	}
	if mode == "agent" || mode == "all" {
		cluster = fs.String("cluster", "", "cluster name shown in mnemo (required)")
		kubeconfig = fs.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
		resources = fs.String("resources", strings.Join(agent.DefaultResources, ","), "comma-separated group/version/resource list")
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
		cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			return err
		}
		client, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return err
		}
		ag = &agent.Agent{Client: client, Cluster: *cluster, Resources: gvrs}
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
		api := hub.Handler(st, *token, au.Access)
		mux := http.NewServeMux()
		mux.Handle("/api/v1/ingest", api) // agents: hub token, not SSO
		mux.Handle("/api/v1/checkpoints", api)
		mux.Handle("/api/", au.Require(api))
		mux.Handle("/auth/", au.Routes())
		mux.Handle("/", au.Require(ui.Handler()))
		srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- srv.ListenAndServe() }()
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.Shutdown(shutdown)
		}()
		slog.Info("hub listening", "addr", *listen, "db", *db, "ingest", *token != "", "auth", au.Mode())
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

func loopback(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
