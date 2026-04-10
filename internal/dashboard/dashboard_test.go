package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	if h.etag == "" {
		t.Fatal("expected non-empty etag")
	}
	if len(h.body) == 0 {
		t.Fatal("expected non-empty body")
	}
}

func TestServeHTTP_OK(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("expected text/html; charset=utf-8, got %q", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Fatal("body missing <html")
	}
	if !strings.Contains(body, "</html>") {
		t.Fatal("body missing </html>")
	}
}

func TestServeHTTP_ContainsAPIEndpoints(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "/v1/pipelines") {
		t.Fatal("body missing /v1/pipelines endpoint reference")
	}
	if !strings.Contains(body, "/v1/stream") {
		t.Fatal("body missing /v1/stream endpoint reference")
	}
}

func TestServeHTTP_NoAPIKeys(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	patterns := []string{"sk-ant", "sk-proj"}
	for _, p := range patterns {
		if strings.Contains(body, p) {
			t.Fatalf("body contains API key pattern %q", p)
		}
	}
	// Check for Bearer followed by non-placeholder text (not "Bearer ' + apiKey")
	// The dashboard only uses "Bearer " in JS string concatenation, not with actual keys
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.Contains(line, "Bearer ") {
			// Allowed: string concatenation patterns in JS
			if strings.Contains(line, "'Bearer '") || strings.Contains(line, "\"Bearer \"") || strings.Contains(line, "`Bearer ") {
				continue
			}
			// Allowed: comment references
			if strings.TrimSpace(line)[:2] == "//" || strings.TrimSpace(line)[:1] == "*" {
				continue
			}
			t.Fatalf("line %d contains potentially hardcoded Bearer token: %s", i+1, strings.TrimSpace(line))
		}
	}
}

func TestServeHTTP_ETag(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// First request — get ETag
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header")
	}

	// Second request with matching If-None-Match → 304
	req2 := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if w2.Code != http.StatusNotModified {
		t.Fatalf("expected 304 with matching ETag, got %d", w2.Code)
	}

	// Third request without If-None-Match → 200 with full body
	req3 := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200 without If-None-Match, got %d", w3.Code)
	}
	if w3.Body.Len() == 0 {
		t.Fatal("expected non-empty body without If-None-Match")
	}
}

func TestServeHTTP_ETagConsistent(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	var firstETag string
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		etag := w.Header().Get("ETag")
		if i == 0 {
			firstETag = etag
		} else if etag != firstETag {
			t.Fatalf("ETag changed on request %d: %q != %q", i+1, etag, firstETag)
		}
	}
}

func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/dashboard", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: expected 405, got %d", method, w.Code)
		}
	}
}
