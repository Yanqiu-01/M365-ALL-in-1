package web

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// EnvHTTPTraceVerbose restores a line for every traced request, including the
// dashboard polling that is filtered by default. Debugging escape hatch: it is
// read only when a line is about to be dropped, so it costs nothing otherwise.
const EnvHTTPTraceVerbose = "M365_HTTP_TRACE_VERBOSE"

// tracePollNoise are the endpoints the admin dashboard polls on a timer. In one
// recent window they accounted for 176 hits on /api/admin/proxy-pool, 143 on
// /api/admin/panel/job/poll, 127 on /api/admin/login and 126 on /api/accounts,
// two log lines each, in a gateway log that had reached 31 MB. A routine success
// on one of these says nothing a human will ever read back, so it is dropped.
//
// Only success is dropped. Anything with status >= 400 is always logged, whatever
// the path - a filter that can hide an error is a filter that will hide the one
// error that mattered.
//
// The match is on path alone, so a *successful* mutation on one of these routes
// (a POST or DELETE to /api/admin/proxy-pool, say) is filtered too; set
// M365_HTTP_TRACE_VERBOSE to see it. Extend the set by adding a line.
var tracePollNoise = map[string]bool{
	"/api/admin/proxy-pool":     true,
	"/api/admin/panel/job/poll": true,
	"/api/admin/login":          true,
	"/api/accounts":             true,
}

type traceWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *traceWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *traceWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}
func (w *traceWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// traceSkip decides whether a completed request is routine dashboard polling.
func traceSkip(path string, status int) bool {
	if status >= 400 || !tracePollNoise[path] {
		return false
	}
	return !truthy(os.Getenv(EnvHTTPTraceVerbose))
}

// httpTrace logs one line per API request, after it completes.
//
// It used to log twice, stage=start and stage=end, which doubled the disk churn
// of the dashboard polling above and split the facts of a single request across
// two lines a reader had to correlate by id. One line after the fact carries
// everything both lines carried; the [http-trace] prefix is unchanged so existing
// greps keep working.
func httpTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		tw := &traceWriter{ResponseWriter: w}
		next.ServeHTTP(tw, r)
		status := tw.status
		if status == 0 {
			status = http.StatusOK
		}
		if traceSkip(r.URL.Path, status) {
			return
		}
		log.Printf("[http-trace] id=%s method=%s path=%s status=%d bytes=%d total_ms=%d",
			requestIDFrom(r), r.Method, r.URL.Path, status, tw.bytes, time.Since(start).Milliseconds())
	})
}
