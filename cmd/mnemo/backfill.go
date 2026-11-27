package main

import (
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/guipguia/mnemo/internal/agent"
	"github.com/guipguia/mnemo/internal/hub"
)

// backfill records history from existing audit logs, before the cluster's first agent run.
// See docs/audit.md#backfill.
func backfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: mnemo backfill --cluster NAME --hub URL [flags] AUDIT-LOG... (.gz, or - for stdin)")
		fs.PrintDefaults()
	}
	cluster := fs.String("cluster", "", "cluster name, as the agent will use it (required)")
	hubURL := fs.String("hub", "", "hub URL (required)")
	token := fs.String("token", "", "shared agent/hub token (default $MNEMO_TOKEN)")
	uid := fs.String("cluster-uid", "", "the cluster's kube-system namespace UID (default: read it with --kubeconfig)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig to read the cluster UID with (default: ~/.kube/config rules)")
	kubeContext := fs.String("context", "", "kubeconfig context (default: the current one)")
	resources := fs.String("resources", strings.Join(agent.DefaultResources, ","), "comma-separated group/version/resource list, as the agent's")
	hubCA := fs.String("hub-ca", "", "CA to trust for the hub's certificate (default: the system's)")
	clientCert := fs.String("client-cert", "", "client certificate for a hub with --agent-listen; must name --cluster")
	clientKey := fs.String("client-key", "", "client key for --client-cert")
	fs.Parse(args)
	fromEnv(token, "MNEMO_TOKEN", "")
	for _, a := range fs.Args() {
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			return fmt.Errorf("%s: flags go before the audit logs", a)
		}
	}
	if *cluster == "" || *hubURL == "" || *token == "" || fs.NArg() == 0 {
		fs.Usage()
		return errors.New("--cluster, --hub, --token and at least one audit log are required")
	}

	var gvrs []schema.GroupVersionResource
	for _, r := range strings.Split(*resources, ",") {
		gvr, err := agent.ParseResource(strings.TrimSpace(r))
		if err != nil {
			return err
		}
		gvrs = append(gvrs, gvr)
	}
	if *uid == "" {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = *kubeconfig
		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: *kubeContext})
		cfg, err := cc.ClientConfig()
		if err != nil {
			return fmt.Errorf("reading the cluster UID needs cluster access (or pass --cluster-uid): %w", err)
		}
		client, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return err
		}
		ns, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, "kube-system", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read cluster identity (kube-system UID): %w", err)
		}
		*uid = string(ns.GetUID())
		raw, _ := cc.RawConfig()
		name := cmp.Or(*kubeContext, raw.CurrentContext)
		// A wrong context would tie --cluster to another cluster's identity: say which one.
		slog.Info("cluster identity", "context", name, "server", cfg.Host, "uid", *uid)
	}
	tc, err := hub.ClientTLS(*hubCA, *clientCert, *clientKey)
	if err != nil {
		return err
	}
	c := hub.NewClient(strings.TrimSuffix(*hubURL, "/"), *token, tc)
	b := &agent.Backfill{Cluster: *cluster, ClusterUID: *uid, Resources: gvrs, Push: c.Push}

	for _, path := range fs.Args() {
		n, err := readLog(b, path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		slog.Info("read audit log", "file", path, "writes", n)
	}
	n, err := b.Run(ctx)
	if n == 0 && err == nil {
		slog.Warn("no writes to send: the logs need the RequestResponse audit level for the watched resources (see docs/audit.md)")
	}
	slog.Info("backfill", "cluster", *cluster, "sent", n)
	return err
}

func readLog(b *agent.Backfill, path string) (int, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		r = f
		if strings.HasSuffix(path, ".gz") {
			gz, err := gzip.NewReader(f)
			if err != nil {
				return 0, err
			}
			defer gz.Close()
			r = gz
		}
	}
	return b.Read(r)
}
