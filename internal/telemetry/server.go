package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"
)

// App is a telemetry.apps row as the receiver needs it.
type App struct {
	ID           string
	StoreCountry bool
}

// StatRow is one increment of telemetry.ingest_stats.
type StatRow struct {
	Day      time.Time
	App      string
	Reason   Reason
	Requests int
	Events   int
}

// Store is what the receiver needs from the database. The Postgres implementation is PG.
type Store interface {
	// LookupKey finds the app whose active key hashes to keyHash.
	LookupKey(ctx context.Context, keyHash string) (App, bool, error)
	// Placements returns the known QR placements (qr_placements.place).
	Placements(ctx context.Context) (map[string]bool, error)
	// Insert stores events, skipping duplicates of (app, session, seq), and counts first opens.
	Insert(ctx context.Context, app string, receivedAt time.Time, evs []Event) (inserted int, err error)
	// AcceptedOn returns how many events of app were accepted on day (restores the daily cap after a restart).
	AcceptedOn(ctx context.Context, app string, day time.Time) (int, error)
	AddStats(ctx context.Context, rows []StatRow) error
	Ping(ctx context.Context) error
}

// Config of the receiver. Zero values get the defaults in New.
type Config struct {
	Store         Store
	Now           func() time.Time
	Log           *slog.Logger
	RatePerMinute int           // requests per IP per minute (default 60)
	Burst         int           // bucket size (default 20)
	DailyCap      int           // accepted events per app and UTC day (default 200000)
	CacheTTL      time.Duration // key and placement cache (default 30 s, so a revoked key stops within a minute)
}

// Server is the HTTP receiver.
type Server struct {
	cfg     Config
	limiter *limiter
	keys    *ttlCache[App]
	places  *ttlCache[map[string]bool]
	mu      sync.Mutex
	daily   map[dayApp]int
	stats   map[statKey]*StatRow
}

type dayApp struct {
	day time.Time
	app string
}

type statKey struct {
	day    time.Time
	app    string
	reason Reason
}

func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.RatePerMinute <= 0 {
		cfg.RatePerMinute = 60
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 20
	}
	if cfg.DailyCap <= 0 {
		cfg.DailyCap = 200_000
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Second
	}
	return &Server{
		cfg:     cfg,
		limiter: newLimiter(float64(cfg.RatePerMinute)/60, float64(cfg.Burst)),
		keys:    newTTLCache[App](cfg.CacheTTL, 1000),
		places:  newTTLCache[map[string]bool](cfg.CacheTTL, 1),
		daily:   map[dayApp]int{},
		stats:   map[statKey]*StatRow{},
	}
}

// Handler serves POST /v1/batch and GET /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/batch", s.serveBatch)
	mux.HandleFunc("GET /healthz", s.serveHealth)
	return s.recoverer(mux)
}

// HashKey is the stored form of an app key.
func HashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.cfg.Store.Ping(ctx); err != nil {
		s.cfg.Log.Warn("healthz: store unavailable", "err", err)
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "ok\n")
}

// serveBatch maps outcomes to the codes the client understands (SinkResult in Kirian):
// 204 accepted; 400, 401, 413 the client drops the batch; 429 and 5xx it retries later.
func (s *Server) serveBatch(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC()
	day := utcDay(now)
	ctx := r.Context()

	// The IP is only a key of the in-memory limiter; it is never stored or logged.
	if !s.limiter.allow(clientIP(r), now) {
		s.count(day, "", ReasonRateLimited, 0)
		reply(w, http.StatusTooManyRequests)
		return
	}

	key := r.Header.Get("X-Telemetry-Key")
	if key == "" || len(key) > maxKeyBytes {
		s.count(day, "", ReasonBadKey, 0)
		reply(w, http.StatusUnauthorized)
		return
	}
	app, ok, err := s.lookupKey(ctx, HashKey(key), now)
	if err != nil {
		s.cfg.Log.Error("key lookup", "err", err)
		s.count(day, "", ReasonStoreError, 0)
		reply(w, http.StatusServiceUnavailable)
		return
	}
	if !ok {
		s.count(day, "", ReasonBadKey, 0)
		reply(w, http.StatusUnauthorized)
		return
	}

	if r.ContentLength > MaxBody {
		s.count(day, app.ID, ReasonTooLarge, 0)
		reply(w, http.StatusRequestEntityTooLarge)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		s.count(day, app.ID, ReasonBadShape, 0)
		reply(w, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.count(day, app.ID, ReasonTooLarge, 0)
			reply(w, http.StatusRequestEntityTooLarge)
			return
		}
		s.count(day, app.ID, ReasonBadShape, 0)
		reply(w, http.StatusBadRequest)
		return
	}

	env, reason, err := ParseEnvelope(body)
	if err != nil {
		s.count(day, app.ID, reason, 0)
		reply(w, http.StatusBadRequest)
		return
	}
	if env.App != app.ID {
		s.count(day, app.ID, ReasonAppMismatch, len(env.Events))
		reply(w, http.StatusUnauthorized)
		return
	}

	var evs []Event
	drops := map[Reason]int{}
	clamped := 0
	country := ""
	if app.StoreCountry {
		country = Country(r.Header.Get("CF-IPCountry"))
	}
	for _, raw := range env.Events {
		ev, c, why := ValidateEvent(raw, now)
		if why != "" {
			drops[why]++
			continue
		}
		if c {
			clamped++
		}
		ev.Country = country
		evs = append(evs, ev)
	}
	if hasCampaign(evs) {
		places, err := s.placements(ctx, now)
		if err != nil {
			s.cfg.Log.Error("qr placements", "err", err)
			s.count(day, app.ID, ReasonStoreError, len(evs))
			reply(w, http.StatusServiceUnavailable)
			return
		}
		for i := range evs {
			NormalizeCampaign(&evs[i], places)
		}
	}

	if !s.reserve(ctx, day, app.ID, len(evs)) {
		s.count(day, app.ID, ReasonDailyCap, len(evs))
		reply(w, http.StatusTooManyRequests)
		return
	}
	inserted := 0
	if len(evs) > 0 {
		inserted, err = s.cfg.Store.Insert(ctx, app.ID, now, evs)
		if err != nil {
			s.release(day, app.ID, len(evs))
			s.cfg.Log.Error("insert events", "app", app.ID, "err", err)
			s.count(day, app.ID, ReasonStoreError, len(evs))
			reply(w, http.StatusServiceUnavailable)
			return
		}
	}
	s.count(day, app.ID, ReasonAccepted, inserted)
	if d := len(evs) - inserted; d > 0 {
		s.countEvents(day, app.ID, ReasonDuplicate, d)
	}
	if clamped > 0 {
		s.countEvents(day, app.ID, ReasonClampedTime, clamped)
	}
	for why, n := range drops {
		s.countEvents(day, app.ID, why, n)
	}
	reply(w, http.StatusNoContent)
}

func reply(w http.ResponseWriter, code int) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
}

func hasCampaign(evs []Event) bool {
	for _, e := range evs {
		if _, ok := e.Props["campaign"]; ok && e.Name == "app_open" {
			return true
		}
	}
	return false
}

func (s *Server) lookupKey(ctx context.Context, hash string, now time.Time) (App, bool, error) {
	if a, ok, hit := s.keys.get(hash, now); hit {
		return a, ok, nil
	}
	a, ok, err := s.cfg.Store.LookupKey(ctx, hash)
	if err != nil {
		return App{}, false, err
	}
	s.keys.put(hash, a, ok, now)
	return a, ok, nil
}

func (s *Server) placements(ctx context.Context, now time.Time) (map[string]bool, error) {
	if p, _, hit := s.places.get("", now); hit {
		return p, nil
	}
	p, err := s.cfg.Store.Placements(ctx)
	if err != nil {
		return nil, err
	}
	s.places.put("", p, true, now)
	return p, nil
}

// reserve takes n events from the app's daily cap. The first use of a day restores the count from the store.
func (s *Server) reserve(ctx context.Context, day time.Time, app string, n int) bool {
	k := dayApp{day, app}
	s.mu.Lock()
	_, known := s.daily[k]
	s.mu.Unlock()
	if !known {
		stored, err := s.cfg.Store.AcceptedOn(ctx, app, day)
		if err != nil {
			s.cfg.Log.Warn("daily cap: cannot read accepted count, starting from 0", "app", app, "err", err)
		}
		s.mu.Lock()
		for d := range s.daily {
			if d.day.Before(day) {
				delete(s.daily, d)
			}
		}
		if _, again := s.daily[k]; !again {
			s.daily[k] = stored
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.daily[k]+n > s.cfg.DailyCap {
		return false
	}
	s.daily[k] += n
	return true
}

func (s *Server) release(day time.Time, app string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.daily[dayApp{day, app}] -= n
}

// count adds one request and n events to a stat row.
func (s *Server) count(day time.Time, app string, reason Reason, n int) {
	s.add(day, app, reason, 1, n)
}

// countEvents adds n events without a request (event-level outcomes of an accepted batch).
func (s *Server) countEvents(day time.Time, app string, reason Reason, n int) {
	s.add(day, app, reason, 0, n)
}

func (s *Server) add(day time.Time, app string, reason Reason, reqs, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := statKey{day, app, reason}
	r := s.stats[k]
	if r == nil {
		r = &StatRow{Day: day, App: app, Reason: reason}
		s.stats[k] = r
	}
	r.Requests += reqs
	r.Events += n
}

// FlushStats writes the counters gathered since the last flush. On failure they are kept for the next try.
func (s *Server) FlushStats(ctx context.Context) error {
	s.mu.Lock()
	rows := make([]StatRow, 0, len(s.stats))
	for _, r := range s.stats {
		rows = append(rows, *r)
	}
	s.stats = map[statKey]*StatRow{}
	s.mu.Unlock()
	if len(rows) == 0 {
		return nil
	}
	if err := s.cfg.Store.AddStats(ctx, rows); err != nil {
		for _, r := range rows {
			s.add(r.Day, r.App, r.Reason, r.Requests, r.Events)
		}
		return err
	}
	return nil
}

// recoverer turns a panic into 500 without net/http's default log line (which would contain the client address).
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.cfg.Log.Error("panic in handler", "panic", v, "path", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP is the limiter key: CF-Connecting-IP behind the tunnel, else the TCP peer.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// limiter is a per-IP token bucket kept only in memory.
type limiter struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		// A bucket idle long enough to be full again carries no information: forget the IP.
		full := time.Duration(l.burst / l.rate * float64(time.Second))
		for k, b := range l.buckets {
			if now.Sub(b.last) > full {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ttlCache remembers positive and negative lookups for a short time.
type ttlCache[V any] struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	data map[string]cached[V]
}

type cached[V any] struct {
	v   V
	ok  bool
	exp time.Time
}

func newTTLCache[V any](ttl time.Duration, max int) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, max: max, data: map[string]cached[V]{}}
}

func (c *ttlCache[V]) get(k string, now time.Time) (v V, ok, hit bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.data[k]
	if !found || !now.Before(e.exp) {
		return v, false, false
	}
	return e.v, e.ok, true
}

func (c *ttlCache[V]) put(k string, v V, ok bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) >= c.max {
		c.data = map[string]cached[V]{}
	}
	c.data[k] = cached[V]{v: v, ok: ok, exp: now.Add(c.ttl)}
}
