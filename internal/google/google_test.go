package google

import (
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
