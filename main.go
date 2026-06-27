// northscraper reads a list of Shopify product URLs (one per line, each with a
// ?variant=<id> query param) and reports the price and availability of the
// variant each URL points to.
//
// It relies on the fact that Shopify product pages embed a JSON array of every
// variant in the page source, so we extract that JSON directly instead of
// parsing the HTML DOM. Prices are in minor currency units (pence).
package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

var (
	titleRe = regexp.MustCompile(`(?is)<title>(.*?)</title>`)
	yearRe  = regexp.MustCompile(`(?:19|20)\d{2}`)
)

const (
	urlsFile    = "urls.txt"
	politeDelay = 500 * time.Millisecond
	// A browser-like User-Agent and Accept-Language are required: Shopify's
	// bot protection soft-404s requests that omit Accept-Language.
	userAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	acceptLanguage = "en-GB,en;q=0.9"
)

// variant mirrors the relevant fields of a Shopify variant object.
type variant struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Price          int    `json:"price"`           // pence
	CompareAtPrice *int   `json:"compare_at_price"` // pence, null if not on sale
	Available      bool   `json:"available"`
	SKU            string `json:"sku"`
}

func main() {
	urls, err := readURLs(urlsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", urlsFile, err)
		os.Exit(1)
	}

	var blocks []string
	for i, u := range urls {
		wantID, err := variantID(u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", u, err)
			continue
		}

		body, err := fetchPage(u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", u, err)
			continue
		}

		variants, err := parseVariants(body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", u, err)
			continue
		}

		v, ok := findVariant(variants, wantID)
		if !ok {
			fmt.Fprintf(os.Stderr, "skip %s: variant %d not found on page\n", u, wantID)
			continue
		}
		blocks = append(blocks, formatVariant(productName(body), modelYear(u), u, v))

		if i < len(urls)-1 {
			time.Sleep(politeDelay)
		}
	}

	if len(blocks) == 0 {
		fmt.Fprintln(os.Stderr, "no variants reported")
		os.Exit(1)
	}
	out := strings.Join(blocks, "\n\n")
	fmt.Println(out)

	// Push the report to ntfy.sh when a topic is configured (used by the
	// scheduled GitHub Action; harmless/skipped when unset locally).
	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		if err := notify(topic, "North kite prices", out); err != nil {
			fmt.Fprintf(os.Stderr, "notify: %v\n", err)
			os.Exit(1)
		}
	}
}

// notify posts a message to an ntfy topic. NTFY_SERVER overrides the default
// public server (https://ntfy.sh).
func notify(topic, title, body string) error {
	server := os.Getenv("NTFY_SERVER")
	if server == "" {
		server = "https://ntfy.sh"
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+"/"+topic, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "shopping_cart")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ntfy status %s", resp.Status)
	}
	return nil
}

// readURLs returns the non-empty, non-comment lines of a file.
func readURLs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, nil
}

// variantID extracts the ?variant= id from a product URL.
func variantID(rawURL string) (int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	v := u.Query().Get("variant")
	if v == "" {
		return 0, fmt.Errorf("no ?variant= in URL")
	}
	var id int64
	if _, err := fmt.Sscan(v, &id); err != nil {
		return 0, fmt.Errorf("bad variant id %q", v)
	}
	return id, nil
}

// fetchPage downloads a product page and returns its HTML body.
func fetchPage(rawURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// productName extracts the product name from the page <title>, dropping any
// trailing " – Store Name" style suffix.
func productName(body string) string {
	m := titleRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	name := html.UnescapeString(strings.TrimSpace(m[1]))
	for _, sep := range []string{" – ", " — ", " | "} {
		if i := strings.Index(name, sep); i != -1 {
			name = name[:i]
		}
	}
	return strings.TrimSpace(name)
}

// modelYear pulls a 4-digit year from the product handle in the URL
// (e.g. .../products/orbit-kite-2025 -> "2025"); empty if none.
func modelYear(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return yearRe.FindString(path.Base(u.Path))
}

// parseVariants finds the embedded `"variants":[ ... ]` array (the one with
// integer prices) and unmarshals it.
func parseVariants(html string) ([]variant, error) {
	const key = `"variants":[`
	for idx := strings.Index(html, key); idx != -1; idx = strings.Index(html[idx+1:], key) + idx + 1 {
		start := idx + len(key) - 1 // position of '['
		// The theme array we want has numeric ids: `[{"id":49107...`.
		// The other (storefront) array uses string ids: `[{"id":"4910...`.
		if !strings.HasPrefix(html[start:], `[{"id":`) || html[start+len(`[{"id":`)] == '"' {
			continue
		}
		raw, ok := matchBrackets(html[start:])
		if !ok {
			continue
		}
		// Skip the slim analytics array that omits availability/options; we
		// want the full theme array with the fields we report on.
		if !strings.Contains(raw, `"available":`) {
			continue
		}
		var vs []variant
		if err := json.Unmarshal([]byte(raw), &vs); err != nil {
			continue // not the array we want; keep looking
		}
		if len(vs) > 0 {
			return vs, nil
		}
	}
	return nil, fmt.Errorf("no variants array found in page")
}

// matchBrackets returns the substring from s[0] (a '[') to its matching ']'.
func matchBrackets(s string) (string, bool) {
	depth := 0
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}

func findVariant(vs []variant, id int64) (variant, bool) {
	for _, v := range vs {
		if v.ID == id {
			return v, true
		}
	}
	return variant{}, false
}

// formatVariant renders a single report block: a price/stock headline, the
// product description, then the URL, e.g.
//
//	[£1109.00 | IN STOCK]
//	Orbit Kite 2025 — 9m / Pacific Blue (was £1479.00, -25%)
//	https://...
func formatVariant(name, year, url string, v variant) string {
	stock := "SOLD OUT"
	if v.Available {
		stock = "IN STOCK"
	}
	head := fmt.Sprintf("[£%.2f | %s]", float64(v.Price)/100, stock)

	label := strings.TrimSpace(name + " " + year)
	if label != "" {
		label += " — "
	}
	desc := label + v.Title
	if v.CompareAtPrice != nil && *v.CompareAtPrice > v.Price {
		off := float64(*v.CompareAtPrice-v.Price) / float64(*v.CompareAtPrice) * 100
		desc += fmt.Sprintf(" (was £%.2f, -%.0f%%)", float64(*v.CompareAtPrice)/100, off)
	}
	return head + "\n" + desc + "\n" + url
}
