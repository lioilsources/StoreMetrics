// Package google downloads Play Console bulk reports from the developer GCS bucket and normalizes them.
//
// Files (monthly, rewritten during the month, UTF-16 CSV):
//
//	stats/installs/installs_<pkg>_<YYYYMM>_country.csv
//	stats/store_performance/store_performance_<pkg>_<YYYYMM>_country.csv
//	stats/store_performance/store_performance_<pkg>_<YYYYMM>_traffic_source.csv
//	sales/salesreport_<YYYYMM>.zip (all apps, UTF-8 CSV inside)
//
// Kinds: "installs" (downloads = daily user installs), "store_performance" (page_views = listing visitors,
// downloads = listing acquisitions), "traffic" (same, split by source/UTM) and "sales" (units, proceeds).
// Never sum downloads across kinds.
package google

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/lioilsources/store-metrics/internal/db"
)

type Client struct {
	Bucket string
	sa     struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	key  *rsa.PrivateKey
	tok  string
	HTTP *http.Client
}

type Object struct {
	Name    string `json:"name"`
	Size    string `json:"size"`
	Updated string `json:"updated"`
}

func New(saPath, bucket string) (*Client, error) {
	b, err := os.ReadFile(saPath)
	if err != nil {
		return nil, err
	}
	c := &Client{Bucket: bucket, HTTP: &http.Client{Timeout: 120 * time.Second}}
	if err := json.Unmarshal(b, &c.sa); err != nil {
		return nil, err
	}
	blk, _ := pem.Decode([]byte(c.sa.PrivateKey))
	if blk == nil {
		return nil, errors.New("service account: bad private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("service account: not RSA")
	}
	c.key = rk
	return c, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (c *Client) token() (string, error) {
	if c.tok != "" {
		return c.tok, nil
	}
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	now := time.Now().Unix()
	p, _ := json.Marshal(map[string]any{"iss": c.sa.ClientEmail, "scope": "https://www.googleapis.com/auth/devstorage.read_only",
		"aud": c.sa.TokenURI, "iat": now, "exp": now + 3000})
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.PostForm(c.sa.TokenURI, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {in + "." + b64(sig)}})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&t)
	if t.AccessToken == "" {
		return "", fmt.Errorf("google token: %s", t.Error)
	}
	c.tok = t.AccessToken
	return c.tok, nil
}

func (c *Client) get(u string) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GCS %d: %.300s", resp.StatusCode, body)
	}
	return body, nil
}

// List returns all objects under prefix.
func (c *Client) List(prefix string) ([]Object, error) {
	var out []Object
	page := ""
	for {
		q := url.Values{"prefix": {prefix}, "maxResults": {"1000"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		b, err := c.get("https://storage.googleapis.com/storage/v1/b/" + c.Bucket + "/o?" + q.Encode())
		if err != nil {
			return nil, err
		}
		var r struct {
			Items []Object `json:"items"`
			Next  string   `json:"nextPageToken"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Items...)
		if r.Next == "" {
			return out, nil
		}
		page = r.Next
	}
}

func (c *Client) Download(name string) ([]byte, error) {
	return c.get("https://storage.googleapis.com/download/storage/v1/b/" + c.Bucket + "/o/" + url.PathEscape(name) + "?alt=media")
}

// Classify returns (kind, package, month) for a report object name, or kind "" when it is not one we load.
func Classify(name string) (kind, pkg string, month time.Time) {
	base := name[strings.LastIndex(name, "/")+1:]
	var prefix, suffix string
	switch {
	case strings.HasPrefix(name, "stats/installs/") && strings.HasSuffix(base, "_country.csv"):
		kind, prefix, suffix = "installs", "installs_", "_country.csv"
	case strings.HasPrefix(name, "stats/store_performance/") && strings.HasSuffix(base, "_country.csv"):
		kind, prefix, suffix = "store_performance", "store_performance_", "_country.csv"
	case strings.HasPrefix(name, "stats/store_performance/") && strings.HasSuffix(base, "_traffic_source.csv"):
		kind, prefix, suffix = "traffic", "store_performance_", "_traffic_source.csv"
	default:
		return "", "", time.Time{}
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(base, prefix), suffix) // <pkg>_<YYYYMM>
	i := strings.LastIndex(mid, "_")
	if i < 0 {
		return "", "", time.Time{}
	}
	m, err := time.Parse("200601", mid[i+1:])
	if err != nil {
		return "", "", time.Time{}
	}
	return kind, mid[:i], m
}

// decode handles UTF-16 (with BOM) and plain UTF-8 CSV.
func decode(b []byte) []byte {
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		out, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder(), b)
		if err == nil {
			return out
		}
	}
	return bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))
}

// Parse normalizes one report CSV of the given kind for app.
func Parse(kind, app string, raw []byte) ([]db.Row, error) {
	r := csv.NewReader(bytes.NewReader(decode(raw)))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) < 1 {
		return nil, nil
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	find := func(rec []string, names ...string) string {
		for _, n := range names {
			if i, ok := col[n]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
		}
		return ""
	}
	num := func(rec []string, names ...string) int {
		s := strings.ReplaceAll(find(rec, names...), ",", "")
		f, _ := strconv.ParseFloat(s, 64)
		return int(f)
	}
	var out []db.Row
	for _, rec := range recs[1:] {
		d, err := time.Parse("2006-01-02", find(rec, "date"))
		if err != nil {
			continue
		}
		row := db.Row{Date: d, Store: "google", AppID: app, Country: find(rec, "country", "country/region")}
		switch kind {
		case "installs":
			row.Downloads = num(rec, "daily user installs", "install events")
			row.Uninstalls = num(rec, "daily user uninstalls", "uninstall events")
			row.Updates = num(rec, "daily device upgrades", "update events")
			row.ActiveDevices = num(rec, "active device installs")
		case "store_performance", "traffic":
			row.PageViews = num(rec, "store listing visitors")
			row.Downloads = num(rec, "store listing acquisitions")
			if kind == "traffic" {
				row.Source = normSource(find(rec, "traffic source"))
				row.Campaign = find(rec, "utm campaign")
				if row.Campaign != "" && find(rec, "utm source") == "qr" {
					row.Source = "qr"
				}
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// SalesMonth returns the month of a sales report object (sales/salesreport_<YYYYMM>.zip), ok=false otherwise.
func SalesMonth(name string) (time.Time, bool) {
	base := strings.TrimPrefix(name, "sales/salesreport_")
	if base == name || !strings.HasSuffix(base, ".zip") {
		return time.Time{}, false
	}
	m, err := time.Parse("200601", strings.TrimSuffix(base, ".zip"))
	return m, err == nil
}

// ParseSales reads a monthly sales report zip (one UTF-8 CSV, one row per order event) into daily rows of
// kind "sales": units = charged orders minus refunds/chargebacks, proceeds = Item Price (the price without
// the tax Google collected, before Google's service fee — Play has no net figure in this report).
// Buyer city, state and postal code are never read. appOf maps a package to an app id ("" = unknown).
func ParseSales(zipBytes []byte, appOf func(string) string) (rows []db.Row, unknown []string, err error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, nil, err
	}
	type key struct {
		date             time.Time
		app, country, cc string
	}
	agg := map[key]*db.Row{}
	var order []key
	seen := map[string]bool{}
	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, nil, err
		}
		r := csv.NewReader(bytes.NewReader(decode(b)))
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		recs, err := r.ReadAll()
		if err != nil {
			return nil, nil, err
		}
		if len(recs) < 1 {
			continue
		}
		col := map[string]int{}
		for i, h := range recs[0] {
			col[strings.ToLower(strings.TrimSpace(h))] = i
		}
		get := func(rec []string, n string) string {
			if i, ok := col[n]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		for _, rec := range recs[1:] {
			d, err := time.Parse("2006-01-02", get(rec, "order charged date"))
			if err != nil {
				continue
			}
			sign := 0
			switch st := strings.ToLower(get(rec, "financial status")); {
			case st == "charged":
				sign = 1
			case strings.Contains(st, "refund"), strings.Contains(st, "chargeback"):
				sign = -1
			}
			if sign == 0 {
				continue
			}
			pkg := get(rec, "package id")
			app := appOf(pkg)
			if app == "" {
				if !seen[pkg] {
					seen[pkg] = true
					unknown = append(unknown, pkg)
				}
				continue
			}
			price, _ := strconv.ParseFloat(strings.ReplaceAll(get(rec, "item price"), ",", ""), 64)
			if price < 0 {
				price = -price
			}
			k := key{d, app, get(rec, "country of buyer"), get(rec, "currency of sale")}
			row := agg[k]
			if row == nil {
				row = &db.Row{Date: d, Store: "google", AppID: app, Country: k.country, ProceedsCcy: k.cc}
				agg[k] = row
				order = append(order, k)
			}
			row.Units += sign
			row.Proceeds += float64(sign) * price
		}
	}
	for _, k := range order {
		rows = append(rows, *agg[k])
	}
	return rows, unknown, nil
}

func normSource(s string) string {
	s = strings.ToLower(s)
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "search"):
		return "search"
	case strings.Contains(s, "explore"), strings.Contains(s, "browse"):
		return "browse"
	case strings.Contains(s, "third-party"), strings.Contains(s, "referral"), strings.Contains(s, "utm"):
		return "campaign"
	default:
		return "other"
	}
}
