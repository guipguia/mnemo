package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	get := func(fsys fstest.MapFS) (int, string) {
		rec := httptest.NewRecorder()
		handler(fsys).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Error("missing CSP header")
		}
		return rec.Code, rec.Body.String()
	}
	if code, body := get(fstest.MapFS{"index.html": {Data: []byte("<div id=root>")}}); code != 200 || !strings.Contains(body, "root") {
		t.Errorf("built: %d %q", code, body)
	}
	if code, body := get(fstest.MapFS{".gitkeep": {}}); code != 404 || !strings.Contains(body, "npm run build") {
		t.Errorf("not built: %d %q", code, body)
	}
}
