package api

import (
	"net/http"
	"slices"
)

// cors lets the listed browser origins call the API. The frontend sends its
// session token in the Authorization header (from Phase 2), never cookies, so
// credentials aren't allowed. Requests from other origins still reach the
// handler (CORS only stops browsers from reading the answer); they just get
// no CORS headers.
func cors(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		ok := origin != "" && slices.Contains(allowed, origin)
		if ok {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if ok {
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				h.Set("Access-Control-Max-Age", "86400")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
