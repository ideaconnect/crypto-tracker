package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ────────────────────────────────────────────────────────────────────────────
// Configuration
// ────────────────────────────────────────────────────────────────────────────

const binanceBaseURL = "https://api.binance.com"

// pairs to track – Binance symbol format (no slash)
var trackedSymbols = []string{"BTCUSDT", "ETHUSDT", "TRXUSDT", "BNBUSDT", "ADAUSDT", "XRPUSDT", "SOLUSDT", "DOGEUSDT"}

// human-readable display names for the output JSON
var symbolNames = map[string]string{
	"BTCUSDT": "BTC/USDT",
	"ETHUSDT": "ETH/USDT",
	"TRXUSDT": "TRX/USDT",
	"BNBUSDT": "BNB/USDT",
	"ADAUSDT": "ADA/USDT",
	"XRPUSDT": "XRP/USDT",
	"SOLUSDT": "SOL/USDT",
	"DOGEUSDT": "DOGE/USDT",
}

// timeframes supported by the Binance rolling-window ticker
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/public-api-endpoints#rolling-window-price-change-statistics
var timeframes = []struct {
	windowSize  string // value sent to Binance API
	label       string // key used in the output JSON
}{
	{"1m", "1min"},
	{"5m", "5min"},
	{"1h", "1h"},
	{"12h", "12h"},
	{"1d", "24h"},
	{"7d", "7d"},
}

// ────────────────────────────────────────────────────────────────────────────
// Binance API response types
// ────────────────────────────────────────────────────────────────────────────

// binanceTicker mirrors the fields returned by GET /api/v3/ticker
type binanceTicker struct {
	Symbol             string `json:"symbol"`
	PriceChange        string `json:"priceChange"`
	PriceChangePercent string `json:"priceChangePercent"`
	WeightedAvgPrice   string `json:"weightedAvgPrice"`
	OpenPrice          string `json:"openPrice"`
	HighPrice          string `json:"highPrice"`
	LowPrice           string `json:"lowPrice"`
	LastPrice          string `json:"lastPrice"`
	Volume             string `json:"volume"`
	QuoteVolume        string `json:"quoteVolume"`
	OpenTime           int64  `json:"openTime"`
	CloseTime          int64  `json:"closeTime"`
	Count              int64  `json:"count"`
}

// ────────────────────────────────────────────────────────────────────────────
// Output types
// ────────────────────────────────────────────────────────────────────────────

// ChangeInfo holds the price difference and percentage change over a window.
type ChangeInfo struct {
	// Absolute price change (positive = up, negative = down)
	PriceChange string `json:"priceChange"`
	// Percentage change, e.g. "1.23" means +1.23 %
	PriceChangePercent string `json:"priceChangePercent"`
}

// PairData contains the current price and per-window changes for one symbol.
type PairData struct {
	Symbol        string                `json:"symbol"`
	Name          string                `json:"name"`
	CurrentPrice  string                `json:"currentPrice"`
	// PreviousPrice is the currentPrice read from the file that existed before
	// this run (empty string when no prior file is present).
	PreviousPrice string                `json:"previousPrice,omitempty"`
	OpenPrices    map[string]string     `json:"openPrices"`
	Changes       map[string]ChangeInfo `json:"changes"`
}

// Summary is the top-level document written to the JSON output file.
type Summary struct {
	// RFC-3339 UTC timestamp of when prices were fetched
	LastUpdate string                `json:"lastUpdate"`
	Pairs      map[string]*PairData  `json:"pairs"`
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────────

// loadPreviousPrices reads the JSON file at path (if it exists) and returns a
// map of symbol → currentPrice from the previous run. Missing or unreadable
// files are silently ignored (returns an empty map).
func loadPreviousPrices(path string) map[string]string {
	result := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return result // file absent on first run
	}
	var prev Summary
	if err := json.Unmarshal(data, &prev); err != nil {
		return result
	}
	for sym, pair := range prev.Pairs {
		if pair.CurrentPrice != "" {
			result[sym] = pair.CurrentPrice
		}
	}
	return result
}

// ────────────────────────────────────────────────────────────────────────────
// Binance client
// ────────────────────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

const maxRetries = 3

// isTransient returns true for errors worth retrying (timeouts, temporary
// network errors, and HTTP 5xx responses).
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	// context.DeadlineExceeded, net timeouts, temporary net errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Also catch the "context deadline exceeded" text that sometimes wraps
	if strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout") {
		return true
	}
	return false
}

// retryGet performs an HTTP GET with exponential backoff on transient errors.
func retryGet(url string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second // 1s, 2s, 4s
			log.Printf("WARN: retry %d/%d after %s for %s", attempt, maxRetries, backoff, url)
			time.Sleep(backoff)
		}

		resp, err := httpClient.Get(url)
		if err != nil {
			if isTransient(err) {
				lastErr = err
				continue
			}
			return nil, err // non-transient: give up immediately
		}

		// Retry on 5xx server errors
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		return resp, nil
	}
	return nil, fmt.Errorf("all %d retries exhausted: %w", maxRetries, lastErr)
}

// fetchTicker calls GET /api/v3/ticker for the given symbols and windowSize.
// Binance accepts multiple symbols as a JSON array in a single request.
func fetchTicker(symbols []string, windowSize string) ([]binanceTicker, error) {
	symbolsJSON, err := json.Marshal(symbols)
	if err != nil {
		return nil, fmt.Errorf("marshal symbols: %w", err)
	}

	params := url.Values{}
	params.Set("symbols", string(symbolsJSON))
	params.Set("windowSize", windowSize)
	// "MINI" type omits volume fields but keeps all price/change fields
	params.Set("type", "FULL")

	reqURL := fmt.Sprintf("%s/api/v3/ticker?%s", binanceBaseURL, params.Encode())

	resp, err := retryGet(reqURL)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d for windowSize=%s", resp.StatusCode, windowSize)
	}

	var tickers []binanceTicker
	if err := json.NewDecoder(resp.Body).Decode(&tickers); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return tickers, nil
}

// ────────────────────────────────────────────────────────────────────────────
// Core update logic
// ────────────────────────────────────────────────────────────────────────────

// runOnce fetches all timeframes from Binance and atomically overwrites
// outputPath. If any network or API error occurs the file is left untouched
// and the error is returned.
func runOnce(outputPath string) error {
	// Read prices from the file that currently exists (may not exist yet).
	prevPrices := loadPreviousPrices(outputPath)

	summary := &Summary{
		LastUpdate: time.Now().UTC().Format(time.RFC3339),
		Pairs:      make(map[string]*PairData, len(trackedSymbols)),
	}

	for _, sym := range trackedSymbols {
		summary.Pairs[sym] = &PairData{
			Symbol:        sym,
			Name:          symbolNames[sym],
			PreviousPrice: prevPrices[sym],
			OpenPrices:    make(map[string]string),
			Changes:       make(map[string]ChangeInfo),
		}
	}

	// Fetch all timeframes concurrently to reduce wall-clock time.
	type tfResult struct {
		label   string
		tickers []binanceTicker
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		results  []tfResult
	)

	for _, tf := range timeframes {
		wg.Add(1)
		go func(windowSize, label string) {
			defer wg.Done()
			tickers, err := fetchTicker(trackedSymbols, windowSize)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("fetch %s window: %w", label, err)
				}
				return
			}
			results = append(results, tfResult{label: label, tickers: tickers})
		}(tf.windowSize, tf.label)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}

	for _, res := range results {
		for _, t := range res.tickers {
			pair, ok := summary.Pairs[t.Symbol]
			if !ok {
				continue
			}
			pair.CurrentPrice = t.LastPrice
			pair.OpenPrices[res.label] = t.OpenPrice
			pair.Changes[res.label] = ChangeInfo{
				PriceChange:        t.PriceChange,
				PriceChangePercent: t.PriceChangePercent,
			}
		}
	}

	// Serialise to a temp file first, then rename to make the update atomic.
	tmp := outputPath + ".tmp"
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, outputPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename to output: %w", err)
	}

	return nil
}

// ────────────────────────────────────────────────────────────────────────────
// HTTP file server
// ────────────────────────────────────────────────────────────────────────────

// startFileServer starts an HTTP server on addr that exposes two endpoints:
//
//	GET /           – same as /prices.json
//	GET /prices.json – returns the current JSON file with proper headers
//
// The server runs in a goroutine; fatal errors are logged and the process exits.
func startFileServer(addr, outputPath string, interval time.Duration) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		data, err := os.ReadFile(outputPath)
		if err != nil {
			// File not yet written (first fetch still in progress).
			http.Error(w, "prices not available yet", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(interval.Seconds())))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	mux.HandleFunc("/prices.json", handler)

	log.Printf("HTTP server listening on %s", addr)

	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()
}

// ────────────────────────────────────────────────────────────────────────────
// Main
// ────────────────────────────────────────────────────────────────────────────

func main() {
	outputPath := flag.String("output", "prices.json", "path to the JSON output file")
	interval := flag.Duration("interval", 10*time.Second, "how often to refresh prices")
	addr := flag.String("addr", ":8080", "address for the HTTP server (empty string disables it)")
	flag.Parse()

	// Graceful shutdown on SIGINT / SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("Starting crypto-tracker (interval=%s, output=%s)", *interval, *outputPath)

	if *addr != "" {
		startFileServer(*addr, *outputPath, *interval)
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	// Run immediately on startup, then on every tick.
	runAndLog := func() {
		if err := runOnce(*outputPath); err != nil {
			log.Printf("ERROR (file unchanged): %v", err)
			return
		}
		log.Printf("Updated %s", *outputPath)
	}

	runAndLog()

	for {
		select {
		case <-ticker.C:
			runAndLog()
		case sig := <-quit:
			log.Printf("Received %s, shutting down.", sig)
			return
		}
	}
}
