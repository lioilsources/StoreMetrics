package apple

import (
	"os"
	"testing"
)

func TestParse(t *testing.T) {
	b, err := os.ReadFile("testdata/sales_example.tsv")
	if err != nil {
		t.Fatal(err)
	}
	apps := map[string]string{"com.ol1n.mirrorbooth": "mirrorbooth", "ol1npuff": "puff", "ol1nswypekids": "swypekids",
		"ol1nduolingocards": "lexify", "com.ol1n.kiran": "kirian", "com.ol1n.storyteller": "pohadkomat", "com.ol1n.PoetryStreamSC": "poesie"}
	rows, unknown, err := Parse(b, func(s string) string { return apps[s] })
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) > 0 {
		t.Errorf("unknown: %v", unknown)
	}
	dl := 0
	for _, r := range rows {
		dl += r.Downloads
	}
	if dl == 0 || len(rows) == 0 {
		t.Fatalf("no downloads parsed from %d rows", len(rows))
	}
	t.Logf("%d rows, %d downloads", len(rows), dl)
}
