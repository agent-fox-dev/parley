package metrics

import (
	"bytes"
	"net/http"
)

// ContentType is the media type of the text exposition, format 0.0.4.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler answers a scrape with reg's exposition of every catalogue metric.
// It asks for no credential: the monitoring that scrapes the hub holds
// none, and the metrics name no secret. The router it is mounted on
// answers the methods other than GET and HEAD.
func Handler(reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		// A bytes.Buffer does not fail.
		_ = WriteText(&body, reg.Gather(r.Context()))
		w.Header().Set("Content-Type", ContentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body.Bytes())
	})
}
