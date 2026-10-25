package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/guipguia/mnemo/internal/hub"
)

// auditEvent is the part of an audit.k8s.io/v1 Event the agent reads.
type auditEvent struct {
	Stage     string `json:"stage"`
	Verb      string `json:"verb"`
	UserAgent string `json:"userAgent"`
	User      struct {
		Username string `json:"username"`
	} `json:"user"`
	// Set when an access proxy (Teleport, Rancher, ...) or kubectl --as acts for someone.
	ImpersonatedUser *struct {
		Username string `json:"username"`
	} `json:"impersonatedUser"`
	ObjectRef *struct {
		APIGroup    string `json:"apiGroup"`
		Resource    string `json:"resource"`
		Subresource string `json:"subresource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
	} `json:"objectRef"`
	ResponseStatus *struct {
		Code int `json:"code"`
	} `json:"responseStatus"`
	// Present at the RequestResponse audit level only: the written object, or for most
	// deletes a Status whose details name the deleted object's UID.
	ResponseObject *struct {
		Metadata struct {
			Name            string `json:"name"`
			Namespace       string `json:"namespace"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Details *struct {
			UID string `json:"uid"`
		} `json:"details"`
	} `json:"responseObject"`
}

var writeVerbs = map[string]bool{"create": true, "update": true, "patch": true, "delete": true}

// audits keeps the successful writes to watched resources whose response names the
// resourceVersion they produced (the version the watch records for that write), or for a
// delete, the UID of the deleted object.
func (a *Agent) audits(events []auditEvent) []hub.Audit {
	watched := map[string]bool{}
	for _, gvr := range a.Resources {
		watched[gvr.Group+"/"+gvr.Resource] = true
	}
	var out []hub.Audit
	for _, e := range events {
		ref, resp := e.ObjectRef, e.ResponseObject
		if e.Stage != "ResponseComplete" || !writeVerbs[e.Verb] || ref == nil || resp == nil ||
			ref.Subresource == "status" || !watched[ref.APIGroup+"/"+ref.Resource] ||
			e.ResponseStatus == nil || e.ResponseStatus.Code >= 300 {
			continue
		}
		var uid string
		if e.Verb == "delete" && resp.Details != nil {
			uid = resp.Details.UID
		}
		if resp.Metadata.ResourceVersion == "" && uid == "" {
			continue
		}
		user := e.User.Username
		if e.ImpersonatedUser != nil {
			user = e.ImpersonatedUser.Username // the person, not the proxy acting for them
		}
		ns, name := resp.Metadata.Namespace, resp.Metadata.Name
		if name == "" { // a Status: the request names the object
			ns, name = ref.Namespace, ref.Name
		}
		out = append(out, hub.Audit{Namespace: ns, Name: name, ResourceVersion: resp.Metadata.ResourceVersion,
			UID: uid, User: user, UserAgent: e.UserAgent})
	}
	return out
}

// serveAudit receives the API server's audit webhook (an EventList per request) and
// forwards who made each write to the hub.
func (a *Agent) serveAudit(ctx context.Context, l net.Listener) {
	h := hub.RequireToken(a.AuditToken, func(w http.ResponseWriter, r *http.Request) {
		var list struct {
			Items []auditEvent `json:"items"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&list); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b := hub.Batch{Cluster: a.Cluster, ClusterUID: a.uid, Audits: a.audits(list.Items)}
		if len(b.Audits) == 0 {
			return
		}
		// No retry loop: the API server retries the batch itself, then drops it, and the
		// field manager stays the author.
		if err := a.Push(r.Context(), b); err != nil {
			slog.Warn("forwarding audit events failed", "err", err, "events", len(b.Audits))
			http.Error(w, "hub unavailable", http.StatusServiceUnavailable)
		}
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ServeTLS(l, a.AuditCert, a.AuditKey); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("audit webhook server stopped", "err", err)
	}
}
