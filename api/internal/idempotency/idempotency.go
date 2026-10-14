// Package idempotency replays the first response to a write that is retried
// with the same Idempotency-Key.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Middleware wraps the writes (POST, PUT, PATCH, DELETE) that carry an
// Idempotency-Key. scope names the caller ("user:<id>", "key:<id>", "anon");
// the method and path are added here, so a key never crosses callers or
// endpoints. The first request runs and its status, Content-Type and body are
// kept for 24 hours; 5xx and 429 answers are not kept, so a retry runs again.
// A repeat replays the kept response with Idempotent-Replayed: true, a repeat
// with another body is 422, and a repeat while the first still runs is 409.
func Middleware(pool *pgxpool.Pool, scope func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				next.ServeHTTP(w, r)
				return
			}
			vals := r.Header.Values("Idempotency-Key")
			if len(vals) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			key := vals[0]
			if len(vals) > 1 || !validKey(key) {
				fail(w, 400, "bad_idempotency_key", "The Idempotency-Key must be one value of 1 to 255 printable ASCII characters.")
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				fail(w, 400, "bad_request", "That request couldn't be read.")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			sum := sha256.Sum256(body)
			sc := scope(r) + " " + r.Method + " " + r.URL.Path
			run(w, r, next, pool, sc, key, sum[:])
		})
	}
}

func validKey(k string) bool {
	if len(k) < 1 || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func run(w http.ResponseWriter, r *http.Request, next http.Handler, pool *pgxpool.Pool, scope, key string, hash []byte) {
	ctx := r.Context()
	for range 3 {
		// ponytail: a row still running after 2 minutes is treated as dead
		// (crashed server); a request that slow could run twice.
		tag, err := pool.Exec(ctx, `insert into idempotency_keys (scope, key, request_hash, expires_at)
			values ($1, $2, $3, now() + interval '2 minutes') on conflict do nothing`, scope, key, hash)
		if err != nil {
			slog.ErrorContext(ctx, "idempotency: insert", "err", err)
			fail(w, 500, "internal", "Something went wrong on our side. Please try again.")
			return
		}
		if tag.RowsAffected() == 1 {
			execute(w, r, next, pool, scope, key)
			return
		}
		var (
			stored  []byte
			status  *int
			ctype   *string
			saved   []byte
			expired bool
		)
		err = pool.QueryRow(ctx, `select request_hash, status, content_type, body, expires_at <= now()
			from idempotency_keys where scope = $1 and key = $2`, scope, key).Scan(&stored, &status, &ctype, &saved, &expired)
		switch {
		case errors.Is(err, pgx.ErrNoRows): // released between our two statements
			continue
		case err != nil:
			slog.ErrorContext(ctx, "idempotency: select", "err", err)
			fail(w, 500, "internal", "Something went wrong on our side. Please try again.")
			return
		case expired:
			if _, err := pool.Exec(ctx, `delete from idempotency_keys where scope = $1 and key = $2 and expires_at <= now()`, scope, key); err != nil {
				slog.ErrorContext(ctx, "idempotency: delete expired", "err", err)
			}
			continue
		case !bytes.Equal(stored, hash):
			fail(w, 422, "idempotency_key_reused", "That Idempotency-Key was already used for a different request. Use a new key for a new action.")
		case status == nil:
			fail(w, 409, "idempotency_in_progress", "The first request with that Idempotency-Key is still running. Wait a moment and retry.")
		default:
			if ctype != nil {
				w.Header().Set("Content-Type", *ctype)
			}
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(*status)
			_, _ = w.Write(saved)
		}
		return
	}
	fail(w, 409, "idempotency_in_progress", "The first request with that Idempotency-Key is still running. Wait a moment and retry.")
}

// execute runs the handler into a buffer, keeps the outcome unless it was a
// server error, and writes it out. If the handler panics the key is released.
func execute(w http.ResponseWriter, r *http.Request, next http.Handler, pool *pgxpool.Pool, scope, key string) {
	rec := &recorder{header: http.Header{}, status: 200}
	done := false
	defer func() {
		if !done {
			release(pool, scope, key)
		}
	}()
	next.ServeHTTP(rec, r)
	done = true
	ctx := context.WithoutCancel(r.Context())
	if rec.status >= 500 || rec.status == http.StatusTooManyRequests {
		release(pool, scope, key)
	} else if _, err := pool.Exec(ctx, `update idempotency_keys set status = $3, content_type = $4, body = $5,
		expires_at = now() + interval '24 hours' where scope = $1 and key = $2`,
		scope, key, rec.status, rec.header.Get("Content-Type"), rec.body.Bytes()); err != nil {
		// Without the stored answer a retry would wait on a row that never
		// completes; free it.
		slog.ErrorContext(ctx, "idempotency: store", "err", err)
		release(pool, scope, key)
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body.Bytes())
}

func release(pool *pgxpool.Pool, scope, key string) {
	if _, err := pool.Exec(context.Background(), `delete from idempotency_keys where scope = $1 and key = $2`, scope, key); err != nil {
		slog.Error("idempotency: release", "err", err)
	}
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.status = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
