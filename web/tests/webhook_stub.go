package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync/atomic"
)

// A counting webhook sink for the end-to-end suite.
//
// The count is the point. "No escalation step fired after the acknowledgement"
// cannot be asserted from a 2xx or from the absence of an error — only from the
// delivery count not moving (AGENTS.md rule 6). /count exposes it so a
// Playwright spec can assert the effect instead of a status code.
func main() {
	var delivered atomic.Int64
	var lastPayload atomic.Value
	lastPayload.Store("{}")

	handler := http.NewServeMux()
	handler.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler.HandleFunc("/notify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		lastPayload.Store(string(body))
		delivered.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	handler.HandleFunc("/count", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"count": delivered.Load()})
	})
	handler.HandleFunc("/last", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lastPayload.Load().(string)))
	})
	handler.HandleFunc("/reset", func(w http.ResponseWriter, _ *http.Request) {
		delivered.Store(0)
		lastPayload.Store("{}")
		w.WriteHeader(http.StatusNoContent)
	})
	if err := http.ListenAndServe("127.0.0.1:3101", handler); err != nil {
		log.Fatal(err)
	}
}
