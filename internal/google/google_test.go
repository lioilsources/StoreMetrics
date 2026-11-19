package google

import (
	"archive/zip"
	"bytes"
	"testing"

	"golang.org/x/text/encoding/unicode"
)

func TestClassify(t *testing.T) {
	k, p, m := Classify("stats/installs/installs_com.ab.SwypeKids_202610_country.csv")
	if k != "installs" || p != "com.ab.SwypeKids" || m.Format("200601") != "202610" {
		t.Fatal(k, p, m)
	}
	k, p, _ = Classify("stats/store_performance/store_performance_com.ol1n.puff_202611_traffic_source.csv")
	if k != "traffic" || p != "com.ol1n.puff" {
		t.Fatal(k, p)
	}
	if k, _, _ := Classify("stats/installs/installs_com.ol1n.puff_202611_overview.csv"); k != "" {
		t.Fatal("overview should be skipped")
	}
}

func TestSales(t *testing.T) {
	if m, ok := SalesMonth("sales/salesreport_202611.zip"); !ok || m.Format("200601") != "202611" {
		t.Fatal(m, ok)
	}
	if _, ok := SalesMonth("stats/installs/installs_com.ol1n.puff_202611_country.csv"); ok {
		t.Fatal("not a sales report")
	}
	csv := "\xEF\xBB\xBFOrder Number,Order Charged Date,Financial Status,Package ID,Currency of Sale,Item Price,Taxes Collected,Charged Amount,City of Buyer,Country of Buyer\n" +
		"GPA.1,2026-11-09,Charged,com.ol1n.kaceny,CZK,21.48,4.51,25.99,X,CZ\n" +
		"GPA.2,2026-11-09,Charged,com.ol1n.kaceny,CZK,21.48,4.51,25.99,Y,CZ\n" +
		"GPA.2,2026-11-10,Refund,com.ol1n.kaceny,CZK,-21.48,-4.51,-25.99,Y,CZ\n" +
		"GPA.3,2026-11-10,Cancelled,com.ol1n.kaceny,CZK,21.48,4.51,25.99,Y,CZ\n" +
		"GPA.4,2026-11-10,Charged,com.other.app,EUR,1.00,0.20,1.20,Z,DE\n"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("salesreport_202611.csv")
	w.Write([]byte(csv))
	zw.Close()
	rows, unknown, err := ParseSales(buf.Bytes(), func(p string) string {
		if p == "com.ol1n.kaceny" {
			return "kaceny"
		}
		return ""
	})
	if err != nil || len(rows) != 2 || len(unknown) != 1 || unknown[0] != "com.other.app" {
		t.Fatalf("%v %+v %v", err, rows, unknown)
	}
	if r := rows[0]; r.AppID != "kaceny" || r.Units != 2 || r.Proceeds < 42.95 || r.Proceeds > 42.97 || r.ProceedsCcy != "CZK" || r.Country != "CZ" {
		t.Fatalf("%+v", r)
	}
	if r := rows[1]; r.Units != -1 || r.Proceeds > -21.47 || r.Date.Format("2006-01-02") != "2026-11-10" {
		t.Fatalf("%+v", r)
	}
}

func TestParseUTF16(t *testing.T) {
	csv := "Date,Package Name,Country,Daily Device Installs,Daily Device Uninstalls,Daily Device Upgrades,Total User Installs,Daily User Installs,Daily User Uninstalls,Active Device Installs\n" +
		"2026-11-01,com.ol1n.puff,CZ,3,1,0,40,2,1,25\n2026-11-02,com.ol1n.puff,DE,1,0,4,41,1,0,26\n"
	enc, _ := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(csv))
	rows, err := Parse("installs", "puff", enc)
	if err != nil || len(rows) != 2 {
		t.Fatal(err, len(rows))
	}
	if rows[0].Downloads != 2 || rows[0].Uninstalls != 1 || rows[0].ActiveDevices != 25 || rows[1].Updates != 4 || rows[1].Country != "DE" {
		t.Fatalf("%+v", rows)
	}
}
