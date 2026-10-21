package main

import (
	"fmt"
	"net/http"

	"github.com/guipguia/mnemo/internal/auth"
	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
	"github.com/guipguia/mnemo/ui"
)

// routes is the hub's whole HTTP surface and who may reach each part. routes_test.go
// checks every rule; change them together.
func routes(st *store.Store, token string, au *auth.Auth) http.Handler {
	api := hub.Handler(st, token, au.Access)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/ingest", api) // agents: hub token, not SSO
	mux.Handle("/api/v1/checkpoints", api)
	mux.Handle("/api/", au.Require(api))
	mux.Handle("/auth/", au.Routes())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Ping(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", au.Require(ui.Handler()))
	return mux
}
