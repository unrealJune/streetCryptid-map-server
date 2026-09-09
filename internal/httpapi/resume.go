package httpapi

import (
	"io"
	"net/http"
	"time"
)

func serveStreamContent(w http.ResponseWriter, r *http.Request, etag string, content io.ReadSeeker) {
	http.ServeContent(&rangeResponseWriter{ResponseWriter: w, etag: etag}, r, "", time.Time{}, content)
}

type rangeResponseWriter struct {
	http.ResponseWriter
	etag string
}

func (w *rangeResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *rangeResponseWriter) WriteHeader(status int) {
	if status == http.StatusRequestedRangeNotSatisfiable {
		// ServeContent strips validators on errors. The client needs the current
		// representation's validator to recognize a fully saved transfer.
		w.Header().Set("ETag", w.etag)
		w.Header().Set("Cache-Control", "no-store, no-transform")
		w.Header().Del("Content-Encoding")
	}
	w.ResponseWriter.WriteHeader(status)
}
