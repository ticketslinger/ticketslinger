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

func badRequest(msg string) error { return &apiError{400, "bad_request", msg} }
func notFound() error {
	return &apiError{404, "not_found", "That doesn't exist, or you don't have access to it."}
}

var errNotImplemented = &apiError{501, "not_implemented", "This isn't available yet."}

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
