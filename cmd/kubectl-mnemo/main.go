// Command kubectl-mnemo is the kubectl plugin: mnemo's history from the terminal.
//
//	kubectl mnemo changes --since 1h --drift
//	kubectl mnemo history deploy/api
//	kubectl mnemo get cm/app --at 2026-10-20T14:32:00Z
//	kubectl mnemo diff deploy/api --at 2h
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// version is set at release build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `usage: kubectl mnemo COMMAND [flags]

  changes              the change feed, newest first
  history KIND/NAME    every recorded version of an object
  get KIND/NAME        an object's YAML, now or --at a time
  diff KIND/NAME       how an object changed between --at and --to (default: now)
  version

Times are RFC 3339 or a duration ago: 2026-10-20T14:32:00Z, 2h. See docs/kubectl-plugin.md.
`

// errDiffers makes the exit code 1 when objects differ, like diff and kubectl diff; other
// errors exit 2, so scripts can tell drift from trouble.
var errDiffers = errors.New("objects differ")

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdout)
	if errors.Is(err, errDiffers) {
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

type headers []string

func (h *headers) String() string     { return strings.Join(*h, ", ") }
func (h *headers) Set(v string) error { *h = append(*h, v); return nil }

// options holds every flag; each command reads the ones that apply to it.
type options struct {
	hub, cluster, namespace, group, kind, author, source string
	since, until, at, to                                 string
	kubeconfig, context                                  string
	drift                                                bool
	limit                                                int
	headers                                              headers
	groupSet                                             bool
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, usage)
		return nil
	}
	cmd := args[0]
	if cmd == "version" {
		fmt.Fprintln(out, version)
		return nil
	}
	if cmd != "changes" && cmd != "history" && cmd != "get" && cmd != "diff" {
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}

	var o options
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&o.hub, "hub", os.Getenv("MNEMO_HUB"), "hub URL (default $MNEMO_HUB)")
	fs.StringVar(&o.cluster, "cluster", os.Getenv("MNEMO_CLUSTER"), "cluster name in mnemo (default $MNEMO_CLUSTER, then the kubeconfig context; changes: all)")
	fs.Var(&o.headers, "header", `extra request header, repeatable, e.g. "Cookie: mnemo_session=..." (also $MNEMO_HEADER)`)
	fs.StringVar(&o.namespace, "n", "", "namespace (default: the kubeconfig's; changes: all)")
	fs.StringVar(&o.namespace, "namespace", "", "same as -n")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig path")
	fs.StringVar(&o.context, "context", "", "kubeconfig context")
	fs.StringVar(&o.group, "group", "", "API group: take KIND literally, with no cluster access (e.g. --group apps Deployment/api)")
	fs.IntVar(&o.limit, "limit", 0, "maximum rows (default 100)")
	switch cmd {
	case "changes":
		fs.StringVar(&o.kind, "kind", "", "exact kind, e.g. ConfigMap")
		fs.StringVar(&o.author, "author", "", "exact author")
		fs.StringVar(&o.source, "source", "", "gitops, controller, manual, or unknown")
		fs.BoolVar(&o.drift, "drift", false, "only manual changes to GitOps-managed objects")
		fs.StringVar(&o.since, "since", "", "from this time")
		fs.StringVar(&o.until, "until", "", "up to this time")
	case "get":
		fs.StringVar(&o.at, "at", "", "the object at this time (default: now)")
	case "diff":
		fs.StringVar(&o.at, "at", "", "the older side (required)")
		fs.StringVar(&o.to, "to", "", "the newer side (default: now)")
	}
	// The flag package stops at the first positional, but "diff deploy/api --at 2h" is how
	// kubectl users type it.
	var pos []string
	for rest := args[1:]; ; rest = fs.Args()[1:] {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
	}
	clusterSet := false
	fs.Visit(func(f *flag.Flag) {
		o.groupSet = o.groupSet || f.Name == "group"
		clusterSet = clusterSet || f.Name == "cluster"
	})
	if cmd == "changes" && !clusterSet {
		o.cluster = "" // $MNEMO_CLUSTER names the object's cluster; the feed defaults to all
	}
	if o.hub == "" {
		return errors.New("--hub or $MNEMO_HUB is required")
	}
	if h := os.Getenv("MNEMO_HEADER"); h != "" {
		o.headers = append(headers{h}, o.headers...) // first, so --header wins
	}
	c := client{base: strings.TrimSuffix(o.hub, "/"), headers: o.headers}
	now := time.Now()

	if cmd == "changes" {
		if len(pos) != 0 {
			return errors.New("changes takes no arguments")
		}
		return changes(ctx, c, o, now, out)
	}
	if len(pos) != 1 {
		return fmt.Errorf("%s takes one KIND/NAME", cmd)
	}
	ref, err := resolve(pos[0], o)
	if err != nil {
		return err
	}
	switch cmd {
	case "history":
		return history(ctx, c, ref, o.limit, out)
	case "get":
		t, err := when(o.at, now)
		if err != nil {
			return err
		}
		obj, err := c.at(ctx, ref, t)
		if err != nil {
			return err
		}
		if obj == "" {
			return ref.missing(o.at)
		}
		_, err = io.WriteString(out, obj)
		return err
	default:
		if o.at == "" {
			return errors.New("diff needs --at")
		}
		return diff(ctx, c, ref, o, now, out)
	}
}

// ref names an object as the hub stores it.
type ref struct{ cluster, group, kind, namespace, name string }

func (r ref) String() string {
	s := r.kind + "/" + r.name
	if r.namespace != "" {
		s = r.namespace + "/" + s
	}
	return s
}

func (r ref) missing(at string) error {
	s := fmt.Sprintf("no %s in cluster %q", r, r.cluster)
	if at != "" {
		s += " at " + at
	}
	return errors.New(s + " (wrong --cluster? names come from the agent's --cluster flag)")
}

func (r ref) query() url.Values {
	return url.Values{"cluster": {r.cluster}, "group": {r.group}, "kind": {r.kind},
		"namespace": {r.namespace}, "name": {r.name}}
}

// resolve turns "deploy/api" into a ref the way kubectl does: discovery expands the short
// name, and the kubeconfig supplies the namespace and cluster. --group skips all of it, for
// a cluster that's down or a kind that's gone: often when you need history most.
func resolve(arg string, o options) (ref, error) {
	kind, name, ok := strings.Cut(arg, "/")
	if !ok || kind == "" || name == "" {
		return ref{}, fmt.Errorf("%q: want KIND/NAME, e.g. deploy/api", arg)
	}
	r := ref{cluster: o.cluster, group: o.group, kind: kind, namespace: o.namespace, name: name}
	if o.groupSet {
		if r.cluster == "" {
			return ref{}, errors.New("--group needs --cluster or $MNEMO_CLUSTER")
		}
		return r, nil
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = o.kubeconfig
	kc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: o.context})
	return resolveIn(r, o, kc, func() (meta.RESTMapper, error) {
		cfg, err := kc.ClientConfig()
		if err != nil {
			return nil, err
		}
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			return nil, err
		}
		groups, err := restmapper.GetAPIGroupResources(dc)
		if err != nil {
			return nil, fmt.Errorf("resolving %q needs the cluster (use --group to skip): %w", r.kind, err)
		}
		return restmapper.NewShortcutExpander(restmapper.NewDiscoveryRESTMapper(groups), dc, nil), nil
	})
}

// resolveIn fills in r's cluster, group, kind and namespace from the kubeconfig and the
// cluster's kinds. Split from resolve so tests can pass both in.
func resolveIn(r ref, o options, kc clientcmd.ClientConfig, newMapper func() (meta.RESTMapper, error)) (ref, error) {
	if r.cluster == "" {
		raw, err := kc.RawConfig()
		if err != nil {
			return ref{}, err
		}
		if r.cluster = raw.CurrentContext; o.context != "" {
			r.cluster = o.context
		}
		if r.cluster == "" {
			return ref{}, errors.New("--cluster, $MNEMO_CLUSTER, or a kubeconfig context is required")
		}
	}
	mapper, err := newMapper()
	if err != nil {
		return ref{}, err
	}
	// "deploy.apps" names the group too, as in kubectl.
	gvk, err := mapper.KindFor(schema.ParseGroupResource(strings.ToLower(r.kind)).WithVersion(""))
	if err != nil {
		return ref{}, fmt.Errorf("unknown kind %q (use --group to name a kind the cluster no longer has): %w", r.kind, err)
	}
	m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return ref{}, err
	}
	r.group, r.kind = gvk.Group, gvk.Kind
	if m.Scope.Name() != meta.RESTScopeNameNamespace {
		r.namespace = ""
	} else if r.namespace == "" {
		if r.namespace, _, err = kc.Namespace(); err != nil {
			return ref{}, err
		}
	}
	return r, nil
}

// when parses an RFC 3339 time or a duration ago. Empty is the zero time.
func when(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, fmt.Errorf("time %q: want RFC 3339 (2026-10-20T14:32:00Z) or a duration ago (2h)", s)
	}
	return t, nil
}

type change struct {
	ID        int64     `json:"id"`
	Cluster   string    `json:"cluster"`
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Author    string    `json:"author"`
	Source    string    `json:"source"`
	Drift     bool      `json:"drift"`
	Paths     []string  `json:"paths"`
}

func (c change) stamp() string { return c.Time.Local().Format(time.DateTime) }

func (c change) from() string {
	if c.Drift {
		return c.Source + ",drift"
	}
	return c.Source
}

func changes(ctx context.Context, c client, o options, now time.Time, out io.Writer) error {
	q := url.Values{}
	for k, v := range map[string]string{"cluster": o.cluster, "namespace": o.namespace, "kind": o.kind,
		"author": o.author, "source": o.source} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if o.drift {
		q.Set("drift", "true")
	}
	if o.limit > 0 {
		q.Set("limit", fmt.Sprint(o.limit))
	}
	for k, v := range map[string]string{"since": o.since, "until": o.until} {
		t, err := when(v, now)
		if err != nil {
			return err
		}
		if !t.IsZero() {
			q.Set(k, t.Format(time.RFC3339Nano))
		}
	}
	var list []change
	if err := c.get(ctx, "/api/v1/changes", q, &list); err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tCLUSTER\tOBJECT\tTYPE\tAUTHOR\tSOURCE")
	for _, ch := range list {
		obj := ref{kind: ch.Kind, namespace: ch.Namespace, name: ch.Name}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", ch.stamp(), ch.Cluster, obj, ch.Type, ch.Author, ch.from())
	}
	return w.Flush()
}

func history(ctx context.Context, c client, r ref, limit int, out io.Writer) error {
	q := r.query()
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var list []change
	if err := c.get(ctx, "/api/v1/history", q, &list); err != nil {
		return err
	}
	if len(list) == 0 {
		return r.missing("")
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tTYPE\tAUTHOR\tSOURCE\tCHANGED")
	for _, ch := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ch.stamp(), ch.Type, ch.Author, ch.from(), strings.Join(ch.Paths, " "))
	}
	return w.Flush()
}

// diff compares two points in time with the system diff, like kubectl diff. An object that
// didn't exist at one of them diffs against nothing.
func diff(ctx context.Context, c client, r ref, o options, now time.Time, out io.Writer) error {
	from, err := when(o.at, now)
	if err != nil {
		return err
	}
	to, err := when(o.to, now)
	if err != nil {
		return err
	}
	before, err := c.at(ctx, r, from)
	if err != nil {
		return err
	}
	after, err := c.at(ctx, r, to)
	if err != nil {
		return err
	}
	if before == "" && after == "" {
		return r.missing(o.at)
	}
	dir, err := os.MkdirTemp("", "kubectl-mnemo")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	a, b := filepath.Join(dir, "before"), filepath.Join(dir, "after")
	if err := os.WriteFile(a, []byte(before), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(b, []byte(after), 0o600); err != nil {
		return err
	}
	toLabel := o.to
	if toLabel == "" {
		toLabel = "now"
	}
	cmd := exec.CommandContext(ctx, "diff", "-u", "-L", r.String()+" at "+o.at, "-L", r.String()+" at "+toLabel, a, b)
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return errDiffers
	}
	return err
}

type client struct {
	base    string
	headers []string
}

var errNotFound = errors.New("not found")

// at returns the object's YAML at t (zero: now), or "" if it didn't exist then.
func (c client) at(ctx context.Context, r ref, t time.Time) (string, error) {
	q := r.query()
	if !t.IsZero() {
		q.Set("time", t.Format(time.RFC3339Nano))
	}
	var v struct {
		Object json.RawMessage `json:"object"`
	}
	if err := c.get(ctx, "/api/v1/at", q, &v); errors.Is(err, errNotFound) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	y, err := yaml.JSONToYAML(v.Object)
	return string(y), err
}

func (c client) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	for _, h := range c.headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			// Not the header itself: it's usually a credential.
			return fmt.Errorf("a header (%d characters) has no colon: want \"Name: value\"", len(h))
		}
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// A logged-out auth proxy redirects to its HTML login page, which answers 200.
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			return fmt.Errorf("hub: got %q from %s, not the mnemo API: not logged in (see --header), or wrong --hub", ct, resp.Request.URL.Redacted())
		}
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		return errNotFound
	case http.StatusUnauthorized:
		return errors.New("hub: not logged in; pass credentials with --header, see docs/kubectl-plugin.md")
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("hub: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}
