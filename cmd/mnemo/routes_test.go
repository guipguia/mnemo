package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guipguia/mnemo/internal/auth"
	"github.com/guipguia/mnemo/internal/store"
)

// Who can reach what. A wrong line in routes() would silently open the API, so every
// rule is checked against the real route table.
func TestRoutes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	policy := filepath.Join(t.TempDir(), "policy.yaml")
	os.WriteFile(policy, []byte("rules:\n- groups: [sre]\n"), 0o600)
	p, err := auth.LoadPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	// httptest requests come from 192.0.2.1, so that is the "auth proxy".
	proxy, err := auth.New(context.Background(), auth.Config{Mode: "proxy", Policy: p,
		UserHeader: "X-User", GroupsHeader: "X-Groups", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}})
	if err != nil {
		t.Fatal(err)
	}
	none, _ := auth.New(context.Background(), auth.Config{Mode: "none"})

	const batch = `{"cluster":"c","clusterUID":"u","items":[{"kind":"ConfigMap","namespace":"n","name":"x","time":"2026-10-21T14:32:00Z","object":{"data":{"a":"1"}}}]}`
	type req struct {
		method, path, token, user, groups string
	}
	do := func(h http.Handler, r req) (int, string) {
		var body *strings.Reader
		if r.method == "POST" {
			body = strings.NewReader(batch)
		} else {
			body = strings.NewReader("")
		}
		hr := httptest.NewRequest(r.method, r.path, body)
		if r.token != "" {
			hr.Header.Set("Authorization", "Bearer "+r.token)
		}
		if r.user != "" {
			hr.Header.Set("X-User", r.user)
			hr.Header.Set("X-Groups", r.groups)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, hr)
		return w.Code, strings.TrimSpace(w.Body.String())
	}

	sso := routes(st, "agent-token", proxy)
	for _, tc := range []struct {
		name string
		r    req
		want int
	}{
		{"health is open", req{"GET", "/healthz", "", "", ""}, 200},

		{"ingest: no credentials", req{"POST", "/api/v1/ingest", "", "", ""}, 401},
		{"ingest: a logged-in user is not an agent", req{"POST", "/api/v1/ingest", "", "amy", "sre"}, 401},
		{"ingest: wrong token", req{"POST", "/api/v1/ingest", "nope", "", ""}, 401},
		{"ingest: agent token, no SSO needed", req{"POST", "/api/v1/ingest", "agent-token", "", ""}, 200},
		{"checkpoints: agent token, no SSO needed", req{"GET", "/api/v1/checkpoints?cluster=c&uid=u", "agent-token", "", ""}, 200},
		{"checkpoints: a logged-in user is not an agent", req{"GET", "/api/v1/checkpoints?cluster=c&uid=u", "", "amy", "sre"}, 401},

		{"read API: no credentials", req{"GET", "/api/v1/changes", "", "", ""}, 401},
		{"read API: the agent token can't read history", req{"GET", "/api/v1/changes", "agent-token", "", ""}, 401},
		{"read API: logged-in user", req{"GET", "/api/v1/changes", "", "amy", "sre"}, 200},
		{"read API: a trailing-slash variant still needs login", req{"GET", "/api/v1/checkpoints/", "agent-token", "", ""}, 401},

		{"UI: no credentials", req{"GET", "/", "", "", ""}, 401},
		{"UI: the agent token is not a login", req{"GET", "/", "agent-token", "", ""}, 401},
		{"who am I: logged-in user", req{"GET", "/auth/me", "", "amy", "sre"}, 200},
	} {
		if code, body := do(sso, tc.r); code != tc.want {
			t.Errorf("%s: %s %s = %d, want %d (%s)", tc.name, tc.r.method, tc.r.path, code, tc.want, body)
		}
	}

	// The batch ingested above is visible to a user the policy grants, and not to others.
	if _, body := do(sso, req{"GET", "/api/v1/changes", "", "amy", "sre"}); !strings.Contains(body, `"name":"x"`) {
		t.Errorf("sre user should see the ingested change: %s", body)
	}
	if _, body := do(sso, req{"GET", "/api/v1/changes", "", "eve", "everyone"}); body != "[]" {
		t.Errorf("user outside the policy should see nothing: %s", body)
	}
	// The UI itself passes auth (it isn't built in tests, so the hint page is a 404).
	if code, _ := do(sso, req{"GET", "/", "", "amy", "sre"}); code == 401 {
		t.Error("logged-in user got 401 for the UI")
	}

	// --auth none still protects ingest with the token.
	open := routes(st, "agent-token", none)
	if code, _ := do(open, req{"GET", "/api/v1/changes", "", "", ""}); code != 200 {
		t.Errorf("auth none: read API = %d, want 200", code)
	}
	if code, _ := do(open, req{"POST", "/api/v1/ingest", "", "", ""}); code != 401 {
		t.Errorf("auth none: ingest without token = %d, want 401", code)
	}
	// Without a token (mode all, no remote agents), ingest doesn't exist at all.
	noToken := routes(st, "", none)
	if code, _ := do(noToken, req{"POST", "/api/v1/ingest", "", "", ""}); code != 404 {
		t.Errorf("no token: ingest = %d, want 404 (not registered)", code)
	}
}
