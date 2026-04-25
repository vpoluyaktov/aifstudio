// Package ifdb — client_test.go tests the IFDB HTTP client (client.go).
//
// Tests use httptest.NewServer to simulate IFDB responses without network calls.
// Mock JSON shapes match the actual IFDB API responses (verified 2026-04).
package ifdb_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"aifstudio/internal/ifdb"
)

const testUserAgent = "StoryCloud/0.1 (contact: vpoluyaktov@gmail.com)"

// newTestClient creates a Client pointed at baseURL with permissive rate limits.
func newTestClient(t *testing.T, baseURL string) *ifdb.Client {
	t.Helper()
	return ifdb.NewClient(ifdb.ClientOptions{
		BaseURL:     baseURL,
		UserAgent:   testUserAgent,
		CacheTTL:    10 * time.Minute,
		GlobalQPS:   100,
		GlobalBurst: 1000,
		PerIPQPS:    100,
		PerIPBurst:  1000,
	})
}

// sampleSearchJSON returns a minimal IFDB search response using the real API shape.
// IFDB returns {"games":[{"tuid":...,"published":{"machine":"1980",...},...}]}.
func sampleSearchJSON(count int) []byte {
	type published struct {
		Machine   string `json:"machine"`
		Printable string `json:"printable"`
	}
	type entry struct {
		TUID          string    `json:"tuid"`
		Title         string    `json:"title"`
		Author        string    `json:"author"`
		Published     published `json:"published"`
		AverageRating float64   `json:"averageRating"`
		HasCoverArt   bool      `json:"hasCoverArt"`
		CoverArtLink  string    `json:"coverArtLink"`
	}
	type response struct {
		Games []entry `json:"games"`
	}
	var entries []entry
	for i := 0; i < count; i++ {
		entries = append(entries, entry{
			TUID:          fmt.Sprintf("tuid%010d", i),
			Title:         fmt.Sprintf("Game %d", i),
			Author:        "Author One",
			Published:     published{Machine: "1980", Printable: "1980"},
			AverageRating: 4.2,
			HasCoverArt:   true,
			CoverArtLink:  fmt.Sprintf("https://ifdb.org/coverart?id=tuid%010d&version=1", i),
		})
	}
	b, _ := json.Marshal(response{Games: entries})
	return b
}

// sampleGameJSON returns a minimal IFDB viewgame response using the real API shape.
// IFDB returns {"identification":{...},"bibliographic":{...},"ifdb":{...}}.
func sampleGameJSON(tuid, title string) []byte {
	type coverArt struct {
		URL string `json:"url"`
	}
	type link struct {
		URL      string `json:"url"`
		Format   string `json:"format"`
		Size     int64  `json:"size"`
		FileType string `json:"fileType"`
		IsGame   bool   `json:"isGame"`
	}
	type downloads struct {
		Links []link `json:"links"`
	}
	type ifdbSection struct {
		TUID      string    `json:"tuid"`
		CoverArt  *coverArt `json:"coverart"`
		Downloads downloads `json:"downloads"`
	}
	type bibliographic struct {
		Title          string `json:"title"`
		Author         string `json:"author"`
		FirstPublished string `json:"firstpublished"`
		Description    string `json:"description"`
	}
	type identification struct {
		Format string `json:"format"`
	}
	type resp struct {
		Identification identification `json:"identification"`
		Bibliographic  bibliographic  `json:"bibliographic"`
		IFDB           ifdbSection    `json:"ifdb"`
	}
	b, _ := json.Marshal(resp{
		Identification: identification{Format: "zcode"},
		Bibliographic: bibliographic{
			Title:          title,
			Author:         "Marc Blank and Dave Lebling",
			FirstPublished: "1980",
			Description:    "<p>Classic adventure.</p>",
		},
		IFDB: ifdbSection{
			TUID:     tuid,
			CoverArt: &coverArt{URL: "https://ifdb.org/coverart?id=" + tuid + "&version=1"},
			Downloads: downloads{
				Links: []link{
					{URL: "https://mirror.ifarchive.org/zork1.z5", Format: "zcode", Size: 84992, IsGame: true},
				},
			},
		},
	})
	return b
}

// ─────────────────────────────────────────────────────────────────────────────
// User-Agent header — ARCHITECTURE.md §7
// ─────────────────────────────────────────────────────────────────────────────

func TestClientAlwaysSendsUserAgent(t *testing.T) {
	var receivedUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.Write(sampleSearchJSON(1)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, _ = c.Search(context.Background(), "", "zork", 25)

	if receivedUA != testUserAgent {
		t.Errorf("User-Agent = %q; want %q", receivedUA, testUserAgent)
	}
}

func TestClientUserAgentSentOnGetGame(t *testing.T) {
	var receivedUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.Write(sampleGameJSON("0dbnusxunq7fw5ro", "Zork I")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, _ = c.GetGame(context.Background(), "", "0dbnusxunq7fw5ro")

	if receivedUA != testUserAgent {
		t.Errorf("User-Agent = %q; want %q", receivedUA, testUserAgent)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Retry logic — ARCHITECTURE.md §9
// ─────────────────────────────────────────────────────────────────────────────

func TestClientRetriesOn503ThenSucceeds(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(sampleSearchJSON(1)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	result, _, err := c.Search(context.Background(), "", "zork", 25)
	if err != nil {
		t.Fatalf("Search() after retry error = %v; want success", err)
	}
	if result == nil {
		t.Fatal("result = nil; want non-nil after retry")
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Errorf("callCount = %d; want ≥ 2 (at least one retry)", callCount)
	}
}

func TestClientMaxRetriesAllFail(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.Search(context.Background(), "", "zork", 25)
	if err == nil {
		t.Error("Search() with all-503 responses should return error")
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Errorf("callCount = %d; want ≥ 2 (retries attempted)", callCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// In-memory cache — ARCHITECTURE.md §9.1
// ─────────────────────────────────────────────────────────────────────────────

func TestSearchCacheHitSecondCallSkipsHTTP(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(sampleSearchJSON(2)) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	r1, hit1, err := c.Search(ctx, "", "zork", 25)
	if err != nil {
		t.Fatalf("first Search() error = %v", err)
	}
	if hit1 {
		t.Error("first call should not be a cache hit")
	}

	r2, hit2, err := c.Search(ctx, "", "zork", 25)
	if err != nil {
		t.Fatalf("second Search() error = %v", err)
	}
	if !hit2 {
		t.Error("second call with same query should be a cache hit")
	}
	if r1.Count != r2.Count {
		t.Errorf("cached count = %d; want %d", r2.Count, r1.Count)
	}
	if n := atomic.LoadInt32(&callCount); n != 1 {
		t.Errorf("HTTP server called %d times; want 1 (cache should serve second request)", n)
	}
}

func TestGetGameCacheHitSecondCallSkipsHTTP(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(sampleGameJSON("0dbnusxunq7fw5ro", "Zork I")) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	_, hit1, err := c.GetGame(ctx, "", "0dbnusxunq7fw5ro")
	if err != nil {
		t.Fatalf("first GetGame() error = %v", err)
	}
	if hit1 {
		t.Error("first call should not be a cache hit")
	}

	_, hit2, _ := c.GetGame(ctx, "", "0dbnusxunq7fw5ro")
	if !hit2 {
		t.Error("second call should be a cache hit")
	}
	if n := atomic.LoadInt32(&callCount); n != 1 {
		t.Errorf("HTTP server called %d times; want 1", n)
	}
}

func TestSearchCacheTTLExpiry(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(sampleSearchJSON(1)) //nolint:errcheck
	}))
	defer srv.Close()

	c := ifdb.NewClient(ifdb.ClientOptions{
		BaseURL:     srv.URL,
		UserAgent:   testUserAgent,
		CacheTTL:    50 * time.Millisecond,
		GlobalQPS:   100,
		GlobalBurst: 1000,
		PerIPQPS:    100,
		PerIPBurst:  1000,
	})

	ctx := context.Background()
	_, _, _ = c.Search(ctx, "", "zork", 25)

	time.Sleep(100 * time.Millisecond)

	_, hit, _ := c.Search(ctx, "", "zork", 25)
	if hit {
		t.Error("second call after TTL expiry should be a cache miss")
	}
	if n := atomic.LoadInt32(&callCount); n < 2 {
		t.Errorf("HTTP server called %d times; want ≥ 2 (TTL expired)", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Response normalization — ARCHITECTURE.md §3.2 / §3.3
// ─────────────────────────────────────────────────────────────────────────────

func TestSearchResultNormalization(t *testing.T) {
	// Real IFDB search JSON shape: top-level "games" array, "tuid", "published.machine", float rating.
	raw := `{
		"games": [{
			"tuid": "0dbnusxunq7fw5ro",
			"title": "Zork I",
			"author": "Marc Blank and Dave Lebling",
			"published": {"machine": "1980", "printable": "1980"},
			"averageRating": 4.4,
			"hasCoverArt": true,
			"coverArtLink": "https://ifdb.org/coverart?id=0dbnusxunq7fw5ro&version=44"
		}]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, raw)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, _, err := c.Search(context.Background(), "", "zork", 25)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if resp.Query != "zork" {
		t.Errorf("Query = %q; want zork", resp.Query)
	}
	if resp.Count != 1 {
		t.Errorf("Count = %d; want 1", resp.Count)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("len(Results) = %d; want 1", len(resp.Results))
	}

	r := resp.Results[0]
	if r.ID != "0dbnusxunq7fw5ro" {
		t.Errorf("ID = %q; want 0dbnusxunq7fw5ro", r.ID)
	}
	if r.Title != "Zork I" {
		t.Errorf("Title = %q; want Zork I", r.Title)
	}
	if len(r.Authors) != 2 {
		t.Errorf("Authors len = %d; want 2 (split on 'and')", len(r.Authors))
	}
	if r.Year != 1980 {
		t.Errorf("Year = %d; want 1980", r.Year)
	}
	if r.Rating != 4.4 {
		t.Errorf("Rating = %v; want 4.4", r.Rating)
	}
	if r.CoverArtURL == "" {
		t.Error("CoverArtURL is empty")
	}
}

func TestGetGameNormalization(t *testing.T) {
	// Real IFDB viewgame JSON shape: nested identification/bibliographic/ifdb sections.
	raw := `{
		"identification": {"ifids":["ZCODE-52-871125"],"format":"zcode"},
		"bibliographic": {
			"title": "Zork I",
			"author": "Marc Blank and Dave Lebling",
			"firstpublished": "1980",
			"description": "<p>Classic treasure hunt <em>through</em> the Underground Empire.</p>"
		},
		"ifdb": {
			"tuid": "0dbnusxunq7fw5ro",
			"coverart": {"url": "https://ifdb.org/coverart?id=0dbnusxunq7fw5ro&version=44"},
			"downloads": {
				"links": [
					{"url":"https://mirror.ifarchive.org/zork1.z5","format":"zcode","size":84992,"isGame":true},
					{"url":"https://mirror.ifarchive.org/zork1.zblorb","format":"zblorb","size":126976,"isGame":true}
				]
			}
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, raw)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "0dbnusxunq7fw5ro")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if game == nil {
		t.Fatal("game = nil")
	}
	if game.ID != "0dbnusxunq7fw5ro" {
		t.Errorf("ID = %q", game.ID)
	}
	if game.Title != "Zork I" {
		t.Errorf("Title = %q", game.Title)
	}
	if len(game.Authors) != 2 {
		t.Errorf("Authors len = %d; want 2", len(game.Authors))
	}
	if game.Year != 1980 {
		t.Errorf("Year = %d; want 1980", game.Year)
	}
	if game.Description == "" {
		t.Error("Description is empty")
	}
	for _, tag := range []string{"<p>", "</p>", "<em>", "</em>"} {
		for i := 0; i <= len(game.Description)-len(tag); i++ {
			if game.Description[i:i+len(tag)] == tag {
				t.Errorf("Description contains HTML tag %q; want plain text", tag)
				break
			}
		}
	}
	if len(game.DownloadLinks) != 2 {
		t.Errorf("DownloadLinks len = %d; want 2", len(game.DownloadLinks))
	}
	if len(game.Formats) < 1 {
		t.Error("Formats is empty")
	}
	if game.CoverArtURL == "" {
		t.Error("CoverArtURL is empty")
	}
}

func TestGetGameMissingYearAndRating(t *testing.T) {
	raw := `{
		"identification": {"format":"zcode"},
		"bibliographic": {"title":"No Dates","author":"","firstpublished":"","description":""},
		"ifdb": {"tuid":"abc1234567","downloads":{"links":[]}}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, raw)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "abc1234567")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if game.Year != 0 {
		t.Errorf("Year = %d; want 0", game.Year)
	}
	if game.Rating != 0 {
		t.Errorf("Rating = %v; want 0", game.Rating)
	}
}

func TestGetGameNoDownloadLinks(t *testing.T) {
	raw := `{
		"identification": {"format":"zcode"},
		"bibliographic": {"title":"No Links","author":"","firstpublished":"","description":""},
		"ifdb": {"tuid":"abc1234567","downloads":{"links":[]}}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, raw)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "abc1234567")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if game.DownloadLinks == nil {
		t.Error("DownloadLinks = nil; want empty slice")
	}
	if len(game.DownloadLinks) != 0 {
		t.Errorf("DownloadLinks len = %d; want 0", len(game.DownloadLinks))
	}
	if game.Formats == nil {
		t.Error("Formats = nil; want empty slice")
	}
}

func TestSearchEmptyResultsNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"games":[]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, _, err := c.Search(context.Background(), "", "xyzzy-no-match", 25)
	if err != nil {
		t.Fatalf("Search() with empty results error = %v; want nil", err)
	}
	if resp.Count != 0 {
		t.Errorf("Count = %d; want 0", resp.Count)
	}
	if resp.Results == nil {
		t.Error("Results = nil; want empty slice []")
	}
}

func TestSearchMalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{this is not json`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.Search(context.Background(), "", "zork", 25)
	if err == nil {
		t.Error("Search() with malformed JSON should return an error")
	}
}

// TestRateLimitingBurstsAreThrottled verifies the global token bucket throttles excess requests.
func TestRateLimitingBurstsAreThrottled(t *testing.T) {
	c := ifdb.NewClient(ifdb.ClientOptions{
		BaseURL:     "http://localhost:0",
		UserAgent:   testUserAgent,
		CacheTTL:    10 * time.Minute,
		GlobalQPS:   1.0,
		GlobalBurst: 1,
		PerIPQPS:    100,
		PerIPBurst:  1000,
	})

	ctx := context.Background()
	var rateLimitErr error
	for i := 0; i < 20; i++ {
		_, _, err := c.Search(ctx, "", "zork", 25)
		if err != nil {
			rateLimitErr = err
			break
		}
	}
	if rateLimitErr == nil {
		t.Error("expected a rate-limit error after exhausting burst budget")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// UnsupportedFormats — ARCHITECTURE.md §A.7.1 / §3.4 (unsupported_format error)
//
// viewgameToGame populates Game.UnsupportedFormats for any download link whose
// format/fileType/extension is not in knownFormats (z3,z4,z5,z6,z7,z8,zblorb,ulx,gblorb).
// ─────────────────────────────────────────────────────────────────────────────

// gameJSONWithFormats builds a minimal IFDB viewgame response containing the
// specified download link entries.
func gameJSONWithFormats(tuid string, links []map[string]interface{}) []byte {
	type resp struct {
		Identification struct {
			Format string `json:"format"`
		} `json:"identification"`
		Bibliographic struct {
			Title          string `json:"title"`
			Author         string `json:"author"`
			FirstPublished string `json:"firstpublished"`
			Description    string `json:"description"`
		} `json:"bibliographic"`
		IFDB struct {
			TUID      string `json:"tuid"`
			Downloads struct {
				Links []map[string]interface{} `json:"links"`
			} `json:"downloads"`
		} `json:"ifdb"`
	}

	var r resp
	r.Identification.Format = "unknown"
	r.Bibliographic.Title = "Test Game"
	r.Bibliographic.Author = "Tester"
	r.IFDB.TUID = tuid
	r.IFDB.Downloads.Links = links

	b, _ := json.Marshal(r)
	return b
}

func TestUnsupportedFormatsPopulated(t *testing.T) {
	// A game with only ALAN-format links (not in knownFormats) → UnsupportedFormats
	// populated, DownloadLinks empty.
	links := []map[string]interface{}{
		{"url": "https://example.com/game.alan", "format": "alan", "fileType": "alan", "size": 1000, "isGame": true},
	}
	raw := gameJSONWithFormats("alangame1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "alangame1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}

	if len(game.DownloadLinks) != 0 {
		t.Errorf("DownloadLinks len = %d; want 0 (unsupported format)", len(game.DownloadLinks))
	}
	if len(game.UnsupportedFormats) == 0 {
		t.Error("UnsupportedFormats is empty; want at least one entry for 'alan'")
	}
	found := false
	for _, f := range game.UnsupportedFormats {
		if f == "alan" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("UnsupportedFormats = %v; want to contain 'alan'", game.UnsupportedFormats)
	}
}

func TestUnsupportedFormatsEmptyForKnownFormats(t *testing.T) {
	// A game with only z5 links (known format) → UnsupportedFormats empty.
	links := []map[string]interface{}{
		{"url": "https://mirror.ifarchive.org/zork1.z5", "format": "zcode", "fileType": "zcode", "size": 84992, "isGame": true},
	}
	raw := gameJSONWithFormats("zorkgame1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "zorkgame1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}

	if len(game.UnsupportedFormats) != 0 {
		t.Errorf("UnsupportedFormats = %v; want empty for known format z5", game.UnsupportedFormats)
	}
	if len(game.DownloadLinks) == 0 {
		t.Error("DownloadLinks is empty; want z5 link parsed from URL extension")
	}
}

func TestUnsupportedFormatsDeduplication(t *testing.T) {
	// Two links with the same unsupported format → UnsupportedFormats contains it only once.
	links := []map[string]interface{}{
		{"url": "https://example.com/game1.alan", "format": "alan", "fileType": "alan", "size": 100, "isGame": true},
		{"url": "https://example.com/game2.alan", "format": "alan", "fileType": "alan", "size": 200, "isGame": true},
	}
	raw := gameJSONWithFormats("alandupe1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "alandupe1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}

	count := 0
	for _, f := range game.UnsupportedFormats {
		if f == "alan" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("'alan' appears %d times in UnsupportedFormats %v; want exactly 1 (deduplication)",
			count, game.UnsupportedFormats)
	}
}

func TestUnsupportedFormatsMixedLinks(t *testing.T) {
	// Mix of z5 and TADS 3 (tads3 format string → "t3" canonical extension).
	// Both are now known formats, so both appear in DownloadLinks.
	// UnsupportedFormats must be empty — "tads3" is no longer unsupported.
	links := []map[string]interface{}{
		{"url": "https://mirror.ifarchive.org/game.z5", "format": "zcode", "fileType": "zcode", "size": 50000, "isGame": true},
		{"url": "https://example.com/game.t3", "format": "tads3", "fileType": "tads3", "size": 60000, "isGame": true},
	}
	raw := gameJSONWithFormats("mixedgame1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "mixedgame1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}

	if len(game.DownloadLinks) != 2 {
		t.Errorf("DownloadLinks len = %d; want 2 (z5 and t3 both now supported)", len(game.DownloadLinks))
	}
	if len(game.UnsupportedFormats) != 0 {
		t.Errorf("UnsupportedFormats = %v; want empty (tads3 maps to known format t3)", game.UnsupportedFormats)
	}
}

func TestUnsupportedFormatsURLExtensionFallback(t *testing.T) {
	// A link with format="" and fileType="" but a .t3 URL extension.
	// Since TADS 3 support was added, "t3" is now in knownFormats and the
	// URL-extension fallback resolves it to a valid download link.
	// DownloadLinks must contain the .t3 entry; UnsupportedFormats must be empty.
	links := []map[string]interface{}{
		{"url": "https://mirror.ifarchive.org/game.t3", "format": "", "fileType": "", "size": 60000, "isGame": true},
	}
	raw := gameJSONWithFormats("tadsgame1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "tadsgame1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}

	if len(game.DownloadLinks) != 1 {
		t.Errorf("DownloadLinks len = %d; want 1 (.t3 is now a known format)", len(game.DownloadLinks))
	} else if game.DownloadLinks[0].Format != "t3" {
		t.Errorf("DownloadLinks[0].Format = %q; want \"t3\"", game.DownloadLinks[0].Format)
	}
	if len(game.UnsupportedFormats) != 0 {
		t.Errorf("UnsupportedFormats = %v; want empty (.t3 is now supported)", game.UnsupportedFormats)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Cache hit preserves UnsupportedFormats — regression test for the json:"-" bug
//
// Game.UnsupportedFormats has json:"-" so marshaling a Game struct strips it.
// GetGame must cache the raw IFDB response bytes and re-parse on cache hit so
// viewgameToGame re-derives UnsupportedFormats correctly every time.
// ─────────────────────────────────────────────────────────────────────────────

func TestGetGameCacheHitPreservesUnsupportedFormats(t *testing.T) {
	// Build a raw IFDB viewgame body for a game with only an AGT-format link.
	links := []map[string]interface{}{
		{"url": "https://example.com/game.agt", "format": "agt", "fileType": "agt", "size": 2000, "isGame": true},
	}
	raw := gameJSONWithFormats("agtgame1234567890", links)

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	// First call — cache miss, populates cache with raw body.
	g1, hit1, err := c.GetGame(ctx, "", "agtgame1234567890")
	if err != nil {
		t.Fatalf("first GetGame() error = %v", err)
	}
	if hit1 {
		t.Error("first call should not be a cache hit")
	}
	if len(g1.UnsupportedFormats) == 0 {
		t.Fatal("first call: UnsupportedFormats is empty; want ['agt']")
	}

	// Second call — cache hit; UnsupportedFormats must survive the round-trip.
	g2, hit2, err := c.GetGame(ctx, "", "agtgame1234567890")
	if err != nil {
		t.Fatalf("second GetGame() error = %v", err)
	}
	if !hit2 {
		t.Error("second call should be a cache hit")
	}
	if n := atomic.LoadInt32(&callCount); n != 1 {
		t.Errorf("HTTP server called %d times; want 1 (cache should serve second call)", n)
	}
	if len(g2.UnsupportedFormats) == 0 {
		t.Error("cache hit: UnsupportedFormats is empty; want ['agt'] (json:\"-\" bug regression)")
	}
	found := false
	for _, f := range g2.UnsupportedFormats {
		if f == "agt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("cache hit: UnsupportedFormats = %v; want to contain 'agt'", g2.UnsupportedFormats)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// detectFormat — TADS 2/3 IFDB format strings (ARCHITECTURE.md §A.5)
//
// IFDB uses several format labels for TADS games:
//   "tads2" / "tads 2" → canonical extension "gam"  (TADS 2)
//   "tads3" / "tads 3" → canonical extension "t3"   (TADS 3)
//   "tads"             → canonical extension "gam"  (bare label, frob auto-detects)
//
// Verified through GetGame: a download link with these format strings must
// appear in Game.DownloadLinks with the correct Format field.
// ─────────────────────────────────────────────────────────────────────────────

func TestDetectFormatTADS2StringMapsToGam(t *testing.T) {
	links := []map[string]interface{}{
		{"url": "https://ifarchive.org/game.gam", "format": "tads2", "fileType": "tads2", "size": 40000, "isGame": true},
	}
	raw := gameJSONWithFormats("tads2fmt1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "tads2fmt1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if len(game.DownloadLinks) != 1 {
		t.Fatalf("DownloadLinks len = %d; want 1 (tads2 → gam is now supported)", len(game.DownloadLinks))
	}
	if game.DownloadLinks[0].Format != "gam" {
		t.Errorf("DownloadLinks[0].Format = %q; want \"gam\"", game.DownloadLinks[0].Format)
	}
	if len(game.UnsupportedFormats) != 0 {
		t.Errorf("UnsupportedFormats = %v; want empty", game.UnsupportedFormats)
	}
}

func TestDetectFormatTADS3StringMapsToT3(t *testing.T) {
	links := []map[string]interface{}{
		{"url": "https://ifarchive.org/game.t3", "format": "tads3", "fileType": "tads3", "size": 60000, "isGame": true},
	}
	raw := gameJSONWithFormats("tads3fmt1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "tads3fmt1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if len(game.DownloadLinks) != 1 {
		t.Fatalf("DownloadLinks len = %d; want 1 (tads3 → t3 is now supported)", len(game.DownloadLinks))
	}
	if game.DownloadLinks[0].Format != "t3" {
		t.Errorf("DownloadLinks[0].Format = %q; want \"t3\"", game.DownloadLinks[0].Format)
	}
	if len(game.UnsupportedFormats) != 0 {
		t.Errorf("UnsupportedFormats = %v; want empty", game.UnsupportedFormats)
	}
}

func TestDetectFormatBareTADSStringMapsToGam(t *testing.T) {
	// Bare "tads" (no version number) maps to "gam"; frob auto-detects the
	// TADS 2 vs TADS 3 variant from the file header at runtime.
	links := []map[string]interface{}{
		{"url": "https://ifarchive.org/oldgame.gam", "format": "tads", "fileType": "tads", "size": 30000, "isGame": true},
	}
	raw := gameJSONWithFormats("tadsbfmt1234567890", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "tadsbfmt1234567890")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if len(game.DownloadLinks) != 1 {
		t.Fatalf("DownloadLinks len = %d; want 1 (bare tads → gam is now supported)", len(game.DownloadLinks))
	}
	if game.DownloadLinks[0].Format != "gam" {
		t.Errorf("DownloadLinks[0].Format = %q; want \"gam\"", game.DownloadLinks[0].Format)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// knownFormats — "gam" and "t3" are recognised formats
//
// A download link whose fileType is "gam" or "t3" (already canonical) must be
// placed in Game.DownloadLinks, not UnsupportedFormats.
// ─────────────────────────────────────────────────────────────────────────────

func TestKnownFormatsContainsGam(t *testing.T) {
	links := []map[string]interface{}{
		{"url": "https://ifarchive.org/game.gam", "format": "gam", "fileType": "gam", "size": 35000, "isGame": true},
	}
	raw := gameJSONWithFormats("gamfmt12345678901", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "gamfmt12345678901")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if len(game.DownloadLinks) != 1 {
		t.Errorf("DownloadLinks len = %d; want 1 (\"gam\" is a known format)", len(game.DownloadLinks))
	}
	if len(game.UnsupportedFormats) != 0 {
		t.Errorf("UnsupportedFormats = %v; want empty for known format gam", game.UnsupportedFormats)
	}
}

func TestKnownFormatsContainsT3(t *testing.T) {
	links := []map[string]interface{}{
		{"url": "https://ifarchive.org/game.t3", "format": "t3", "fileType": "t3", "size": 55000, "isGame": true},
	}
	raw := gameJSONWithFormats("t3fmt123456789012", links)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	game, _, err := c.GetGame(context.Background(), "", "t3fmt123456789012")
	if err != nil {
		t.Fatalf("GetGame() error = %v", err)
	}
	if len(game.DownloadLinks) != 1 {
		t.Errorf("DownloadLinks len = %d; want 1 (\"t3\" is a known format)", len(game.DownloadLinks))
	}
	if len(game.UnsupportedFormats) != 0 {
		t.Errorf("UnsupportedFormats = %v; want empty for known format t3", game.UnsupportedFormats)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// devsys search filtering (ARCHITECTURE.md §A.6)
//
// parseSearchResponse filters games using devsysSupported:
//   - case-insensitive prefix matching against supportedDevSysPrefixes
//   - empty devsys → included (unknown authoring system, optimistic allow)
//   - Hugo, AGT, Adrift → excluded (unsupported interpreters)
//   - TADS 2, TADS 3, Inform 7, Inform 6 → included
// ─────────────────────────────────────────────────────────────────────────────

// searchJSONWithDevsys returns a single-game search response JSON with the
// given devsys field value.
func searchJSONWithDevsys(devsys, tuid string) []byte {
	raw := fmt.Sprintf(`{
		"games": [{
			"tuid": %q,
			"title": "Test Game",
			"author": "Test Author",
			"published": {"machine": "2000", "printable": "2000"},
			"averageRating": 3.5,
			"hasCoverArt": false,
			"coverArtLink": "",
			"devsys": %q
		}]
	}`, tuid, devsys)
	return []byte(raw)
}

func TestDevsysFilteringTableDriven(t *testing.T) {
	tests := []struct {
		devsys       string
		wantIncluded bool
		desc         string
	}{
		{"TADS 2", true, "TADS 2 is supported via frob"},
		{"TADS 3", true, "TADS 3 is supported via frob"},
		{"tads 2", true, "TADS 2 matching is case-insensitive"},
		{"Inform 7", true, "Inform 7 is supported via dfrotz/glulxe"},
		{"Inform 6", true, "Inform 6 is supported via dfrotz"},
		{"", true, "empty devsys = unknown system = include optimistically"},
		{"Hugo", false, "Hugo has no supported interpreter"},
		{"AGT", false, "AGT has no supported interpreter"},
		{"Adrift", false, "Adrift has no supported interpreter"},
	}

	for i, tt := range tests {
		tt := tt
		i := i
		t.Run(tt.desc, func(t *testing.T) {
			// Each subtest uses a unique TUID and query to avoid cache collisions.
			tuid := fmt.Sprintf("devsystest%010d", i)
			body := searchJSONWithDevsys(tt.devsys, tuid)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(body) //nolint:errcheck
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			query := fmt.Sprintf("devsysq%d", i)
			resp, _, err := c.Search(context.Background(), "", query, 25)
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}

			if tt.wantIncluded && resp.Count != 1 {
				t.Errorf("devsys=%q: Count = %d; want 1 (%s)", tt.devsys, resp.Count, tt.desc)
			}
			if !tt.wantIncluded && resp.Count != 0 {
				t.Errorf("devsys=%q: Count = %d; want 0 (%s)", tt.devsys, resp.Count, tt.desc)
			}
		})
	}
}

func TestDevsysFilteringMultiValueIncludedWhenOnePart(t *testing.T) {
	// IFDB sometimes lists multiple authoring systems: "The Quill, Inform 6".
	// The game is included when at least one comma-separated part matches.
	body := searchJSONWithDevsys("The Quill, Inform 6", "multidevsys00001")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, _, err := c.Search(context.Background(), "", "multi devsys quill inform", 25)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("Count = %d; want 1 (multi-value devsys with at least one supported part)", resp.Count)
	}
}

func TestDevsysFilteringMixedResultSet(t *testing.T) {
	// A response containing both a supported game (Inform 7) and an unsupported
	// game (Hugo). Only the Inform 7 game must survive filtering.
	raw := `{
		"games": [
			{"tuid":"informgame00001","title":"Inform Game","author":"A","published":{"machine":"2010","printable":"2010"},"averageRating":4.0,"hasCoverArt":false,"coverArtLink":"","devsys":"Inform 7"},
			{"tuid":"hugogame000001","title":"Hugo Game","author":"B","published":{"machine":"2005","printable":"2005"},"averageRating":3.0,"hasCoverArt":false,"coverArtLink":"","devsys":"Hugo"}
		]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, raw)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, _, err := c.Search(context.Background(), "", "mixed devsys result", 25)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("Count = %d; want 1 (only Inform 7 game survives filtering)", resp.Count)
	}
	if resp.Results[0].ID != "informgame00001" {
		t.Errorf("Results[0].ID = %q; want \"informgame00001\"", resp.Results[0].ID)
	}
}
