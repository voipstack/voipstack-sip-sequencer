package b2bua

import "net/http"

// healthHandler reports process liveness: 200 with body "ok" while the process
// serves. Pure liveness — it inspects no engine state (a down process is an
// inherent connect failure, no readiness semantics).
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
