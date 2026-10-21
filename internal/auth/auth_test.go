package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePolicy(t *testing.T, yaml string) *Policy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	os.WriteFile(path, []byte(yaml), 0o600)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const testPolicy = `
rules:
- groups: [sre]
- groups: [team-shop]
  users: [bob@example.com]
  clusters: [prod]
  namespaces: ["shop*"]
`

func TestPolicy(t *testing.T) {
	p := writePolicy(t, testPolicy)
	for _, tc := range []struct {
		u           User
		cluster, ns string
		want        bool
	}{
		{User{"amy", []string{"sre"}}, "staging", "", true},
		{User{"alice", []string{"team-shop"}}, "prod", "shop-api", true},
		{User{"alice", []string{"team-shop"}}, "prod", "payments", false},
		{User{"alice", []string{"team-shop"}}, "staging", "shop", false},
		{User{"alice", []string{"team-shop"}}, "prod", "", false}, // cluster-scoped
		{User{"bob@example.com", nil}, "prod", "shop", true},
		{User{"eve", []string{"everyone"}}, "prod", "shop", false},
	} {
		if got := p.Access(tc.u).Allows(tc.cluster, tc.ns); got != tc.want {
			t.Errorf("%v on %s/%s: %v, want %v", tc.u, tc.cluster, tc.ns, got, tc.want)
		}
	}

	path := filepath.Join(t.TempDir(), "bad.yaml")
	for _, bad := range []string{
		"rules:\n- groups: [x]\n  namespace: [shop]\n", // typo must not silently widen access
		"rules:\n- clusters: [prod]\n",                 // nobody
		"rules:\n- groups: [x]\n  namespaces: ['s*p']\n",
	} {
		os.WriteFile(path, []byte(bad), 0o600)
		if _, err := LoadPolicy(path); err == nil {
			t.Errorf("policy %q loaded, want error", bad)
		}
	}
}

func TestProxy(t *testing.T) {
	a, err := New(t.Context(), Config{Mode: "proxy", Policy: writePolicy(t, testPolicy),
		UserHeader: "X-Forwarded-User", GroupsHeader: "X-Forwarded-Groups",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	h := a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, a.Access(r).Allows("prod", "shop"))
	}))
	call := func(remote, user, groups string) (int, string) {
		r := httptest.NewRequest("GET", "/api/v1/changes", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-User", user)
		r.Header.Set("X-Forwarded-Groups", groups)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := call("10.1.2.3:5000", "alice", "eng, team-shop"); code != 200 || body != "true" {
		t.Errorf("trusted proxy: %d %s", code, body)
	}
	if code, _ := call("192.168.1.9:5000", "alice", "sre"); code != 401 {
		t.Errorf("headers from an untrusted address must be ignored: %d", code)
	}
	if code, _ := call("10.1.2.3:5000", "", ""); code != 401 {
		t.Errorf("no user header: %d", code)
	}
	if _, err := New(t.Context(), Config{Mode: "proxy", Policy: &Policy{}}); err == nil {
		t.Error("proxy mode without trusted CIDRs must be refused")
	}
}

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint that
// checks PKCE and returns an RS256-signed ID token.
type fakeIdP struct {
	*httptest.Server
	key       *rsa.PrivateKey
	challenge string         // from the authorize URL
	claims    map[string]any // ID token claims to issue
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeIdP{key: key}
	b64 := base64.RawURLEncoding.EncodeToString
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"jwks_uri": f.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if b64(sum[:]) != f.challenge || r.Form.Get("code") != "good-code" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		payload, _ := json.Marshal(f.claims)
		signing := b64(header) + "." + b64(payload)
		h := sha256.Sum256([]byte(signing))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": signing + "." + b64(sig)})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestOIDCLogin(t *testing.T) {
	idp := newFakeIdP(t)
	a, err := New(t.Context(), Config{Mode: "oidc", Policy: writePolicy(t, testPolicy),
		Issuer: idp.URL, ClientID: "mnemo", ClientSecret: "s", RedirectURL: "http://mnemo.test/auth/callback",
		Scopes: []string{"openid", "email", "groups"}, UsernameClaim: "email", GroupsClaim: "groups",
		SessionKey: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	app := http.NewServeMux()
	app.Handle("/auth/", a.Routes())
	app.Handle("/", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, a.Access(r).Allows("prod", "shop"))
	})))
	do := func(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	cookie := func(w *httptest.ResponseRecorder, name string) *http.Cookie {
		for _, c := range w.Result().Cookies() {
			if c.Name == name && c.MaxAge > 0 {
				return c
			}
		}
		return nil
	}

	// login starts the round trip; returns the login cookie, the IdP's view of the request, and the state.
	login := func(rd string, claims map[string]any) (*http.Cookie, string) {
		w := do("/auth/login?rd=" + url.QueryEscape(rd))
		authz, _ := url.Parse(w.Header().Get("Location"))
		q := authz.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
			t.Fatalf("authorize URL lacks PKCE or nonce: %s", authz)
		}
		idp.challenge = q.Get("code_challenge")
		if _, set := claims["nonce"]; !set {
			claims["nonce"] = q.Get("nonce")
		}
		idp.claims = claims
		return cookie(w, loginCookie), q.Get("state")
	}
	claims := func(extra map[string]any) map[string]any {
		c := map[string]any{"iss": idp.URL, "aud": "mnemo", "sub": "123", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(), "email": "alice@example.com", "email_verified": true,
			"groups": []string{"team-shop", "all-staff", "some-other-group"}}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}

	// Unauthenticated browser → login; API → 401.
	if w := do("/?cluster=prod"); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/auth/login?rd=") {
		t.Fatalf("browser without session: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := do("/auth/me"); w.Code != 401 {
		t.Fatalf("/auth/me without session: %d", w.Code)
	}

	// Happy path.
	lc, state := login("/?cluster=prod", claims(nil))
	w := do("/auth/callback?code=good-code&state="+state, lc)
	session := cookie(w, sessionCookie)
	if w.Code != 302 || w.Header().Get("Location") != "/?cluster=prod" || session == nil || !session.HttpOnly {
		t.Fatalf("callback: %d %s %+v\n%s", w.Code, w.Header().Get("Location"), session, w.Body)
	}
	if w := do("/", session); w.Body.String() != "true" {
		t.Fatalf("logged-in access: %d %s", w.Code, w.Body)
	}
	var me struct{ User User }
	json.NewDecoder(do("/auth/me", session).Body).Decode(&me)
	if me.User.Name != "alice@example.com" || strings.Join(me.User.Groups, ",") != "team-shop" {
		t.Errorf("me = %+v; groups not in the policy must be dropped", me.User)
	}

	// Tampering with the session is detected.
	forged := *session
	forged.Value = strings.Replace(forged.Value, ".", "x.", 1)
	if w := do("/auth/me", &forged); w.Code != 401 {
		t.Errorf("tampered session: %d", w.Code)
	}
	// A login cookie is not a session.
	lc2, _ := login("/", claims(nil))
	if w := do("/auth/me", &http.Cookie{Name: sessionCookie, Value: lc2.Value}); w.Code != 401 {
		t.Errorf("login cookie replayed as session: %d", w.Code)
	}

	for name, tc := range map[string]struct {
		rd, code, state string
		claims          map[string]any
		want            int
	}{
		"wrong state":              {"/", "good-code", "nope", claims(nil), 400},
		"wrong code":               {"/", "bad-code", "", claims(nil), 401},
		"token from another login": {"/", "good-code", "", claims(map[string]any{"nonce": "replayed"}), 401},
		"wrong audience":           {"/", "good-code", "", claims(map[string]any{"aud": "someone-else"}), 401},
		"expired token":            {"/", "good-code", "", claims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), 401},
		"unverified email":         {"/", "good-code", "", claims(map[string]any{"email_verified": false}), 403},
		"no username claim":        {"/", "good-code", "", claims(map[string]any{"email": nil}), 403},
		"open redirect is cut":     {"//evil.example/x", "good-code", "", claims(nil), 302},
	} {
		lc, state := login(tc.rd, tc.claims)
		if tc.state != "" {
			state = tc.state
		}
		w := do("/auth/callback?code="+tc.code+"&state="+state, lc)
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d: %s", name, w.Code, tc.want, w.Body)
		}
		if tc.want == 302 && w.Header().Get("Location") != "/" {
			t.Errorf("%s: redirected to %q, want /", name, w.Header().Get("Location"))
		}
		if tc.want != 302 && cookie(w, sessionCookie) != nil {
			t.Errorf("%s: session issued on failure", name)
		}
	}

	// No policy, no start.
	if _, err := New(t.Context(), Config{Mode: "oidc", Issuer: idp.URL, ClientID: "c", RedirectURL: "x"}); err == nil {
		t.Error("oidc without a policy must be refused")
	}
}
