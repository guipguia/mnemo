package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type oidcLogin struct {
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

func newOIDC(ctx context.Context, cfg Config) (*oidcLogin, error) {
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", cfg.Issuer, err)
	}
	return &oidcLogin{
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: cfg.Scopes},
	}, nil
}

// loginState survives the round trip to the IdP in a signed cookie.
type loginState struct {
	State, Nonce, Verifier, Return string
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	st := loginState{State: rand.Text(), Nonce: rand.Text(), Verifier: oauth2.GenerateVerifier(),
		Return: localPath(r.URL.Query().Get("rd"))}
	a.setCookie(w, loginCookie, a.seal("login", st, 10*time.Minute), 10*time.Minute)
	http.Redirect(w, r, a.oidc.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "identity provider refused the login: "+e+" "+q.Get("error_description"), http.StatusForbidden)
		return
	}
	var st loginState
	c, err := r.Cookie(loginCookie)
	if err != nil || a.open("login", c.Value, &st) != nil {
		http.Error(w, "login expired or started elsewhere; open mnemo again", http.StatusBadRequest)
		return
	}
	a.setCookie(w, loginCookie, "", -1)
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		http.Error(w, "login state mismatch", http.StatusBadRequest)
		return
	}

	tok, err := a.oidc.oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		http.Error(w, "code exchange failed: "+err.Error(), http.StatusUnauthorized)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.oidc.verifier.Verify(r.Context(), raw)
	if err != nil {
		http.Error(w, "invalid ID token: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		http.Error(w, "ID token nonce mismatch", http.StatusUnauthorized)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	u, err := a.userFromClaims(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	a.setCookie(w, sessionCookie, a.seal("session", u, a.cfg.SessionTTL), a.cfg.SessionTTL)
	slog.Info("login", "user", u.Name, "groups", u.Groups)
	http.Redirect(w, r, st.Return, http.StatusFound)
}

func (a *Auth) userFromClaims(claims map[string]any) (User, error) {
	name, _ := claims[a.cfg.UsernameClaim].(string)
	if name == "" {
		return User{}, fmt.Errorf("ID token has no %q claim; set --oidc-username-claim", a.cfg.UsernameClaim)
	}
	// Some IdPs let users set an unverified email; never treat one as an identity.
	if a.cfg.UsernameClaim == "email" && claims["email_verified"] == false {
		return User{}, fmt.Errorf("email %s is not verified by the identity provider", name)
	}
	var groups []string
	if gs, ok := claims[a.cfg.GroupsClaim].([]any); ok {
		for _, g := range gs {
			if g, ok := g.(string); ok {
				groups = append(groups, g)
			}
		}
	}
	return User{Name: name, Groups: a.cfg.Policy.relevant(groups)}, nil
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	a.setCookie(w, sessionCookie, "", -1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><title>mnemo</title><p style="font-family:system-ui;margin:3em">Signed out of mnemo. <a href="/auth/login">Sign in again</a></p>`)
}

// localPath only allows same-site paths after login, never "//evil.example" or "https://...".
func localPath(rd string) string {
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.HasPrefix(rd, `/\`) {
		return "/"
	}
	return rd
}
