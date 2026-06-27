package main

import (
	"os"
	"testing"
)

// TestParseVariantsExample checks parsing against the saved example.html.
func TestParseVariantsExample(t *testing.T) {
	html, err := os.ReadFile("example.html")
	if err != nil {
		t.Skipf("no example.html: %v", err)
	}
	vs, err := parseVariants(string(html))
	if err != nil {
		t.Fatalf("parseVariants: %v", err)
	}

	var nine int
	for _, v := range vs {
		if v.ID == 49107405472085 { // 9m / Pacific Blue
			nine++
			if v.Price != 110900 {
				t.Errorf("price = %d, want 110900", v.Price)
			}
			if !v.Available {
				t.Errorf("expected available")
			}
		}
	}
	if nine != 1 {
		t.Fatalf("found target variant %d times, want 1", nine)
	}
	t.Logf("parsed %d variants from example.html", len(vs))
}
