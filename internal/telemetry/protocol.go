// Package telemetry receives anonymous usage events (protocol v1, PLAN-analytika §5.1) and reports on them.
//
// The receiver enforces shape: names, enums, bands, integers and booleans only. A value that looks like
// anything else costs the event, never a leak. No IP is stored or logged; the country comes from CF-IPCountry.
//
// Specification: docs/protocol.md in github.com/lioilsources/Ol1nTelemetry. Its testdata/ is copied to
// testdata/protocol and checked against cases.json in the tests.
package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Protocol v1 limits.
const (
	Version     = 1
	MaxBody     = 64 << 10 // bytes
	MaxEvents   = 100
	MaxProps    = 32
	MaxInt      = 1_000_000_000
	FutureSkew  = 10 * time.Minute    // event times further ahead are replaced by the receive time
	MaxAge      = 30 * 24 * time.Hour // older events are dropped
	OtherValue  = "other"             // campaign that is not a known QR placement
	maxKeyBytes = 200
)

var (
	nameRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)
	stringRe   = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,40}$`)
	sessionRe  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	appRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	campaignRe = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)
	countryRe  = regexp.MustCompile(`^[A-Z][A-Z0-9]$`)
)

// Reason names a rejection or a notable outcome; it is the `reason` column of telemetry.ingest_stats.
type Reason string

const (
	// Batch level.
	ReasonAccepted      Reason = "accepted"
	ReasonBadKey        Reason = "bad_key"
	ReasonAppMismatch   Reason = "app_mismatch"
	ReasonBadShape      Reason = "bad_shape"
	ReasonBadVersion    Reason = "bad_version"
	ReasonTooManyEvents Reason = "too_many_events"
	ReasonTooLarge      Reason = "too_large"
	ReasonRateLimited   Reason = "rate_limited"
	ReasonDailyCap      Reason = "daily_cap"
	ReasonStoreError    Reason = "store_error"
	// Event level (the event is dropped, the rest of the batch is kept).
	ReasonBadEvent   Reason = "bad_event"   // not an object, a field of the wrong JSON type
	ReasonBadName    Reason = "bad_name"    // n
	ReasonBadTime    Reason = "bad_time"    // t: not RFC 3339 in UTC (Z)
	ReasonBadSession Reason = "bad_session" // s
	ReasonBadSeq     Reason = "bad_seq"     // i: not an integer 1..10^9
	ReasonBadProps   Reason = "bad_props"   // p: missing, too many, bad key, bad value
	ReasonTooOld     Reason = "too_old"
	ReasonDuplicate  Reason = "duplicate"
	// Accepted with a fix.
	ReasonClampedTime Reason = "clamped_time"
)

// Event is one validated event, ready to store. Props values are bool, int64 or string only.
type Event struct {
	Name    string
	Time    time.Time // UTC, clamped
	Day     time.Time // UTC midnight of Time
	Session string
	Seq     *int32 // nil = no duplicate check
	Country string
	Props   map[string]any
}

// Envelope is the parsed batch before event validation.
type Envelope struct {
	App    string
	Events []json.RawMessage
}

// ParseEnvelope checks the batch frame: JSON object with v=1, app and up to MaxEvents events.
func ParseEnvelope(body []byte) (Envelope, Reason, error) {
	var raw struct {
		V      *int              `json:"v"`
		App    string            `json:"app"`
		Events []json.RawMessage `json:"events"`
	}
	// Unknown fields are ignored (forward compatibility, docs/protocol.md in Ol1nTelemetry).
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return Envelope{}, ReasonBadShape, fmt.Errorf("decode batch: %w", err)
	}
	if dec.More() {
		return Envelope{}, ReasonBadShape, errors.New("trailing data after batch")
	}
	if raw.V == nil || *raw.V != Version {
		return Envelope{}, ReasonBadVersion, errors.New("unsupported protocol version")
	}
	if !appRe.MatchString(raw.App) {
		return Envelope{}, ReasonBadShape, errors.New("bad app id")
	}
	if len(raw.Events) == 0 {
		return Envelope{}, ReasonBadShape, errors.New("missing or empty events")
	}
	if len(raw.Events) > MaxEvents {
		return Envelope{}, ReasonTooManyEvents, fmt.Errorf("%d events, max %d", len(raw.Events), MaxEvents)
	}
	return Envelope{App: raw.App, Events: raw.Events}, "", nil
}

// ValidateEvent turns one raw event into an Event or names why it is dropped.
// now is the receive time. clamped reports that a future time was replaced by now.
func ValidateEvent(raw json.RawMessage, now time.Time) (ev Event, clamped bool, reason Reason) {
	var e struct {
		N string                     `json:"n"`
		T string                     `json:"t"`
		S string                     `json:"s"`
		I json.RawMessage            `json:"i"`
		P map[string]json.RawMessage `json:"p"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&e); err != nil {
		return Event{}, false, ReasonBadEvent
	}
	if !nameRe.MatchString(e.N) {
		return Event{}, false, ReasonBadName
	}
	t, err := time.Parse(time.RFC3339Nano, e.T)
	if err != nil || !strings.HasSuffix(e.T, "Z") {
		return Event{}, false, ReasonBadTime
	}
	t = t.UTC()
	now = now.UTC()
	switch {
	case t.After(now.Add(FutureSkew)):
		t, clamped = now, true
	case t.Before(now.Add(-MaxAge)):
		return Event{}, false, ReasonTooOld
	}
	s := e.S
	if !sessionRe.MatchString(s) {
		return Event{}, false, ReasonBadSession
	}
	var seq *int32
	if i := bytes.TrimSpace(e.I); len(i) > 0 && string(i) != "null" {
		n, ok := parseInt(json.Number(i))
		if !ok || n < 1 {
			return Event{}, false, ReasonBadSeq
		}
		v := int32(n)
		seq = &v
	}
	if e.P == nil || len(e.P) > MaxProps {
		return Event{}, false, ReasonBadProps
	}
	props := make(map[string]any, len(e.P))
	for k, v := range e.P {
		if !nameRe.MatchString(k) {
			return Event{}, false, ReasonBadProps
		}
		val, ok := propValue(v)
		if !ok {
			return Event{}, false, ReasonBadProps
		}
		props[k] = val
	}
	return Event{
		Name:    e.N,
		Time:    t,
		Day:     time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC),
		Session: s,
		Seq:     seq,
		Props:   props,
	}, clamped, ""
}

// propValue accepts true/false, an integer 0..MaxInt or a short enum-like string. Everything else
// (null, floats, negative numbers, arrays, objects, text with spaces) is refused.
func propValue(raw json.RawMessage) (any, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	switch raw[0] {
	case 't', 'f':
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, false
		}
		return b, true
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || !stringRe.MatchString(s) {
			return nil, false
		}
		return s, true
	default:
		n, ok := parseInt(json.Number(raw))
		return n, ok
	}
}

// parseInt accepts only plain decimal integers in 0..MaxInt (no sign, fraction or exponent).
func parseInt(n json.Number) (int64, bool) {
	s := n.String()
	if s == "" || s[0] == '-' || s[0] == '+' {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > MaxInt {
		return 0, false
	}
	return v, true
}

// Country returns a CF-IPCountry value worth keeping, or "".
func Country(header string) string {
	if countryRe.MatchString(header) {
		return header
	}
	return ""
}

// NormalizeCampaign applies §6.4 to an app_open: a campaign that is not a known QR placement becomes "other".
func NormalizeCampaign(ev *Event, placements map[string]bool) {
	if ev.Name != "app_open" {
		return
	}
	v, ok := ev.Props["campaign"]
	if !ok {
		return
	}
	s, _ := v.(string)
	if !campaignRe.MatchString(s) || !placements[s] {
		ev.Props["campaign"] = OtherValue
	}
}

// FirstOpen reports whether ev counts as a first open (public.first_open) and with which platform and campaign.
// Tester builds do not count.
func FirstOpen(ev Event) (platform, campaign string, ok bool) {
	if ev.Name != "app_open" || ev.Props["first_session"] != true || ev.Props["tester"] == true {
		return "", "", false
	}
	platform, _ = ev.Props["platform"].(string)
	campaign, _ = ev.Props["campaign"].(string)
	return platform, campaign, true
}
