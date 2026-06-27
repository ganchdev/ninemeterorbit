// ninemeterorbit reads a list of Shopify product URLs (one per line, each with
// a ?variant=<id> query param) and reports the price and availability of the
// variant each URL points to.
//
// It fetches each product's Shopify .js endpoint (e.g. /products/<handle>.js),
// which returns a clean JSON document of every variant. This is more robust
// than scraping the HTML page, whose bot protection tends to serve a challenge
// page (HTTP 200, no product data) to datacenter IPs such as CI runners.
// Prices are in minor currency units (pence).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

var yearRe = regexp.MustCompile(`(?:19|20)\d{2}`)

const (
	urlsFile    = "urls.txt"
	politeDelay = 500 * time.Millisecond
	// A browser-like User-Agent and Accept-Language are required: Shopify's
	// bot protection soft-404s requests that omit Accept-Language.
	userAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	acceptLanguage = "en-GB,en;q=0.9"
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

		prod, err := fetchProduct(u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", u, err)
			continue
		}

		v, ok := findVariant(prod.Variants, wantID)
		if !ok {
			fmt.Fprintf(os.Stderr, "skip %s: variant %d not found\n", u, wantID)
			continue
		}
		blocks = append(blocks, formatVariant(prod.Title, modelYear(u), u, v))

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

// fetchProduct fetches and decodes a product's .js JSON document.
func fetchProduct(rawURL string) (product, error) {
	var p product
	jsURL, err := productJSURL(rawURL)
	if err != nil {
		return p, err
	}
	body, err := fetchPage(jsURL)
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

// fetchPage GETs a URL with browser-like headers and returns the body.
func fetchPage(rawURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", acceptLanguage)
	req.Header.Set("Accept", "application/json,text/javascript,*/*;q=0.8")

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
