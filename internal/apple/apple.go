// Package apple downloads App Store Connect daily Sales reports and normalizes them.
package apple

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/rand"
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

	"github.com/lioilsources/store-metrics/internal/db"
)

// ErrNotYet means Apple has not published the report for that day yet.
var ErrNotYet = errors.New("report not available yet")

type Client struct {
	KeyID, IssuerID, Vendor string
	key                     *ecdsa.PrivateKey
	HTTP                    *http.Client
}

func New(keyID, issuer, keyPath, vendor string) (*Client, error) {
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("asc key: not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("asc key: not ECDSA")
	}
	return &Client{KeyID: keyID, IssuerID: issuer, Vendor: vendor, key: ek, HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (c *Client) token() (string, error) {
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.KeyID, "typ": "JWT"})
	now := time.Now().Unix()
	p, _ := json.Marshal(map[string]any{"iss": c.IssuerID, "iat": now, "exp": now + 1100, "aud": "appstoreconnect-v1"})
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + b64(sig), nil
}

// SalesTSV returns the raw (decompressed) DAILY SALES SUMMARY report for day.
func (c *Client) SalesTSV(day time.Time) ([]byte, error) {
	q := url.Values{}
	q.Set("filter[frequency]", "DAILY")
	q.Set("filter[reportType]", "SALES")
	q.Set("filter[reportSubType]", "SUMMARY")
	q.Set("filter[vendorNumber]", c.Vendor)
	q.Set("filter[reportDate]", day.Format("2006-01-02"))
	q.Set("filter[version]", "1_1")
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", "https://api.appstoreconnect.apple.com/v1/salesReports?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/a-gzip")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 404 {
		return nil, ErrNotYet
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("salesReports %s: %d %s", day.Format("2006-01-02"), resp.StatusCode, truncate(body, 300))
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// Parse turns a Sales SUMMARY TSV into rows. appOf maps an SKU or Parent Identifier to an internal app id.
// Product Type Identifier: 1*/F1 download, 3*/F3 redownload, 7*/F7 update, IA*/FI1 in-app purchase.
func Parse(tsv []byte, appOf func(string) string) ([]db.Row, []string, error) {
	r := csv.NewReader(bytes.NewReader(tsv))
	r.Comma = '\t'
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil || len(recs) == 0 {
		return nil, nil, err
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[strings.TrimSpace(h)] = i
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	type key struct{ date, app, country, device, ccy string }
	agg := map[key]*db.Row{}
	var unknown []string
	for _, rec := range recs[1:] {
		sku, parent := get(rec, "SKU"), get(rec, "Parent Identifier")
		app := appOf(sku)
		if app == "" {
			app = appOf(parent)
		}
		if app == "" {
			unknown = append(unknown, sku+" ("+get(rec, "Title")+")")
			continue
		}
		d, err := time.Parse("01/02/2006", get(rec, "Begin Date"))
		if err != nil {
			continue
		}
		units, _ := strconv.Atoi(get(rec, "Units"))
		pt := get(rec, "Product Type Identifier")
		isIAP := strings.HasPrefix(pt, "IA") || pt == "FI1"
		ccy := ""
		if isIAP {
			ccy = get(rec, "Currency of Proceeds")
		}
		k := key{d.Format("2006-01-02"), app, get(rec, "Country Code"), get(rec, "Device"), ccy}
		row := agg[k]
		if row == nil {
			row = &db.Row{Date: d, Store: "apple", AppID: app, Country: k.country, Device: k.device, ProceedsCcy: ccy}
			agg[k] = row
		}
		base := strings.TrimPrefix(pt, "F")
		switch {
		case isIAP:
			p, _ := strconv.ParseFloat(get(rec, "Developer Proceeds"), 64)
			row.Units += units
			row.Proceeds += float64(units) * p
		case strings.HasPrefix(base, "1"):
			row.Downloads += units
		case strings.HasPrefix(base, "3"):
			row.Redownloads += units
		case strings.HasPrefix(base, "7"):
			row.Updates += units
		}
	}
	out := make([]db.Row, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	return out, unknown, nil
}
