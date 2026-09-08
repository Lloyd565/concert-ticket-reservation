package middleware

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes a JSON response body with the given status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError writes the gateway's error envelope. It has the same shape as the
// Booking service's REST errors, so a client sees one error format whether the
// request was stopped at the edge or downstream.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, map[string]string{"error": code, "message": message})
}
