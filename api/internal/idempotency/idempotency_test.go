package idempotency_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/idempotency"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

type rig struct {
	srv   *httptest.Server
	pool  *pgxpool.Pool
	calls atomic.Int32
}

// newRig serves a handler that counts its runs and answers status for the
// request number n (1-based). The scope is the X-Caller header.
func newRig(t *testing.T, delay time.Duration, status func(n int) int) *rig {
	t.Helper()
	return newRigWith(t, delay, status, nil)
}

func newRigWith(t *testing.T, delay time.Duration, status func(n int) int, noStore func(*http.Request) bool) *rig {
	t.Helper()
	r := &rig{pool: testdb.New(t)}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := int(r.calls.Add(1))
		time.Sleep(delay)
		b, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Secret", "not stored")
		w.WriteHeader(status(n))
		_, _ = w.Write([]byte(`{"run":` + string(rune('0'+n%10)) + `,"echo":"` + string(b) + `"}`))
	})
	scope := func(req *http.Request) string { return req.Header.Get("X-Caller") }
	r.srv = httptest.NewServer(idempotency.Middleware(r.pool, scope, noStore)(h))
	t.Cleanup(r.srv.Close)
	return r
}

func always(code int) func(int) int { return func(int) int { return code } }

func (r *rig) send(t *testing.T, method, key, caller, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+"/v1/things", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("X-Caller", caller)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func (r *rig) rows(t *testing.T) (n int) {
	t.Helper()
	if err := r.pool.QueryRow(context.Background(), `select count(*) from idempotency_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReplaySameKeyReturnsSameResponse(t *testing.T) {
	r := newRig(t, 0, always(201))
	a, abody := r.send(t, "POST", "k1", "user:1", `{"a":1}`)
	b, bbody := r.send(t, "POST", "k1", "user:1", `{"a":1}`)
	if r.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", r.calls.Load())
	}
	if a.StatusCode != 201 || b.StatusCode != 201 || abody != bbody {
		t.Fatalf("first %d %q, second %d %q", a.StatusCode, abody, b.StatusCode, bbody)
	}
	if b.Header.Get("Idempotent-Replayed") != "true" || a.Header.Get("Idempotent-Replayed") != "" {
		t.Errorf("Idempotent-Replayed: first %q, second %q", a.Header.Get("Idempotent-Replayed"), b.Header.Get("Idempotent-Replayed"))
	}
	if b.Header.Get("Content-Type") != "application/json" {
		t.Errorf("replayed Content-Type %q", b.Header.Get("Content-Type"))
	}
	if a.Header.Get("X-Secret") == "" || b.Header.Get("X-Secret") != "" {
		t.Errorf("X-Secret: first %q, replay %q; only the first response carries it", a.Header.Get("X-Secret"), b.Header.Get("X-Secret"))
	}
}

func TestDifferentBodySameKeyIs422(t *testing.T) {
	r := newRig(t, 0, always(201))
	r.send(t, "POST", "k1", "user:1", `{"a":1}`)
	res, body := r.send(t, "POST", "k1", "user:1", `{"a":2}`)
	if res.StatusCode != 422 || !strings.Contains(body, `"idempotency_key_reused"`) {
		t.Fatalf("got %d %s", res.StatusCode, body)
	}
	if r.calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", r.calls.Load())
	}
}

func TestConcurrentSameKeyRunsOnce(t *testing.T) {
	r := newRig(t, 100*time.Millisecond, always(201))
	var wg sync.WaitGroup
	var created, busy, replayed atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, body := r.send(t, "POST", "k1", "user:1", `{"a":1}`)
			switch {
			case res.StatusCode == 409 && strings.Contains(body, `"idempotency_in_progress"`):
				if res.Header.Get("Retry-After") != "1" {
					t.Errorf("409 Retry-After = %q, want 1", res.Header.Get("Retry-After"))
				}
				busy.Add(1)
			case res.StatusCode == 201 && res.Header.Get("Idempotent-Replayed") == "true":
				replayed.Add(1)
			case res.StatusCode == 201:
				created.Add(1)
			default:
				t.Errorf("unexpected %d %s", res.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	if r.calls.Load() != 1 || created.Load() != 1 {
		t.Fatalf("handler ran %d times, %d fresh responses; want 1 and 1", r.calls.Load(), created.Load())
	}
	if busy.Load() == 0 {
		t.Errorf("no request saw 409 while the first was running (replays %d)", replayed.Load())
	}
}

func TestServerErrorReleasesTheKey(t *testing.T) {
	r := newRig(t, 0, func(n int) int {
		if n == 1 {
			return 500
		}
		return 201
	})
	a, _ := r.send(t, "POST", "k1", "user:1", `{}`)
	if a.StatusCode != 500 || r.rows(t) != 0 {
		t.Fatalf("first: %d, rows %d; want 500 and no stored row", a.StatusCode, r.rows(t))
	}
	b, _ := r.send(t, "POST", "k1", "user:1", `{}`)
	if b.StatusCode != 201 || r.calls.Load() != 2 || b.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("retry: %d, runs %d", b.StatusCode, r.calls.Load())
	}
}

func TestRateLimitedResponseIsNotKept(t *testing.T) {
	r := newRig(t, 0, func(n int) int {
		if n == 1 {
			return 429
		}
		return 201
	})
	r.send(t, "POST", "k1", "user:1", `{}`)
	if b, _ := r.send(t, "POST", "k1", "user:1", `{}`); b.StatusCode != 201 {
		t.Fatalf("retry after 429: %d", b.StatusCode)
	}
}

func TestClientErrorIsKept(t *testing.T) {
	r := newRig(t, 0, always(409))
	r.send(t, "POST", "k1", "user:1", `{}`)
	b, _ := r.send(t, "POST", "k1", "user:1", `{}`)
	if b.StatusCode != 409 || b.Header.Get("Idempotent-Replayed") != "true" || r.calls.Load() != 1 {
		t.Fatalf("got %d, runs %d", b.StatusCode, r.calls.Load())
	}
}

func TestScopesDontShareKeys(t *testing.T) {
	r := newRig(t, 0, always(201))
	r.send(t, "POST", "k1", "user:1", `{}`)
	b, _ := r.send(t, "POST", "k1", "user:2", `{}`)
	if r.calls.Load() != 2 || b.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("runs %d, replayed %q", r.calls.Load(), b.Header.Get("Idempotent-Replayed"))
	}
	// The same caller on another path is another scope too.
	req, _ := http.NewRequest("POST", r.srv.URL+"/v1/other", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "k1")
	req.Header.Set("X-Caller", "user:1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if r.calls.Load() != 3 {
		t.Errorf("another path replayed: runs %d", r.calls.Load())
	}
}

func TestNoKeyMeansNoStorage(t *testing.T) {
	r := newRig(t, 0, always(201))
	r.send(t, "POST", "", "user:1", `{}`)
	r.send(t, "POST", "", "user:1", `{}`)
	if r.calls.Load() != 2 || r.rows(t) != 0 {
		t.Fatalf("runs %d, rows %d", r.calls.Load(), r.rows(t))
	}
}

func TestGetIgnoresKeys(t *testing.T) {
	r := newRig(t, 0, always(200))
	r.send(t, "GET", "k1", "user:1", ``)
	r.send(t, "GET", "k1", "user:1", ``)
	if r.calls.Load() != 2 || r.rows(t) != 0 {
		t.Fatalf("runs %d, rows %d", r.calls.Load(), r.rows(t))
	}
}

func TestBadKeyIs400(t *testing.T) {
	r := newRig(t, 0, always(201))
	for name, key := range map[string]string{
		"256 chars": strings.Repeat("a", 256),
		"non-ascii": "café",
	} {
		res, body := r.send(t, "POST", key, "user:1", `{}`)
		if res.StatusCode != 400 || !strings.Contains(body, `"bad_idempotency_key"`) {
			t.Errorf("%s: got %d %s", name, res.StatusCode, body)
		}
	}
	if res, _ := r.send(t, "POST", strings.Repeat("a", 255), "user:1", `{}`); res.StatusCode != 201 {
		t.Errorf("255 chars: got %d, want 201", res.StatusCode)
	}
	if r.calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", r.calls.Load())
	}
}

func TestExpiredKeyRunsAgain(t *testing.T) {
	r := newRig(t, 0, always(201))
	r.send(t, "POST", "k1", "user:1", `{}`)
	if _, err := testdb.Owner(t, r.pool).Exec(context.Background(), `update idempotency_keys set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	b, _ := r.send(t, "POST", "k1", "user:1", `{}`)
	if r.calls.Load() != 2 || b.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("runs %d, replayed %q", r.calls.Load(), b.Header.Get("Idempotent-Replayed"))
	}
}

func TestPanicReleasesTheKey(t *testing.T) {
	pool := testdb.New(t)
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			panic("boom")
		}
		w.WriteHeader(201)
	})
	srv := httptest.NewServer(idempotency.Middleware(pool, func(*http.Request) string { return "anon" }, nil)(h))
	defer srv.Close()
	for i, want := range []bool{false, true} {
		req, _ := http.NewRequest("POST", srv.URL+"/x", strings.NewReader(`{}`))
		req.Header.Set("Idempotency-Key", strings.Repeat("k", 22))
		res, err := http.DefaultClient.Do(req)
		if (err == nil && res.StatusCode == 201) != want {
			t.Fatalf("call %d: %v %v", i, res, err)
		}
		if res != nil {
			_ = res.Body.Close()
		}
	}
}

func TestQueryIsPartOfTheFingerprint(t *testing.T) {
	r := newRig(t, 0, always(201))
	post := func(q string) int {
		req, _ := http.NewRequest("POST", r.srv.URL+"/v1/things"+q, strings.NewReader(`{}`))
		req.Header.Set("Idempotency-Key", "k1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if a, b := post("?name=a"), post("?name=b"); a != 201 || b != 422 {
		t.Fatalf("got %d then %d, want 201 then 422", a, b)
	}
}

func TestAnonymousKeysNeedEntropy(t *testing.T) {
	r := newRig(t, 0, always(201))
	for _, k := range []string{"k1", "123456", strings.Repeat("a", 21), strings.Repeat("a", 21) + "!"} {
		res, body := r.send(t, "POST", k, "anon", `{}`)
		if res.StatusCode != 400 || !strings.Contains(body, `"bad_idempotency_key"`) {
			t.Errorf("anon key %q: got %d %s", k, res.StatusCode, body)
		}
	}
	for _, k := range []string{strings.Repeat("a", 22), "0f8fad5b-d9cb-469f-a165-70867728950e"} {
		if res, body := r.send(t, "POST", k, "anon", `{}`); res.StatusCode != 201 {
			t.Errorf("anon key %q: got %d %s", k, res.StatusCode, body)
		}
	}
	if res, _ := r.send(t, "POST", "k1", "user:1", `{}`); res.StatusCode != 201 {
		t.Errorf("short key for a signed-in caller: %d", res.StatusCode)
	}
}

func TestNoStoreReservesButNeverKeeps(t *testing.T) {
	r := newRigWith(t, 0, always(200), func(*http.Request) bool { return true })
	a, _ := r.send(t, "POST", "k1", "user:1", `{}`)
	b, _ := r.send(t, "POST", "k1", "user:1", `{}`)
	if r.calls.Load() != 2 || a.StatusCode != 200 || b.StatusCode != 200 || b.Header.Get("Idempotent-Replayed") != "" || r.rows(t) != 0 {
		t.Fatalf("runs %d, rows %d, replayed %q", r.calls.Load(), r.rows(t), b.Header.Get("Idempotent-Replayed"))
	}
	slow := newRigWith(t, 150*time.Millisecond, always(200), func(*http.Request) bool { return true })
	done := make(chan struct{})
	go func() { slow.send(t, "POST", "k1", "user:1", `{}`); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if res, body := slow.send(t, "POST", "k1", "user:1", `{}`); res.StatusCode != 409 || !strings.Contains(body, "idempotency_in_progress") {
		t.Errorf("duplicate while running: %d %s", res.StatusCode, body)
	}
	<-done
}

func TestBodilessReplayHasNoContentType(t *testing.T) {
	pool := testdb.New(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	srv := httptest.NewServer(idempotency.Middleware(pool, func(*http.Request) string { return "user:1" }, nil)(h))
	defer srv.Close()
	for i := range 2 {
		req, _ := http.NewRequest("DELETE", srv.URL+"/x", nil)
		req.Header.Set("Idempotency-Key", "k1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 204 || res.Header.Get("Content-Type") != "" {
			t.Errorf("call %d: %d content-type %q", i, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
}

// A request that outlives its two-minute reservation must not overwrite the
// row of the run that took over.
func TestStaleRunCannotClobberANewerRow(t *testing.T) {
	pool := testdb.New(t)
	var n atomic.Int32
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		run := n.Add(1)
		if run == 1 {
			<-release
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("run" + string(rune('0'+run))))
	})
	srv := httptest.NewServer(idempotency.Middleware(pool, func(*http.Request) string { return "user:1" }, nil)(h))
	defer srv.Close()
	send := func() string {
		req, _ := http.NewRequest("POST", srv.URL+"/x", strings.NewReader(`{}`))
		req.Header.Set("Idempotency-Key", "k1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return ""
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	first := make(chan string)
	go func() { first <- send() }()
	time.Sleep(100 * time.Millisecond)
	if _, err := testdb.Owner(t, pool).Exec(context.Background(), `update idempotency_keys set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if got := send(); got != "run2" {
		t.Fatalf("takeover run = %q", got)
	}
	close(release)
	if got := <-first; got != "run1" {
		t.Fatalf("stale run = %q", got)
	}
	if got := send(); got != "run2" || n.Load() != 2 {
		t.Fatalf("replay = %q after %d runs; the stale run clobbered the newer row", got, n.Load())
	}
}
