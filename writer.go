package prometheus

import (
	"net/http"
)

type writer struct {
	w    http.ResponseWriter
	code int
}

// Unwrap implements the http.ResponseController unwrap interface so that
// middleware chains that probe for http.Hijacker, http.Pusher, or HTTP/2
// ResponseWriter extensions can reach the underlying ResponseWriter.
func (w *writer) Unwrap() http.ResponseWriter {
	return w.w
}

func (w *writer) Flush() {
	if fl, ok := w.w.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *writer) WriteHeader(code int) {
	w.code = code
	w.w.WriteHeader(code)
}

func (w *writer) Write(b []byte) (int, error) {
	// If WriteHeader was never called, Go's net/http defaults to 200.
	// Mirror that here so the metrics label is accurate.
	if w.code == -1 {
		w.code = http.StatusOK
	}
	return w.w.Write(b)
}

func (w *writer) Header() http.Header {
	return w.w.Header()
}
