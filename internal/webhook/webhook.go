// Package webhook posts recorded changes to outside receivers, e.g. "tell Slack when any
// ClusterRoleBinding changes in prod".
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/guipguia/mnemo/internal/store"
)

// Config is the webhooks file. It holds secrets: mount it from a Secret.
//
//	url: https://mnemo.example.com     # optional: link each change to the UI
//	webhooks:
//	- name: prod-rbac                    # keys the delivery cursor: renaming starts over
//	  url: https://hooks.slack.com/services/...
//	  secret: s3cret                     # optional: sign with HMAC-SHA256
//	  delay: 35s                         # optional: wait for audit attribution first
//	  match:
//	    clusters: [prod]
//	    kinds: [ClusterRoleBinding, ClusterRole]
type Config struct {
	URL      string    `json:"url,omitempty"`
	Webhooks []Webhook `json:"webhooks"`
}

type Webhook struct {
	Name   string          `json:"name"`
	URL    string          `json:"url"`
	Secret string          `json:"secret,omitempty"`
	Delay  metav1.Duration `json:"delay,omitempty"`
	Match  Match           `json:"match"`
}

// Match selects changes. Empty fields match everything; clusters and namespaces take
// the same patterns as the access policy. Baselines (objects found at startup) never match.
type Match struct {
	store.Scope `json:",inline"`
	Kinds       []string     `json:"kinds,omitempty"`
	Names       []string     `json:"names,omitempty"`
	Types       []store.Type `json:"types,omitempty"`   // added, modified, deleted
	Sources     []string     `json:"sources,omitempty"` // gitops, controller, manual, unknown
	DriftOnly   bool         `json:"driftOnly,omitempty"`
}

func (m Match) matches(c store.Change) bool {
	in := func(list []string, v string) bool { return len(list) == 0 || slices.Contains(list, v) }
	return c.Type != store.Baseline &&
		(store.Access{Scopes: []store.Scope{m.Scope}}).Allows(c.Cluster, c.Namespace) &&
		in(m.Kinds, c.Kind) && in(m.Names, c.Name) && in(m.Sources, c.Source) &&
		(len(m.Types) == 0 || slices.Contains(m.Types, c.Type)) && (!m.DriftOnly || c.Drift)
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	// Strict: a typo like "kind:" must fail loudly, not silently match everything.
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("webhooks %s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, w := range c.Webhooks {
		u, err := url.Parse(w.URL)
		switch {
		case w.Name == "" || seen[w.Name]:
			return nil, fmt.Errorf("webhooks %s: webhook %d: name must be set and unique", path, i+1)
		case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "":
			return nil, fmt.Errorf("webhooks %s: %s: url must be http(s)://host/...", path, w.Name)
		}
		for _, p := range slices.Concat(w.Match.Clusters, w.Match.Namespaces) {
			if p == "" || strings.Contains(strings.TrimSuffix(p, "*"), "*") {
				return nil, fmt.Errorf("webhooks %s: %s: pattern %q: want a name, \"*\", or \"prefix*\"", path, w.Name, p)
			}
		}
		for _, t := range w.Match.Types {
			if t != store.Added && t != store.Modified && t != store.Deleted {
				return nil, fmt.Errorf("webhooks %s: %s: type %q: want added, modified, or deleted", path, w.Name, t)
			}
		}
		seen[w.Name] = true
	}
	return &c, nil
}

// Payload is the JSON body: the change (without the object), a link to it, and a line of
// text for chat receivers that read a "text" field, like Slack. Receivers dedupe on id.
type Payload struct {
	store.Change
	Link string `json:"link,omitempty"`
	Text string `json:"text"`
}

// Run delivers to every webhook until ctx is done.
func Run(ctx context.Context, st *store.Store, c *Config) {
	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	for _, w := range c.Webhooks {
		wg.Go(func() { w.run(ctx, st, strings.TrimSuffix(c.URL, "/"), client) })
	}
	wg.Wait()
}

// poll is how often a caught-up webhook checks for new changes.
var poll = 2 * time.Second

// run walks the change log in ID order from the webhook's cursor, so delivery survives
// restarts and never slows ingest. A change is retried until the receiver accepts it, and
// later ones wait: at-least-once, in order.
func (w Webhook) run(ctx context.Context, st *store.Store, base string, client *http.Client) {
	log := slog.With("webhook", w.Name)
	cursor, ok, err := st.WebhookCursor(ctx, w.Name)
	if err == nil && !ok {
		// New webhook: start from now, not from the beginning of history.
		if cursor, err = st.LastID(ctx); err == nil {
			err = st.SetWebhookCursor(ctx, w.Name, cursor)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Error("webhook stopped", "err", err)
		}
		return
	}
	// With Delay, deliver only changes recorded at least Delay ago, so a trailing audit
	// event has named the user. marks remembers the last ID at each poll.
	type mark struct {
		t  time.Time
		id int64
	}
	var marks []mark
	upto := cursor
	for ctx.Err() == nil {
		last, err := st.LastID(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("read changes", "err", err)
			}
			sleepCtx(ctx, poll)
			continue
		}
		marks = append(marks, mark{time.Now(), last})
		for len(marks) > 0 && time.Since(marks[0].t) >= w.Delay.Duration {
			upto, marks = marks[0].id, marks[1:]
		}
		changes, err := st.After(ctx, cursor, upto, 100)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("read changes", "err", err)
			}
			sleepCtx(ctx, poll)
			continue
		}
		for _, c := range changes {
			if w.Match.matches(c) && !w.deliver(ctx, client, base, c, log) {
				return // ctx done
			}
			cursor = c.ID
		}
		if len(changes) > 0 {
			if err := st.SetWebhookCursor(ctx, w.Name, cursor); err != nil {
				log.Error("save cursor", "err", err) // harmless: those changes are resent
			}
		}
		if len(changes) < 100 {
			sleepCtx(ctx, poll)
		}
	}
}

// deliver posts c until the receiver answers 2xx, and reports false if ctx ended first.
func (w Webhook) deliver(ctx context.Context, client *http.Client, base string, c store.Change, log *slog.Logger) bool {
	p := Payload{Change: c, Text: text(c)}
	if base != "" {
		q := url.Values{"cluster": {c.Cluster}, "group": {c.Group}, "kind": {c.Kind},
			"namespace": {c.Namespace}, "name": {c.Name}, "change": {strconv.FormatInt(c.ID, 10)}}
		p.Link = base + "/?" + q.Encode()
		p.Text += " " + p.Link
	}
	body, _ := json.Marshal(p)
	for backoff := time.Second; ctx.Err() == nil; backoff = min(2*backoff, 5*time.Minute) {
		err := w.post(ctx, client, body, c.ID)
		if err == nil {
			return true
		}
		log.Warn("delivery failed, retrying", "change", c.ID, "err", err, "in", backoff)
		sleepCtx(ctx, backoff)
	}
	return false
}

func (w Webhook) post(ctx context.Context, client *http.Client, body []byte, id int64) error {
	req, err := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mnemo-Change", strconv.FormatInt(id, 10))
	if w.Secret != "" {
		req.Header.Set("X-Mnemo-Signature", "sha256="+Sign(w.Secret, body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// Sign is the hex HMAC-SHA256 of body: receivers compare it to X-Mnemo-Signature
// (after "sha256=") in constant time.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// text is one line, e.g. "prod: ClusterRoleBinding admin modified by alice (manual, drift)".
func text(c store.Change) string {
	obj := c.Name
	if c.Namespace != "" {
		obj = c.Namespace + "/" + c.Name
	}
	s := fmt.Sprintf("%s: %s %s %s", c.Cluster, c.Kind, obj, c.Type)
	if c.Author != "" {
		s += " by " + c.Author
	}
	s += " (" + c.Source
	if c.Drift {
		s += ", drift"
	}
	return s + ")"
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
