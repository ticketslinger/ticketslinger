package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
)

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code }

func unauthorized() error {
	return &apiError{401, "unauthorized", "Please sign in to continue."}
}
func tooManyAttempts() error {
	return &apiError{429, "rate_limited", "Too many attempts. Try again in a few minutes."}
}
func notFound() error {
	return &apiError{404, "not_found", "That doesn't exist, or you don't have access to it."}
}

var errNotImplemented = &apiError{501, "not_implemented", "This isn't available yet."}

// writeBadRequest answers a request that couldn't be decoded. The decoder's
// detail goes to the log, not the caller.
func writeBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	slog.InfoContext(r.Context(), "bad request", "path", r.URL.Path, "err", err)
	writeError(w, &apiError{400, "bad_request", "That request couldn't be read."})
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		slog.Error("unhandled error", "err", err)
		ae = &apiError{500, "internal", "Something went wrong on our side. Please try again."}
	}
	writeJSON(w, ae.Status, map[string]any{"error": map[string]string{"code": ae.Code, "message": ae.Message}})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "value", v, "path", r.URL.Path)
				writeError(w, errors.New("panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
