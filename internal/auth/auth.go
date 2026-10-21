// Package auth identifies UI and API users (OIDC login or a trusted auth proxy) and
// turns the policy into what each user may see. Agents don't go through here: they
// use the hub token.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/guipguia/mnemo/internal/store"
)

type User struct {
	Name   string   `json:"name"`
	Groups []string `json:"groups,omitempty"`
}

type Config struct {
	Mode   string // none | oidc | proxy
	Policy *Policy

	// oidc
	Issuer, ClientID, ClientSecret string
	RedirectURL                    string // e.g. https://mnemo.example.com/auth/callback
	Scopes                         []string
	UsernameClaim, GroupsClaim     string
	SessionKey                     []byte // signs cookies; random per process if empty
	SessionTTL                     time.Duration

	// proxy
	UserHeader, GroupsHeader string
	TrustedProxies           []netip.Prefix
}

type Auth struct {
	cfg    Config
	oidc   *oidcLogin
	secure bool // set the Secure flag on cookies
}

func New(ctx context.Context, cfg Config) (*Auth, error) {
	a := &Auth{cfg: cfg}
	if cfg.Mode != "none" && cfg.Policy == nil {
		return nil, errors.New("--policy is required with --auth: without it nobody could see anything")
	}
	switch cfg.Mode {
	case "none":
	case "proxy":
		if len(cfg.TrustedProxies) == 0 {
			return nil, errors.New("--proxy-trusted-cidrs is required: otherwise anyone reaching the hub could claim any identity")
		}
	case "oidc":
		if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
			return nil, errors.New("oidc needs --oidc-issuer, --oidc-client-id and --oidc-redirect-url")
		}
		if len(cfg.SessionKey) == 0 {
			slog.Warn("MNEMO_SESSION_KEY not set: using a random key, so users must log in again after every restart")
			cfg.SessionKey = []byte(rand.Text() + rand.Text())
		} else if len(cfg.SessionKey) < 32 {
			return nil, errors.New("MNEMO_SESSION_KEY must be at least 32 bytes")
		}
		if cfg.SessionTTL <= 0 {
			cfg.SessionTTL = 8 * time.Hour
		}
		a.cfg = cfg
		a.secure = strings.HasPrefix(cfg.RedirectURL, "https://")
		var err error
		if a.oidc, err = newOIDC(ctx, cfg); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown --auth %q: want none, oidc, or proxy", cfg.Mode)
	}
	return a, nil
}

func (a *Auth) Mode() string { return a.cfg.Mode }

type ctxKey struct{}

// Require authenticates the request. Browsers without a session are sent to log in;
// API calls get 401 so the UI can do the redirect itself.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.authenticate(r)
		if err != nil {
			if a.cfg.Mode == "oidc" && !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/auth/me" {
				http.Redirect(w, r, "/auth/login?rd="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func (a *Auth) authenticate(r *http.Request) (User, error) {
	switch a.cfg.Mode {
	case "proxy":
		return a.fromProxy(r)
	case "oidc":
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			return User{}, errors.New("not logged in")
		}
		var u User
		if err := a.open("session", c.Value, &u); err != nil {
			return User{}, err
		}
		return u, nil
	default:
		return User{}, nil
	}
}

// Access is what the authenticated user of r may see.
func (a *Auth) Access(r *http.Request) store.Access {
	if a.cfg.Mode == "none" {
		return store.Access{All: true}
	}
	u, ok := r.Context().Value(ctxKey{}).(User)
	if !ok {
		return store.Access{}
	}
	return a.cfg.Policy.Access(u)
}

// Routes serves /auth/*: who am I, plus login, callback and logout for OIDC.
func (a *Auth) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /auth/me", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := r.Context().Value(ctxKey{}).(User)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"mode": a.cfg.Mode, "user": u, "access": a.Access(r)})
	})))
	if a.oidc != nil {
		mux.HandleFunc("GET /auth/login", a.login)
		mux.HandleFunc("GET /auth/callback", a.callback)
		mux.HandleFunc("GET /auth/logout", a.logout)
	}
	return mux
}

func (a *Auth) fromProxy(r *http.Request) (User, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return User{}, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return User{}, err
	}
	trusted := false
	for _, p := range a.cfg.TrustedProxies {
		trusted = trusted || p.Contains(ip.Unmap())
	}
	if !trusted {
		return User{}, fmt.Errorf("request from %s did not come through a trusted proxy", ip)
	}
	u := User{Name: strings.TrimSpace(r.Header.Get(a.cfg.UserHeader))}
	if u.Name == "" {
		return User{}, fmt.Errorf("proxy sent no %s header", a.cfg.UserHeader)
	}
	for _, g := range strings.Split(r.Header.Get(a.cfg.GroupsHeader), ",") {
		if g = strings.TrimSpace(g); g != "" {
			u.Groups = append(u.Groups, g)
		}
	}
	return u, nil
}

const (
	sessionCookie = "mnemo_session"
	loginCookie   = "mnemo_login"
)

// envelope is what signed cookies carry. Purpose stops a cookie minted for one use
// (the login round trip) from being replayed as another (a session).
type envelope struct {
	Purpose string          `json:"p"`
	Expires int64           `json:"e"`
	Data    json.RawMessage `json:"d"`
}

func (a *Auth) seal(purpose string, v any, ttl time.Duration) string {
	data, _ := json.Marshal(v)
	payload, _ := json.Marshal(envelope{purpose, time.Now().Add(ttl).Unix(), data})
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + base64.RawURLEncoding.EncodeToString(a.mac(p))
}

func (a *Auth) open(purpose, s string, v any) error {
	p, sig, ok := strings.Cut(s, ".")
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !hmac.Equal(got, a.mac(p)) {
		return errors.New("invalid session")
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return errors.New("invalid session")
	}
	var e envelope
	if err := json.Unmarshal(payload, &e); err != nil || e.Purpose != purpose {
		return errors.New("invalid session")
	}
	if time.Now().Unix() > e.Expires {
		return errors.New("session expired")
	}
	return json.Unmarshal(e.Data, v)
}

func (a *Auth) mac(s string) []byte {
	h := hmac.New(sha256.New, a.cfg.SessionKey)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}
