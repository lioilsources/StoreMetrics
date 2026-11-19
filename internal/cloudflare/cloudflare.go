// Package cloudflare reads daily website metrics (olin.now, ol1n.com) from the Cloudflare GraphQL Analytics API.
//
// Two sources, both read-only (API token with Zone > Analytics > Read and Account > Account Analytics > Read):
//   - zone traffic (httpRequests1dGroups): requests, page views and unique visitors per day, requests per country.
//     Counts every client, bots included.
//   - Web Analytics (rumPageloadEventsAdaptiveGroups): real browser page loads and visits per day, country,
//     path and referring host. Only when Web Analytics is enabled for the site (siteTag).
//
// Rows go into metrics_daily with store "web"; the column mapping is documented on each kind below.
package cloudflare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lioilsources/store-metrics/internal/db"
)

const endpoint = "https://api.cloudflare.com/client/v4/graphql"

// Kinds written to metrics_daily (store "web").
const (
	// KindZone: one row per day, country "": impressions = requests, page_views = page views, active_devices = unique visitors.
	KindZone = "zone"
	// KindZoneCountry: one row per day and country: impressions = requests.
	KindZoneCountry = "zone_country"
	// KindVisits: per day, country, host + path (source) and referring host (campaign): page_views = page loads, impressions = visits.
	KindVisits = "visits"
)

type Client struct {
	token string
	http  *http.Client
}

func New(token string) (*Client, error) {
	if token == "" {
		return nil, errors.New("CF_API_TOKEN not set")
	}
	return &Client{token: token, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (c *Client) query(q string, vars map[string]any) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cloudflare: HTTP %d: %.200s", resp.StatusCode, b)
	}
	var e struct {
		Errors []struct{ Message string } `json:"errors"`
	}
	_ = json.Unmarshal(b, &e)
	if len(e.Errors) > 0 {
		var msgs []string
		for _, m := range e.Errors {
			msgs = append(msgs, m.Message)
		}
		return nil, fmt.Errorf("cloudflare: %s", strings.Join(msgs, "; "))
	}
	return b, nil
}

const zoneQuery = `query($zone: String!, $from: Date!, $to: Date!) {
  viewer { zones(filter: {zoneTag: $zone}) {
    httpRequests1dGroups(limit: 400, filter: {date_geq: $from, date_leq: $to}) {
      dimensions { date }
      sum { requests pageViews countryMap { clientCountryName requests } }
      uniq { uniques }
    }
  } }
}`

// Zone returns KindZone and KindZoneCountry rows for [from, to].
func (c *Client) Zone(zoneID, app string, from, to time.Time) (zone, country []db.Row, err error) {
	b, err := c.query(zoneQuery, map[string]any{"zone": zoneID, "from": day(from), "to": day(to)})
	if err != nil {
		return nil, nil, err
	}
	return ParseZone(b, app)
}

func ParseZone(b []byte, app string) (zone, country []db.Row, err error) {
	var r struct {
		Data struct {
			Viewer struct {
				Zones []struct {
					Groups []struct {
						Dimensions struct{ Date string }
						Sum        struct {
							Requests   int
							PageViews  int
							CountryMap []struct {
								ClientCountryName string
								Requests          int
							}
						}
						Uniq struct{ Uniques int }
					} `json:"httpRequests1dGroups"`
				}
			}
		}
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, nil, err
	}
	if len(r.Data.Viewer.Zones) == 0 {
		return nil, nil, errors.New("cloudflare: zone not found (check CF_ZONE_ID and token permissions)")
	}
	for _, g := range r.Data.Viewer.Zones[0].Groups {
		d, err := time.Parse("2006-01-02", g.Dimensions.Date)
		if err != nil {
			return nil, nil, err
		}
		zone = append(zone, db.Row{Date: d, Store: "web", AppID: app, Impressions: g.Sum.Requests, PageViews: g.Sum.PageViews, ActiveDevices: g.Uniq.Uniques})
		for _, cm := range g.Sum.CountryMap {
			country = append(country, db.Row{Date: d, Store: "web", AppID: app, Country: countryCode(cm.ClientCountryName), Impressions: cm.Requests})
		}
	}
	return zone, country, nil
}

const rumQuery = `query($account: String!, $site: String!, $from: Date!, $to: Date!) {
  viewer { accounts(filter: {accountTag: $account}) {
    rumPageloadEventsAdaptiveGroups(limit: 5000, filter: {siteTag: $site, date_geq: $from, date_leq: $to}) {
      count
      sum { visits }
      dimensions { date countryName requestHost requestPath refererHost }
    }
  } }
}`

// Visits returns KindVisits rows (Web Analytics) for [from, to].
func (c *Client) Visits(accountID, siteTag, app string, from, to time.Time) ([]db.Row, error) {
	b, err := c.query(rumQuery, map[string]any{"account": accountID, "site": siteTag, "from": day(from), "to": day(to)})
	if err != nil {
		return nil, err
	}
	return ParseVisits(b, app)
}

func ParseVisits(b []byte, app string) ([]db.Row, error) {
	var r struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Groups []struct {
						Count      int
						Sum        struct{ Visits int }
						Dimensions struct {
							Date        string
							CountryName string
							RequestHost string
							RequestPath string
							RefererHost string
						}
					} `json:"rumPageloadEventsAdaptiveGroups"`
				}
			}
		}
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if len(r.Data.Viewer.Accounts) == 0 {
		return nil, errors.New("cloudflare: account not found (check CF_ACCOUNT_ID and token permissions)")
	}
	var rows []db.Row
	for _, g := range r.Data.Viewer.Accounts[0].Groups {
		d, err := time.Parse("2006-01-02", g.Dimensions.Date)
		if err != nil {
			return nil, err
		}
		rows = append(rows, db.Row{Date: d, Store: "web", AppID: app, Country: countryCode(g.Dimensions.CountryName),
			Source: cut(g.Dimensions.RequestHost+g.Dimensions.RequestPath, 200), Campaign: cut(g.Dimensions.RefererHost, 200), PageViews: g.Count, Impressions: g.Sum.Visits})
	}
	return rows, nil
}

// Site is one website: app_id in metrics_daily, its zone and (optional) Web Analytics site tag.
type Site struct{ App, Zone, SiteTag string }

// ParseSites reads CF_SITES: comma-separated "app:zone_id[:site_tag]" entries.
func ParseSites(s string) ([]Site, error) {
	var out []Site
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		f := strings.Split(e, ":")
		if len(f) < 2 || len(f) > 3 || f[0] == "" || f[1] == "" {
			return nil, fmt.Errorf("CF_SITES: bad entry %q (want app:zone_id[:site_tag])", e)
		}
		st := Site{App: f[0], Zone: f[1]}
		if len(f) == 3 {
			st.SiteTag = f[2]
		}
		out = append(out, st)
	}
	return out, nil
}

func day(t time.Time) string { return t.Format("2006-01-02") }

// countryCode keeps ISO 3166 alpha-2 codes as the other stores use; Cloudflare's "XX"/"T1" (unknown, Tor) become "".
func countryCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 2 || s == "XX" || s == "T1" {
		return ""
	}
	return s
}

// cut drops query strings and limits length: paths and hosts only, nothing that could identify a visitor.
func cut(s string, n int) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n]
	}
	return s
}
