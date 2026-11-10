package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleEvents streams hub events as SSE (CONTRACTS 2). One "maki" event per
// hub event with the Event JSON as data, a literal retry hint, and a comment
// ping every 20s to keep proxies warm.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	events := s.cfg.Hub.Subscribe()
	defer s.cfg.Hub.Unsubscribe(events)

	fmt.Fprint(w, sseRetry)
	rc.Flush()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.cfg.Ctx.Done():
			return
		case e := <-events:
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: maki\ndata: %s\n\n", data)
			rc.Flush()
		case <-ping.C:
			fmt.Fprint(w, ssePing)
			rc.Flush()
		}
	}
}
