package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	lexifyKey = "tk1_test_lexify_key"
	clientIP4 = "203.0.113.77" // documentation range; must never reach storage or logs
)

// memStore is an in-memory Store with the same duplicate rule as the unique index (app, session, seq).
type memStore struct {
	mu        sync.Mutex
	apps      map[string]App // key hash -> app
	places    map[string]bool
	events    []Event
	firstOpen map[string]int // app|platform|campaign
	stats     []StatRow
	accepted  int
	failWith  error
	lookups   int
}

func newMemStore() *memStore {
	return &memStore{
		apps:      map[string]App{HashKey(lexifyKey): {ID: "lexify", StoreCountry: true}},
		places:    map[string]bool{"flyer-cafe": true},
		firstOpen: map[string]int{},
	}
}

func (m *memStore) LookupKey(_ context.Context, h string) (App, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	a, ok := m.apps[h]
	return a, ok, m.failWith
}

func (m *memStore) Placements(context.Context) (map[string]bool, error) { return m.places, m.failWith }

func (m *memStore) Insert(_ context.Context, app string, _ time.Time, evs []Event) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return 0, m.failWith
	}
	n := 0
	for _, e := range evs {
		if e.Seq != nil && m.has(e) {
			continue
		}
		m.events = append(m.events, e)
		n++
		if p, c, ok := FirstOpen(e); ok {
			m.firstOpen[app+"|"+p+"|"+c]++
		}
	}
	return n, nil
}

func (m *memStore) has(e Event) bool {
	for _, x := range m.events {
		if x.Seq != nil && x.Session == e.Session && *x.Seq == *e.Seq {
			return true
		}
	}
	return false
}

func (m *memStore) AcceptedOn(context.Context, string, time.Time) (int, error) {
	return m.accepted, nil
}

func (m *memStore) AddStats(_ context.Context, rows []StatRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats = append(m.stats, rows...)
	return nil
}

func (m *memStore) Ping(context.Context) error { return m.failWith }

func (m *memStore) stat(reason Reason) (reqs, evs int) {
	for _, r := range m.stats {
		if r.Reason == reason {
			reqs += r.Requests
			evs += r.Events
		}
	}
	return
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestServer(st *memStore, logs *bytes.Buffer) (*Server, *clock) {
	c := &clock{t: testNow}
	return New(Config{Store: st, Now: c.now, Log: slog.New(slog.NewTextHandler(logs, nil))}), c
}

func post(h http.Handler, key string, body []byte, mod ...func(*http.Request)) int {
	r := httptest.NewRequest(http.MethodPost, "/v1/batch", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("X-Telemetry-Key", key)
	}
	r.Header.Set("CF-IPCountry", "CZ")
	r.Header.Set("CF-Connecting-IP", clientIP4)
	r.Header.Set("X-Forwarded-For", clientIP4)
	r.RemoteAddr = clientIP4 + ":51234"
	for _, f := range mod {
		f(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// oneEvent is the smallest valid batch.
var oneEvent = []byte(`{"v":1,"app":"lexify","events":[{"n":"app_open","t":"2026-11-14T10:00:00Z","s":"` + sess + `","p":{}}]}`)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBatchResponses(t *testing.T) {
	big := append([]byte(`{"v":1,"app":"lexify","events":[{}]`), bytes.Repeat([]byte(" "), MaxBody)...)
	big = append(big, '}')
	tests := []struct {
		name     string
		file     string
		body     []byte
		key      string
		mod      func(*http.Request)
		code     int
		stored   int
		reasons  map[Reason]int // events per stats reason after flush
		firstOps map[string]int
	}{
		{name: "valid", file: "valid.json", key: lexifyKey, code: 204, stored: 4,
			reasons: map[Reason]int{ReasonAccepted: 4}, firstOps: map[string]int{"lexify|ios|": 1}},
		{name: "missing key", file: "valid.json", code: 401, reasons: map[Reason]int{ReasonBadKey: 0}},
		{name: "unknown key", file: "valid.json", key: "tk1_wrong", code: 401},
		{name: "key of another app", file: "bad_key.json", key: lexifyKey, code: 401, reasons: map[Reason]int{ReasonAppMismatch: 1}},
		{name: "bad shape", file: "protocol/events_not_list.json", key: lexifyKey, code: 400, reasons: map[Reason]int{ReasonBadShape: 0}},
		{name: "bad version", file: "protocol/bad_version.json", key: lexifyKey, code: 400, reasons: map[Reason]int{ReasonBadVersion: 0}},
		{name: "free text dropped per event", file: "free_text.json", key: lexifyKey, code: 204, stored: 1,
			reasons: map[Reason]int{ReasonAccepted: 1, ReasonBadProps: 4, ReasonBadName: 1}},
		{name: "future clamped, old dropped", file: "future_time.json", key: lexifyKey, code: 204, stored: 2,
			reasons: map[Reason]int{ReasonAccepted: 2, ReasonClampedTime: 1, ReasonTooOld: 1}},
		{name: "duplicate in batch", file: "duplicate.json", key: lexifyKey, code: 204, stored: 3,
			reasons: map[Reason]int{ReasonAccepted: 3, ReasonDuplicate: 1}},
		{name: "campaign checked against qr_placements", file: "campaign.json", key: lexifyKey, code: 204, stored: 3,
			firstOps: map[string]int{"lexify|android|flyer-cafe": 1, "lexify|android|other": 1}},
		{name: "too large", body: big, key: lexifyKey, code: 413, reasons: map[Reason]int{ReasonTooLarge: 0}},
		{name: "too large without content length", body: big, key: lexifyKey, code: 413,
			mod: func(r *http.Request) { r.ContentLength = -1 }},
		{name: "wrong content type", file: "valid.json", key: lexifyKey, code: 400,
			mod: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{name: "content type with charset", file: "valid.json", key: lexifyKey, code: 204, stored: 4,
			mod: func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemStore()
			var logs bytes.Buffer
			s, _ := newTestServer(st, &logs)
			body := tc.body
			if tc.file != "" {
				body = readTestdata(t, tc.file)
			}
			var mods []func(*http.Request)
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			if code := post(s.Handler(), tc.key, body, mods...); code != tc.code {
				t.Fatalf("code = %d, want %d", code, tc.code)
			}
			if len(st.events) != tc.stored {
				t.Fatalf("stored %d events, want %d", len(st.events), tc.stored)
			}
			if err := s.FlushStats(context.Background()); err != nil {
				t.Fatal(err)
			}
			for reason, want := range tc.reasons {
				if _, evs := st.stat(reason); evs != want {
					t.Errorf("stat %s = %d events, want %d", reason, evs, want)
				}
			}
			for k, want := range tc.firstOps {
				if st.firstOpen[k] != want {
					t.Errorf("first_open[%s] = %d, want %d (all: %v)", k, st.firstOpen[k], want, st.firstOpen)
				}
			}
			if tc.firstOps != nil && len(st.firstOpen) != len(tc.firstOps) {
				t.Errorf("first_open = %v, want %v", st.firstOpen, tc.firstOps)
			}
			assertNoIP(t, st, &logs)
		})
	}
}

// assertNoIP: the client address appears in no stored event, stat or log line.
func assertNoIP(t *testing.T, st *memStore, logs *bytes.Buffer) {
	t.Helper()
	b, _ := json.Marshal(st.events)
	s, _ := json.Marshal(st.stats)
	for what, data := range map[string]string{"events": string(b), "stats": string(s), "logs": logs.String()} {
		if strings.Contains(data, clientIP4) {
			t.Errorf("client IP found in %s", what)
		}
	}
}

func TestStoredEventShape(t *testing.T) {
	st := newMemStore()
	s, _ := newTestServer(st, &bytes.Buffer{})
	if code := post(s.Handler(), lexifyKey, readTestdata(t, "valid.json")); code != 204 {
		t.Fatal(code)
	}
	ev := st.events[0]
	if ev.Name != "app_open" || ev.Country != "CZ" || ev.Session != sess || ev.Seq == nil || *ev.Seq != 1 {
		t.Fatalf("event = %+v", ev)
	}
	if !ev.Time.Equal(time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)) || !ev.Day.Equal(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("time %v day %v", ev.Time, ev.Day)
	}
	if st.events[3].Seq != nil {
		t.Fatal("event without i must have no seq")
	}
}

func TestCountryNotStoredWhenDisabled(t *testing.T) {
	st := newMemStore()
	st.apps[HashKey(lexifyKey)] = App{ID: "lexify", StoreCountry: false}
	s, _ := newTestServer(st, &bytes.Buffer{})
	if code := post(s.Handler(), lexifyKey, readTestdata(t, "valid.json")); code != 204 {
		t.Fatal(code)
	}
	for _, e := range st.events {
		if e.Country != "" {
			t.Fatalf("country stored: %q", e.Country)
		}
	}
}

func TestResendDeduplicates(t *testing.T) {
	st := newMemStore()
	s, _ := newTestServer(st, &bytes.Buffer{})
	body := readTestdata(t, "valid.json")
	for range 2 {
		if code := post(s.Handler(), lexifyKey, body); code != 204 {
			t.Fatal(code)
		}
	}
	// Three events carry i and are stored once; the one without i is stored twice (no check without i).
	if len(st.events) != 5 {
		t.Fatalf("stored %d, want 5", len(st.events))
	}
	if st.firstOpen["lexify|ios|"] != 1 {
		t.Fatalf("first_open counted twice: %v", st.firstOpen)
	}
}

func TestStoreErrorIsRetryable(t *testing.T) {
	st := newMemStore()
	s, _ := newTestServer(st, &bytes.Buffer{})
	h := s.Handler()
	if code := post(h, lexifyKey, readTestdata(t, "valid.json")); code != 204 { // warms the key cache
		t.Fatal(code)
	}
	st.failWith = errors.New("connection refused")
	if code := post(h, lexifyKey, readTestdata(t, "valid.json")); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", code)
	}
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz = %d", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(newMemStore(), &bytes.Buffer{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/batch", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/batch = %d", w.Code)
	}
}

func TestRateLimitPerIP(t *testing.T) {
	st := newMemStore()
	c := &clock{t: testNow}
	s := New(Config{Store: st, Now: c.now, RatePerMinute: 60, Burst: 3})
	h := s.Handler()
	body := oneEvent
	other := func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "198.51.100.9") }
	for i := range 3 {
		if code := post(h, lexifyKey, body); code != 204 {
			t.Fatalf("request %d = %d", i, code)
		}
	}
	if code := post(h, lexifyKey, body); code != http.StatusTooManyRequests {
		t.Fatalf("4th request = %d, want 429", code)
	}
	if code := post(h, lexifyKey, body, other); code != 204 {
		t.Fatalf("other IP = %d", code)
	}
	c.t = c.t.Add(time.Second) // one token back at 60/min
	if code := post(h, lexifyKey, body); code != 204 {
		t.Fatalf("after refill = %d", code)
	}
	// Idle IPs are forgotten.
	c.t = c.t.Add(10 * time.Minute)
	post(h, lexifyKey, body, other)
	s.limiter.mu.Lock()
	n := len(s.limiter.buckets)
	s.limiter.mu.Unlock()
	if n != 1 {
		t.Fatalf("limiter keeps %d IPs, want 1", n)
	}
}

func TestDailyCap(t *testing.T) {
	st := newMemStore()
	st.accepted = 5 // already accepted today before a restart
	c := &clock{t: testNow}
	s := New(Config{Store: st, Now: c.now, DailyCap: 8, Burst: 100})
	h := s.Handler()
	body := readTestdata(t, "valid.json") // 4 events
	if code := post(h, lexifyKey, body); code != http.StatusTooManyRequests {
		t.Fatalf("over cap = %d, want 429", code)
	}
	c.t = c.t.Add(24 * time.Hour)
	st.accepted = 0
	if code := post(h, lexifyKey, readTestdata(t, "duplicate.json")); code != 204 {
		t.Fatalf("next day = %d", code)
	}
}

func TestRevokedKeyStopsWithinCacheTTL(t *testing.T) {
	st := newMemStore()
	c := &clock{t: testNow}
	s := New(Config{Store: st, Now: c.now, CacheTTL: 30 * time.Second, Burst: 100})
	h := s.Handler()
	body := oneEvent
	if code := post(h, lexifyKey, body); code != 204 {
		t.Fatal(code)
	}
	delete(st.apps, HashKey(lexifyKey)) // store-metrics telemetry key revoke lexify
	c.t = c.t.Add(10 * time.Second)
	if code := post(h, lexifyKey, body); code != 204 {
		t.Fatalf("within TTL = %d (cached)", code)
	}
	c.t = c.t.Add(31 * time.Second)
	if code := post(h, lexifyKey, body); code != 401 {
		t.Fatalf("after TTL = %d, want 401", code)
	}
	// Unknown keys are cached too, so a fork with a wrong key does not hit the database on every request.
	before := st.lookups
	for range 5 {
		post(h, "tk1_fork", body)
	}
	if st.lookups-before != 1 {
		t.Fatalf("%d lookups for a repeated unknown key", st.lookups-before)
	}
}

func TestFlushStatsKeepsRowsOnError(t *testing.T) {
	st := &failingStats{memStore: newMemStore()}
	s, _ := newTestServer(st.memStore, &bytes.Buffer{})
	s.cfg.Store = st
	post(s.Handler(), "", nil)
	if err := s.FlushStats(context.Background()); err == nil {
		t.Fatal("want error")
	}
	st.ok = true
	if err := s.FlushStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reqs, _ := st.stat(ReasonBadKey); reqs != 1 {
		t.Fatalf("bad_key requests = %d after retry", reqs)
	}
}

type failingStats struct {
	*memStore
	ok bool
}

func (f *failingStats) AddStats(ctx context.Context, rows []StatRow) error {
	if !f.ok {
		return fmt.Errorf("db down")
	}
	return f.memStore.AddStats(ctx, rows)
}

// TestProtocolCases runs the shared test batches of the client package (testdata/protocol, copied from
// Ol1nTelemetry) against the receiver and checks the expected outcome in cases.json.
func TestProtocolCases(t *testing.T) {
	var spec struct {
		Cases []struct {
			File            string   `json:"file"`
			Status          int      `json:"status"`
			Stored          int      `json:"stored"`
			Rejected        int      `json:"rejected"`
			Duplicates      int      `json:"duplicates"`
			RejectedReasons []string `json:"rejected_reasons"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(readTestdata(t, "protocol/cases.json"), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Cases) == 0 {
		t.Fatal("no cases")
	}
	rejectedReasons := []Reason{ReasonBadEvent, ReasonBadName, ReasonBadTime, ReasonBadSession, ReasonBadSeq, ReasonBadProps, ReasonTooOld}
	for _, c := range spec.Cases {
		t.Run(c.File, func(t *testing.T) {
			st := newMemStore()
			s, _ := newTestServer(st, &bytes.Buffer{})
			if code := post(s.Handler(), lexifyKey, readTestdata(t, "protocol/"+c.File)); code != c.Status {
				t.Fatalf("code = %d, want %d", code, c.Status)
			}
			if err := s.FlushStats(context.Background()); err != nil {
				t.Fatal(err)
			}
			rejected := 0
			for _, r := range rejectedReasons {
				_, n := st.stat(r)
				rejected += n
			}
			_, dups := st.stat(ReasonDuplicate)
			if len(st.events) != c.Stored || rejected != c.Rejected || dups != c.Duplicates {
				t.Fatalf("stored %d rejected %d duplicates %d, want %d %d %d", len(st.events), rejected, dups, c.Stored, c.Rejected, c.Duplicates)
			}
			if c.RejectedReasons != nil && len(c.RejectedReasons) != c.Rejected {
				t.Fatalf("cases.json lists %d reasons for %d rejected", len(c.RejectedReasons), c.Rejected)
			}
		})
	}
}
