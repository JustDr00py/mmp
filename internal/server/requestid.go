package server

import (
	"context"
	"net/http"
)

func contextWithRequestID(r *http.Request, id string) context.Context {
	return context.WithValue(r.Context(), requestIDKey, id)
}

// RequestIDFrom returns the request id assigned by the RequestID middleware.
func RequestIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey).(string)
	return id
}
