package hub

import (
	"encoding/json"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guipguia/mnemo/internal/store"
)

// ponytail: hardcoded manager lists; make them configurable when someone runs a tool we misclassify.
var gitopsManagers = map[string]bool{
	"argocd-controller":             true,
	"argocd-application-controller": true,
	"kustomize-controller":          true,
	"helm-controller":               true,
}

// Labels/annotations that mean "this object is owned by a GitOps tool".
var gitopsMarkers = []string{
	"argocd.argoproj.io/instance",
	"argocd.argoproj.io/tracking-id",
	"kustomize.toolkit.fluxcd.io/name",
	"helm.toolkit.fluxcd.io/name",
}

// classify maps a field manager to a change source.
func classify(manager string) string {
	switch {
	case manager == "":
		return "unknown"
	case gitopsManagers[manager]:
		return "gitops"
	// Humans: kubectl and its subcommands, the helm CLI, browser-based dashboards.
	case strings.HasPrefix(manager, "kubectl"), manager == "helm", strings.HasPrefix(manager, "Mozilla"):
		return "manual"
	default:
		return "controller"
	}
}

// attribute sets c.Author, c.Source and c.Drift. A writer takes ownership of the fields it
// changes, so the author of a modification is whoever owns the changed paths. Timestamps
// alone can't do this: /scale entries have none, and they only have 1s precision.
func attribute(c *store.Change, managers []metav1.ManagedFieldsEntry) {
	if c.Object == nil {
		c.Author, c.Source = "", "unknown" // a delete doesn't update managedFields
		return
	}
	if c.Type == store.Modified {
		c.Author = owner(c.Paths, managers)
	}
	if c.Author == "" { // created, baseline, or only removed fields (nobody owns them now)
		c.Author = newest(managers)
	}
	c.Source = classify(c.Author)
	c.Drift = c.Source == "manual" && gitopsManaged(c.Object, managers)
}

// owner returns the manager owning the most changed paths, the newest on ties.
func owner(paths []string, managers []metav1.ManagedFieldsEntry) string {
	best, bestN := -1, 0
	for i, m := range managers {
		if m.Subresource == "status" || m.FieldsV1 == nil {
			continue
		}
		var tree map[string]any
		if json.Unmarshal(m.FieldsV1.Raw, &tree) != nil {
			continue
		}
		n := 0
		for _, p := range paths {
			if owns(tree, splitPointer(p)) {
				n++
			}
		}
		if n > bestN || (n == bestN && n > 0 && newer(m, managers[best])) {
			best, bestN = i, n
		}
	}
	if best < 0 {
		return ""
	}
	return managers[best].Manager
}

// owns reports whether a fieldsV1 tree covers the path. List items are keyed by value
// (k:{"name":"nginx"}), not index, so an index segment matches any item.
func owns(tree map[string]any, segs []string) bool {
	if len(segs) == 0 {
		return true
	}
	if len(tree) == 0 {
		return true // leaf: an atomic field owned as a whole
	}
	if child, ok := tree["f:"+segs[0]].(map[string]any); ok {
		return owns(child, segs[1:])
	}
	if _, err := strconv.Atoi(segs[0]); err == nil {
		for k, child := range tree {
			if child, ok := child.(map[string]any); ok && (strings.HasPrefix(k, "k:") || strings.HasPrefix(k, "v:") || strings.HasPrefix(k, "i:")) && owns(child, segs[1:]) {
				return true
			}
		}
	}
	return false
}

func newest(managers []metav1.ManagedFieldsEntry) string {
	best := -1
	for i, m := range managers {
		if m.Subresource != "status" && (best < 0 || newer(m, managers[best])) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return managers[best].Manager
}

// newer orders entries by time; entries without one (e.g. /scale) sort oldest.
func newer(a, b metav1.ManagedFieldsEntry) bool {
	if a.Time == nil {
		return false
	}
	return b.Time == nil || !a.Time.Before(b.Time)
}

func gitopsManaged(object []byte, managers []metav1.ManagedFieldsEntry) bool {
	for _, m := range managers {
		if gitopsManagers[m.Manager] {
			return true
		}
	}
	var o struct {
		Metadata struct{ Labels, Annotations map[string]string }
	}
	json.Unmarshal(object, &o)
	for _, k := range gitopsMarkers {
		if _, ok := o.Metadata.Labels[k]; ok {
			return true
		}
		if _, ok := o.Metadata.Annotations[k]; ok {
			return true
		}
	}
	return false
}

var pointerUnescape = strings.NewReplacer("~1", "/", "~0", "~")

func splitPointer(p string) []string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		segs[i] = pointerUnescape.Replace(s)
	}
	return segs
}
