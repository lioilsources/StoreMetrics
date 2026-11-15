package main

import (
	"os"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// apps.yaml ids are telemetry app ids too, so they must pass the ingest's app pattern and be unique.
func TestAppsYAML(t *testing.T) {
	b, err := os.ReadFile("../../apps.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	idRe := regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	ids, skus, pkgs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, a := range cfg.Apps {
		if !idRe.MatchString(a.ID) {
			t.Errorf("id %q does not match %s", a.ID, idRe)
		}
		if a.Name == "" {
			t.Errorf("%s: empty name", a.ID)
		}
		if ids[a.ID] {
			t.Errorf("duplicate id %q", a.ID)
		}
		ids[a.ID] = true
		for _, s := range a.AppleSKUs {
			if skus[s] {
				t.Errorf("%s: SKU %q used twice", a.ID, s)
			}
			skus[s] = true
		}
		if a.PlayPackage != "" {
			if pkgs[a.PlayPackage] {
				t.Errorf("%s: package %q used twice", a.ID, a.PlayPackage)
			}
			pkgs[a.PlayPackage] = true
		}
	}
	for _, id := range []string{"kirian", "mirrorbooth", "puff", "lexify", "poesie", "pohadkomat", "webtoon",
		"kaceny", "hajanek", "papanek", "behanek", "immunorun", "handwrittenstickers", "alienmatch",
		"restyler", "tlumocnik", "djfy"} {
		if !ids[id] {
			t.Errorf("telemetry app %q missing from apps.yaml", id)
		}
	}
}
