package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// sampleJS is a trimmed Shopify /products/<handle>.js document.
const sampleJS = `{"title":"Orbit Kite","handle":"orbit-kite-2025","variants":[
{"id":49107405472085,"title":"9m / Pacific Blue","option1":"9m","price":110900,"compare_at_price":147900,"available":true},
{"id":1,"title":"6m / Pacific Blue","option1":"6m","price":99900,"compare_at_price":null,"available":false}]}`

func TestProductJSURL(t *testing.T) {
	got, err := productJSURL("https://northactionsports.com/products/orbit-kite-2025?variant=49107405472085")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://northactionsports.com/products/orbit-kite-2025.js"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestModelYear(t *testing.T) {
	if y := modelYear("https://x/products/orbit-kite-2025?variant=1"); y != "2025" {
		t.Errorf("year = %q, want 2025", y)
	}
	if y := modelYear("https://x/products/orbit-ultra-kite"); y != "" {
		t.Errorf("year = %q, want empty", y)
	}
}

func TestParseAndFormat(t *testing.T) {
	var p product
	if err := json.Unmarshal([]byte(sampleJS), &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "Orbit Kite" {
		t.Errorf("title = %q", p.Title)
	}

	v, ok := findVariant(p.Variants, 49107405472085)
	if !ok {
		t.Fatal("target variant not found")
	}
	if v.Price != 110900 || !v.Available {
		t.Errorf("unexpected variant: %+v", v)
	}

	line := formatVariant(p.Title, "2025", "https://example/p", v)
	for _, want := range []string{
		"[£1109.00 | IN STOCK]",
		"Orbit Kite 2025 — 9m / Pacific Blue",
		"-25%",
		"https://example/p",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("output missing %q in:\n%s", want, line)
		}
	}
}
