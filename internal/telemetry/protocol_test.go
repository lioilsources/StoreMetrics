package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

const sess = "0123456789abcdef0123456789abcdef"

func event(n, t, s, i, p string) json.RawMessage {
	parts := []string{}
	if n != "-" {
		parts = append(parts, `"n":`+n)
	}
	if t != "-" {
		parts = append(parts, `"t":`+t)
	}
	if s != "-" {
		parts = append(parts, `"s":`+s)
	}
	if i != "" {
		parts = append(parts, `"i":`+i)
	}
	switch p {
	case "-":
	case "":
		parts = append(parts, `"p":{}`)
	default:
		parts = append(parts, `"p":`+p)
	}
	return json.RawMessage("{" + strings.Join(parts, ",") + "}")
}

func TestValidateEvent(t *testing.T) {
	okT := `"2026-11-14T10:00:00.000Z"`
	okS := `"` + sess + `"`
	long := strings.Repeat("a", 41)
	manyProps := func(n int) string {
		var b []string
		for i := range n {
			b = append(b, `"k`+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))+`":1`)
		}
		return "{" + strings.Join(b, ",") + "}"
	}
	tests := []struct {
		name    string
		raw     json.RawMessage
		want    Reason
		clamped bool
	}{
		{"minimal", event(`"app_open"`, okT, okS, "", ""), "", false},
		{"with seq and props", event(`"screen_view"`, okT, okS, "2", `{"screen":"deck","first_session":true,"secs":0,"ver":"2.3.4+12","lang":"pt_PT","band":"3-5"}`), "", false},
		{"microseconds time", event(`"app_open"`, `"2026-11-14T10:00:00.123456Z"`, okS, "", ""), "", false},
		{"unknown field ignored", json.RawMessage(`{"n":"app_open","t":` + okT + `,"s":` + okS + `,"p":{},"x":1}`), "", false},
		{"offset time", event(`"app_open"`, `"2026-11-14T11:00:00+01:00"`, okS, "", ""), ReasonBadTime, false},
		{"uppercase session", event(`"app_open"`, okT, `"`+strings.ToUpper(sess)+`"`, "", ""), ReasonBadSession, false},
		{"max int", event(`"x_end"`, okT, okS, "1000000000", `{"secs":1000000000}`), "", false},
		{"32 props", event(`"app_open"`, okT, okS, "", manyProps(32)), "", false},

		{"not an object", json.RawMessage(`[1,2]`), ReasonBadEvent, false},
		{"name missing", event("-", okT, okS, "", ""), ReasonBadName, false},
		{"name uppercase", event(`"AppOpen"`, okT, okS, "", ""), ReasonBadName, false},
		{"name one char", event(`"a"`, okT, okS, "", ""), ReasonBadName, false},
		{"name 41 chars", event(`"`+long+`"`, okT, okS, "", ""), ReasonBadName, false},
		{"name starts with digit", event(`"1open"`, okT, okS, "", ""), ReasonBadName, false},
		{"time missing", event(`"app_open"`, "-", okS, "", ""), ReasonBadTime, false},
		{"time not rfc3339", event(`"app_open"`, `"14.11.2026 10:00"`, okS, "", ""), ReasonBadTime, false},
		{"time a number", event(`"app_open"`, `1731578400`, okS, "", ""), ReasonBadEvent, false},
		{"future 9 min kept", event(`"app_open"`, `"2026-11-14T12:09:00Z"`, okS, "", ""), "", false},
		{"future 11 min clamped", event(`"app_open"`, `"2026-11-14T12:11:00Z"`, okS, "", ""), "", true},
		{"29 days old kept", event(`"app_open"`, `"2026-10-16T12:00:00Z"`, okS, "", ""), "", false},
		{"31 days old dropped", event(`"app_open"`, `"2026-10-14T12:00:00Z"`, okS, "", ""), ReasonTooOld, false},
		{"session missing", event(`"app_open"`, okT, "-", "", ""), ReasonBadSession, false},
		{"session short", event(`"app_open"`, okT, `"abc"`, "", ""), ReasonBadSession, false},
		{"session uuid with dashes", event(`"app_open"`, okT, `"01234567-89ab-cdef-0123-456789abcdef"`, "", ""), ReasonBadSession, false},
		{"seq negative", event(`"app_open"`, okT, okS, "-1", ""), ReasonBadSeq, false},
		{"seq zero", event(`"app_open"`, okT, okS, "0", ""), ReasonBadSeq, false},
		{"props missing", event(`"app_open"`, okT, okS, "", "-"), ReasonBadProps, false},
		{"props null", event(`"app_open"`, okT, okS, "", "null"), ReasonBadProps, false},
		{"seq float", event(`"app_open"`, okT, okS, "1.5", ""), ReasonBadSeq, false},
		{"seq too big", event(`"app_open"`, okT, okS, "1000000001", ""), ReasonBadSeq, false},
		{"seq string", event(`"app_open"`, okT, okS, `"1"`, ""), ReasonBadSeq, false},
		{"seq null is no seq", event(`"app_open"`, okT, okS, `null`, ""), "", false},
		{"33 props", event(`"app_open"`, okT, okS, "", manyProps(33)), ReasonBadProps, false},
		{"prop key uppercase", event(`"app_open"`, okT, okS, "", `{"Screen":"deck"}`), ReasonBadProps, false},
		{"prop free text", event(`"app_open"`, okT, okS, "", `{"screen":"hello world"}`), ReasonBadProps, false},
		{"prop email", event(`"app_open"`, okT, okS, "", `{"who":"a@b.cz"}`), ReasonBadProps, false},
		{"prop path", event(`"app_open"`, okT, okS, "", `{"route":"/story/123"}`), ReasonBadProps, false},
		{"prop empty string", event(`"app_open"`, okT, okS, "", `{"screen":""}`), ReasonBadProps, false},
		{"prop 41 chars", event(`"app_open"`, okT, okS, "", `{"screen":"`+long+`"}`), ReasonBadProps, false},
		{"prop diacritics", event(`"app_open"`, okT, okS, "", `{"screen":"příběh"}`), ReasonBadProps, false},
		{"prop null", event(`"app_open"`, okT, okS, "", `{"screen":null}`), ReasonBadProps, false},
		{"prop float", event(`"app_open"`, okT, okS, "", `{"secs":1.5}`), ReasonBadProps, false},
		{"prop exponent", event(`"app_open"`, okT, okS, "", `{"secs":1e3}`), ReasonBadProps, false},
		{"prop negative", event(`"app_open"`, okT, okS, "", `{"secs":-1}`), ReasonBadProps, false},
		{"prop over max", event(`"app_open"`, okT, okS, "", `{"secs":1000000001}`), ReasonBadProps, false},
		{"prop array", event(`"app_open"`, okT, okS, "", `{"f":["a"]}`), ReasonBadProps, false},
		{"prop object", event(`"app_open"`, okT, okS, "", `{"f":{"a":1}}`), ReasonBadProps, false},
		{"props not object", event(`"app_open"`, okT, okS, "", `[1]`), ReasonBadEvent, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, clamped, why := ValidateEvent(tc.raw, testNow)
			if why != tc.want {
				t.Fatalf("reason = %q, want %q", why, tc.want)
			}
			if clamped != tc.clamped {
				t.Fatalf("clamped = %v, want %v", clamped, tc.clamped)
			}
			if why != "" {
				return
			}
			if clamped && !ev.Time.Equal(testNow) {
				t.Errorf("clamped time = %v, want receive time", ev.Time)
			}
			if ev.Session != strings.ToLower(ev.Session) {
				t.Errorf("session not lowercased: %s", ev.Session)
			}
			if ev.Day.Hour() != 0 || ev.Day.Location() != time.UTC {
				t.Errorf("day not UTC midnight: %v", ev.Day)
			}
		})
	}
}

func TestValidateEventTypes(t *testing.T) {
	ev, _, why := ValidateEvent(event(`"app_open"`, `"2026-11-14T10:00:00Z"`, `"`+sess+`"`, "3",
		`{"first_session":true,"tester":false,"secs":42,"screen":"deck"}`), testNow)
	if why != "" {
		t.Fatal(why)
	}
	if ev.Seq == nil || *ev.Seq != 3 {
		t.Errorf("seq = %v", ev.Seq)
	}
	if ev.Props["first_session"] != true || ev.Props["tester"] != false || ev.Props["secs"] != int64(42) || ev.Props["screen"] != "deck" {
		t.Errorf("props = %#v", ev.Props)
	}
}

func TestParseEnvelope(t *testing.T) {
	hundred := "[" + strings.TrimSuffix(strings.Repeat(`{},`, 100), ",") + "]"
	tests := []struct {
		name string
		body string
		want Reason
	}{
		{"ok", `{"v":1,"app":"lexify","events":[{}]}`, ""},
		{"empty events", `{"v":1,"app":"lexify","events":[]}`, ReasonBadShape},
		{"app with dash", `{"v":1,"app":"mirror-booth","events":[{}]}`, ""},
		{"app with underscore", `{"v":1,"app":"mirror_booth","events":[{}]}`, ReasonBadShape},
		{"app 33 chars", `{"v":1,"app":"` + strings.Repeat("a", 33) + `","events":[{}]}`, ReasonBadShape},
		{"100 events", `{"v":1,"app":"lexify","events":` + hundred + `}`, ""},
		{"101 events", `{"v":1,"app":"lexify","events":[{},` + strings.TrimPrefix(hundred, "[")[0:] + `}`, ReasonTooManyEvents},
		{"not json", `v=1`, ReasonBadShape},
		{"array", `[]`, ReasonBadShape},
		{"trailing data", `{"v":1,"app":"lexify","events":[{}]} {}`, ReasonBadShape},
		{"unknown field ignored", `{"v":1,"app":"lexify","events":[{}],"sdk":"x"}`, ""},
		{"missing version", `{"app":"lexify","events":[{}]}`, ReasonBadVersion},
		{"version 2", `{"v":2,"app":"lexify","events":[{}]}`, ReasonBadVersion},
		{"missing app", `{"v":1,"events":[{}]}`, ReasonBadShape},
		{"bad app", `{"v":1,"app":"Lexify App","events":[{}]}`, ReasonBadShape},
		{"missing events", `{"v":1,"app":"lexify"}`, ReasonBadShape},
		{"events object", `{"v":1,"app":"lexify","events":{}}`, ReasonBadShape},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, why, err := ParseEnvelope([]byte(tc.body))
			if why != tc.want || (err == nil) != (tc.want == "") {
				t.Fatalf("reason = %q err = %v, want %q", why, err, tc.want)
			}
		})
	}
}

func TestCountry(t *testing.T) {
	for in, want := range map[string]string{"CZ": "CZ", "XX": "XX", "T1": "T1", "cz": "", "CZE": "", "": "", "1.2": ""} {
		if got := Country(in); got != want {
			t.Errorf("Country(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeCampaign(t *testing.T) {
	places := map[string]bool{"flyer-cafe": true}
	tests := []struct {
		name  string
		event string
		in    any
		want  any
	}{
		{"known", "app_open", "flyer-cafe", "flyer-cafe"},
		{"unknown", "app_open", "made-up", OtherValue},
		{"bad shape", "app_open", "Flyer_Cafe", OtherValue},
		{"too long", "app_open", strings.Repeat("a", 25), OtherValue},
		{"number", "app_open", int64(5), OtherValue},
		{"other event untouched", "screen_view", "made-up", "made-up"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := Event{Name: tc.event, Props: map[string]any{"campaign": tc.in}}
			NormalizeCampaign(&ev, places)
			if ev.Props["campaign"] != tc.want {
				t.Fatalf("campaign = %v, want %v", ev.Props["campaign"], tc.want)
			}
		})
	}
	ev := Event{Name: "app_open", Props: map[string]any{}}
	NormalizeCampaign(&ev, places)
	if _, ok := ev.Props["campaign"]; ok {
		t.Fatal("campaign added to an app_open without one")
	}
}

func TestFirstOpen(t *testing.T) {
	tests := []struct {
		name     string
		ev       Event
		ok       bool
		platform string
		campaign string
	}{
		{"first", Event{Name: "app_open", Props: map[string]any{"first_session": true, "platform": "android", "campaign": "flyer-cafe"}}, true, "android", "flyer-cafe"},
		{"no campaign", Event{Name: "app_open", Props: map[string]any{"first_session": true, "platform": "ios"}}, true, "ios", ""},
		{"not first", Event{Name: "app_open", Props: map[string]any{"first_session": false}}, false, "", ""},
		{"tester", Event{Name: "app_open", Props: map[string]any{"first_session": true, "tester": true}}, false, "", ""},
		{"other event", Event{Name: "screen_view", Props: map[string]any{"first_session": true}}, false, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, c, ok := FirstOpen(tc.ev)
			if ok != tc.ok || p != tc.platform || c != tc.campaign {
				t.Fatalf("got %q %q %v", p, c, ok)
			}
		})
	}
}
