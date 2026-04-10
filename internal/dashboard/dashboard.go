// Package dashboard serves the embedded pipeline dashboard as a single HTML file.
package dashboard

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
)

//go:embed dashboard.html
var content embed.FS

// Handler serves the embedded dashboard HTML with caching headers.
type Handler struct {
	body []byte
	etag string
}

// New creates a dashboard Handler by reading the embedded HTML file.
func New() (*Handler, error) {
	data, err := content.ReadFile("dashboard.html")
	if err != nil {
		return nil, fmt.Errorf("reading embedded dashboard: %w", err)
	}
	hash := sha256.Sum256(data)
	return &Handler{
		body: data,
		etag: fmt.Sprintf(`"%x"`, hash[:16]),
	}, nil
}

// ServeHTTP serves the dashboard HTML with ETag-based caching.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("ETag", h.etag)
	w.Header().Set("Cache-Control", "no-cache")
	// SEC4-002: Content-Security-Policy restricts script sources to self and
	// the D3 CDN, prevents framing, and limits connect-src to same-origin.
	// style-src allows 'unsafe-inline' because the dashboard uses an embedded
	// <style> block — moving to a nonce would require dynamic HTML generation.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'self' https://cdnjs.cloudflare.com; "+
			"style-src 'self' 'unsafe-inline'; "+
			"connect-src 'self'; "+
			"img-src 'self'; "+
			"frame-ancestors 'none'")

	if match := r.Header.Get("If-None-Match"); match == h.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(h.body)
}
