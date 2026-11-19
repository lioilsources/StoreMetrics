package cloudflare

import "testing"

func TestParseZone(t *testing.T) {
	b := []byte(`{"data":{"viewer":{"zones":[{"httpRequests1dGroups":[
	 {"dimensions":{"date":"2026-11-18"},"sum":{"requests":120,"pageViews":40,"countryMap":[{"clientCountryName":"CZ","requests":100},{"clientCountryName":"XX","requests":20}]},"uniq":{"uniques":15}}
	]}]}},"errors":null}`)
	zone, country, err := ParseZone(b, "olin.now")
	if err != nil || len(zone) != 1 || len(country) != 2 {
		t.Fatal(err, zone, country)
	}
	if z := zone[0]; z.Store != "web" || z.Impressions != 120 || z.PageViews != 40 || z.ActiveDevices != 15 || z.Date.Format("2006-01-02") != "2026-11-18" {
		t.Fatalf("%+v", z)
	}
	if country[0].Country != "CZ" || country[0].Impressions != 100 || country[1].Country != "" {
		t.Fatalf("%+v", country)
	}
	if _, _, err := ParseZone([]byte(`{"data":{"viewer":{"zones":[]}}}`), "olin.now"); err == nil {
		t.Fatal("missing zone should fail")
	}
}

func TestParseVisits(t *testing.T) {
	b := []byte(`{"data":{"viewer":{"accounts":[{"rumPageloadEventsAdaptiveGroups":[
	 {"count":5,"sum":{"visits":3},"dimensions":{"date":"2026-11-18","countryName":"DE","requestHost":"olin.now","requestPath":"/mirrorbooth/support?x=1","refererHost":"apps.apple.com"}}
	]}]}}}`)
	rows, err := ParseVisits(b, "olin.now")
	if err != nil || len(rows) != 1 {
		t.Fatal(err, rows)
	}
	if r := rows[0]; r.Country != "DE" || r.Source != "olin.now/mirrorbooth/support" || r.Campaign != "apps.apple.com" || r.PageViews != 5 || r.Impressions != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestParseSites(t *testing.T) {
	s, err := ParseSites("olin.now:z1:t1, ol1n.com:z2")
	if err != nil || len(s) != 2 || s[0] != (Site{"olin.now", "z1", "t1"}) || s[1] != (Site{"ol1n.com", "z2", ""}) {
		t.Fatal(err, s)
	}
	if _, err := ParseSites("olin.now"); err == nil {
		t.Fatal("missing zone should fail")
	}
}
