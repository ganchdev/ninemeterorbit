// ninemeterorbit reads a list of Shopify product URLs (one per line, each with
// a ?variant=<id> query param) and reports the price, availability and stock
// count of the variant each URL points to.
//
// It scrapes the rendered HTML product page, which embeds both the variants
// array (price/availability) and a custom "stock" map (exact inventory counts
// that Shopify strips from its public .js/.json APIs). Shopify redirects cold,
// sessionless product-page requests from datacenter IPs to the homepage, so we
// prime a session on the homepage first to obtain the _shopify_s/_shopify_y
// cookies. If a product's HTML can't be parsed we fall back to the .js endpoint
// for price/availability (without a stock count). Prices are in pence.
package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	htmlAccept     = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	jsonAccept     = "application/json,text/javascript,*/*;q=0.8"
)

// product mirrors the relevant fields of Shopify's /products/<handle>.js JSON.
type product struct {
	Title    string    `json:"title"`
	Variants []variant `json:"variants"`
}

// variant mirrors the relevant fields of a Shopify variant object.
type variant struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Price          int    `json:"price"`            // pence
	CompareAtPrice *int   `json:"compare_at_price"` // pence, null if not on sale
	Available      bool   `json:"available"`
	SKU            string `json:"sku"`
}

// stockEntry mirrors an entry of the theme's custom `"stock"` map, keyed by
// variant id. Shopify strips inventory_quantity from its public APIs (.js/.json),
// so exact counts are only available from the rendered HTML product page.
type stockEntry struct {
	InventoryQuantity int    `json:"inventory_quantity"`
	InventoryPolicy   string `json:"inventory_policy"`
}

func main() {
	urls, err := readURLs(urlsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", urlsFile, err)
		os.Exit(1)
	}

	// All URLs share a host; prime a session on its homepage so product pages
	// render instead of redirecting (Shopify bounces sessionless datacenter-IP
	// requests to the homepage).
	root := siteRoot(urls[0])
	client, err := newClient(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		os.Exit(1)
	}
	if err := primeSession(client, root); err != nil {
		fmt.Fprintf(os.Stderr, "warn: session prime failed: %v (stock counts may be unavailable)\n", err)
	}

	var blocks []string
	for i, u := range urls {
		wantID, err := variantID(u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", u, err)
			continue
		}

		name, v, qty, ok := scrape(client, u, wantID, root)
		if !ok {
			continue // scrape logs the reason
		}
		blocks = append(blocks, formatVariant(name, modelYear(u), u, v, qty))

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
	// scheduled cron job; harmless/skipped when unset locally).
	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		if err := notify(topic, "North kite prices", out); err != nil {
			fmt.Fprintf(os.Stderr, "notify: %v\n", err)
			os.Exit(1)
		}
	}
}

// scrape resolves one product variant, preferring the HTML page (which carries
// the stock count) and falling back to the .js endpoint (price/availability
// only) when the HTML can't be parsed. It returns the product name, variant,
// optional stock count, and ok=false (after logging) if nothing usable was found.
func scrape(c *http.Client, rawURL string, wantID int64, root string) (string, variant, *int, bool) {
	// HTML-first: one request yields price, availability and the stock count.
	if body, err := get(c, rawURL, htmlAccept, root); err == nil {
		if vs, perr := parseVariants(body); perr == nil {
			if v, found := findVariant(vs, wantID); found {
				var qty *int
				if stock, serr := parseStock(body); serr == nil {
					if e, has := stock[wantID]; has {
						q := e.InventoryQuantity
						qty = &q
					}
				} else {
					fmt.Fprintf(os.Stderr, "note: no stock count for %s: %v\n", rawURL, serr)
				}
				return productName(body), v, qty, true
			}
		}
		// HTML loaded but unusable (e.g. redirected to the homepage); fall back.
	}

	// Fallback: .js endpoint — robust price/availability, but no stock count.
	prod, err := fetchProduct(c, rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skip %s: %v\n", rawURL, err)
		return "", variant{}, nil, false
	}
	v, found := findVariant(prod.Variants, wantID)
	if !found {
		fmt.Fprintf(os.Stderr, "skip %s: variant %d not found\n", rawURL, wantID)
		return "", variant{}, nil, false
	}
	fmt.Fprintf(os.Stderr, "note: used .js fallback (no stock count) for %s\n", rawURL)
	return prod.Title, v, nil, true
}

// newClient builds an HTTP client with a cookie jar seeded for the GB market so
// prices come back in GBP regardless of the server's geo-IP. Session cookies
// from priming are stored in the same jar and reused across requests.
func newClient(root string) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if u, err := url.Parse(root); err == nil {
		jar.SetCookies(u, []*http.Cookie{
			{Name: "localization", Value: "GB"},
			{Name: "cart_currency", Value: "GBP"},
		})
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}, nil
}

// primeSession GETs the homepage to obtain Shopify's session cookies, without
// which product-page requests from datacenter IPs are redirected to the homepage.
func primeSession(c *http.Client, root string) error {
	_, err := get(c, root, htmlAccept, "")
	return err
}

// siteRoot returns the scheme://host/ root of a URL (used for priming and as
// the Referer for product-page requests).
func siteRoot(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/"
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

// productJSURL converts a product page URL into its Shopify .js endpoint,
// e.g. /products/orbit-kite-2025?variant=… -> /products/orbit-kite-2025.js
func productJSURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.RawQuery = ""
	u.Path = strings.TrimSuffix(u.Path, "/") + ".js"
	return u.String(), nil
}

// fetchProduct fetches and decodes a product's .js JSON document (the fallback
// when the HTML page can't be parsed).
func fetchProduct(c *http.Client, rawURL string) (product, error) {
	var p product
	jsURL, err := productJSURL(rawURL)
	if err != nil {
		return p, err
	}
	body, err := get(c, jsURL, jsonAccept, "")
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		// Surface a snippet so a bot-challenge HTML page (instead of JSON) is
		// obvious in the logs.
		snippet := strings.TrimSpace(body)
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return p, fmt.Errorf("decode %s: %v (got: %q)", jsURL, err, snippet)
	}
	if len(p.Variants) == 0 {
		return p, fmt.Errorf("no variants in %s", jsURL)
	}
	return p, nil
}

// get performs a GET with browser-like headers using the shared client (and its
// cookie jar). accept is the Accept header; referer is sent when non-empty.
func get(c *http.Client, rawURL, accept, referer string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("Accept", accept)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := c.Do(req)
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

// parseVariants finds the embedded `"variants":[ ... ]` array (the one with
// integer prices) in the page HTML and unmarshals it.
func parseVariants(html string) ([]variant, error) {
	const key = `"variants":[`
	for idx := strings.Index(html, key); idx != -1; idx = strings.Index(html[idx+1:], key) + idx + 1 {
		start := idx + len(key) - 1 // position of '['
		// The theme array we want has numeric ids: `[{"id":49107...`.
		// The other (storefront) array uses string ids: `[{"id":"4910...`.
		if !strings.HasPrefix(html[start:], `[{"id":`) || html[start+len(`[{"id":`)] == '"' {
			continue
		}
		raw, ok := matchDelim(html[start:], '[', ']')
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

// parseStock finds the theme's `"stock":{ ... }` map in the page HTML and
// returns it keyed by variant id. The map is the only source of exact inventory
// counts (Shopify's public APIs omit them).
func parseStock(html string) (map[int64]stockEntry, error) {
	const key = `"stock":`
	idx := strings.Index(html, key)
	if idx == -1 {
		return nil, fmt.Errorf("no stock map found in page")
	}
	brace := strings.IndexByte(html[idx:], '{')
	if brace == -1 {
		return nil, fmt.Errorf("malformed stock map")
	}
	raw, ok := matchDelim(html[idx+brace:], '{', '}')
	if !ok {
		return nil, fmt.Errorf("unbalanced stock map")
	}
	// Keys are quoted variant ids; decode as strings then convert.
	var byString map[string]stockEntry
	if err := json.Unmarshal([]byte(raw), &byString); err != nil {
		return nil, err
	}
	stock := make(map[int64]stockEntry, len(byString))
	for k, e := range byString {
		var id int64
		if _, err := fmt.Sscan(k, &id); err != nil {
			continue
		}
		stock[id] = e
	}
	return stock, nil
}

// matchDelim returns the substring from s[0] (an open delimiter) to its
// matching close delimiter, respecting nesting.
func matchDelim(s string, open, close rune) (string, bool) {
	depth := 0
	for i, r := range s {
		switch r {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
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
//	[£1109.00 | IN STOCK | 4 left]
//	Orbit Kite 2025 — 9m / Pacific Blue (was £1479.00, -25%)
//	https://...
//
// qty is the exact inventory count when known (nil if the HTML page omits it
// or could not be fetched, e.g. on the .js fallback).
func formatVariant(name, year, url string, v variant, qty *int) string {
	stock := "SOLD OUT"
	if v.Available {
		stock = "IN STOCK"
	}
	if qty != nil {
		stock += fmt.Sprintf(" | %d left", *qty)
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
