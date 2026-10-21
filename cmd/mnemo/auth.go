package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/guipguia/mnemo/internal/auth"
)

// authFlags registers the UI/API login flags and returns a constructor to call after parsing.
func authFlags(fs *flag.FlagSet) func(context.Context) (*auth.Auth, error) {
	mode := fs.String("auth", "none", "UI/API login: none, oidc, or proxy")
	policy := fs.String("policy", "", "access policy file (required with --auth); see docs/auth.md")
	issuer := fs.String("oidc-issuer", "", "OIDC issuer URL")
	clientID := fs.String("oidc-client-id", "", "OIDC client ID")
	secret := fs.String("oidc-client-secret", os.Getenv("MNEMO_OIDC_CLIENT_SECRET"), "OIDC client secret (default $MNEMO_OIDC_CLIENT_SECRET)")
	redirect := fs.String("oidc-redirect-url", "", "OIDC redirect URL, e.g. https://mnemo.example.com/auth/callback")
	scopes := fs.String("oidc-scopes", "openid,email,profile", "OIDC scopes; add groups for Dex, Keycloak, Okta")
	userClaim := fs.String("oidc-username-claim", "email", "ID token claim used as the username")
	groupsClaim := fs.String("oidc-groups-claim", "groups", "ID token claim holding group names")
	ttl := fs.Duration("session-ttl", 8*time.Hour, "how long a login lasts")
	userHeader := fs.String("proxy-user-header", "X-Forwarded-User", "header carrying the username from the auth proxy")
	groupsHeader := fs.String("proxy-groups-header", "X-Forwarded-Groups", "header carrying comma-separated groups from the auth proxy")
	trusted := fs.String("proxy-trusted-cidrs", "", "comma-separated CIDRs the auth proxy connects from (required with --auth proxy)")

	return func(ctx context.Context) (*auth.Auth, error) {
		cfg := auth.Config{
			Mode: *mode, Issuer: *issuer, ClientID: *clientID, ClientSecret: *secret, RedirectURL: *redirect,
			Scopes: strings.Split(*scopes, ","), UsernameClaim: *userClaim, GroupsClaim: *groupsClaim,
			SessionKey: []byte(os.Getenv("MNEMO_SESSION_KEY")), SessionTTL: *ttl,
			UserHeader: *userHeader, GroupsHeader: *groupsHeader,
		}
		for _, c := range strings.Split(*trusted, ",") {
			if c = strings.TrimSpace(c); c == "" {
				continue
			}
			p, err := netip.ParsePrefix(c)
			if err != nil {
				return nil, fmt.Errorf("--proxy-trusted-cidrs: %w", err)
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, p)
		}
		if *policy != "" {
			var err error
			if cfg.Policy, err = auth.LoadPolicy(*policy); err != nil {
				return nil, err
			}
		}
		return auth.New(ctx, cfg)
	}
}
