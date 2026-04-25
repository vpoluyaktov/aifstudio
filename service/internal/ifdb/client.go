package ifdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

// knownFormats lists the IF file extensions the platform supports.
var knownFormats = []string{"z3", "z4", "z5", "z6", "z7", "z8", "zblorb", "ulx", "gblorb", "gam", "t3"}

// supportedDevSysPrefixes lists lowercase prefix strings that identify authoring
// systems whose games StoryCloud can execute. Matching is case-insensitive and
// prefix-based so that "Inform 7", "Inform6", "inform 5" etc. all resolve via
// the "inform" prefix, and "Alan 2"/"Alan 3" resolve via "alan".
//
// Multi-value IFDB entries ("The Quill, Inform 6, ZIL") are split on comma;
// the game is included when at least one part matches a supported prefix.
// Games with an empty devsys are included (optimistic unknown = allow).
var supportedDevSysPrefixes = []string{
	"inform",    // Inform 5, 6, 7, and any "Inform N.N" variant
	"zil",       // Infocom's Zork Implementation Language → Z-machine
	"z-machine", // explicit Z-machine label
	"zcode",     // IFDB sometimes uses "zcode" for Z-machine games
	"glulx",     // Glulx / Inform 7 Glulx target
	"tads",      // TADS (bare), TADS 2, TADS 3 — frob auto-detects
}

// htmlTagRE strips HTML tags for description cleanup.
var htmlTagRE = regexp.MustCompile(`<[^>]*>`)

// Client is the IFDB proxy HTTP client with retries, rate limiting, and caching.
type Client struct {
	baseURL   string
	userAgent string
	cacheTTL  time.Duration
	http      *http.Client

	globalRL *rate.Limiter
	perIPRL  sync.Map // map[string]*rate.Limiter

	perIPQPS   float64
	perIPBurst int

	cache  *memCache
	sf     singleflight.Group
}

// ClientOptions configures the IFDB client.
type ClientOptions struct {
	BaseURL      string
	UserAgent    string
	CacheTTL     time.Duration
	GlobalQPS    float64
	GlobalBurst  int
	PerIPQPS     float64
	PerIPBurst   int
}

// NewClient creates a new IFDB client.
func NewClient(opts ClientOptions) *Client {
	return &Client{
		baseURL:    opts.BaseURL,
		userAgent:  opts.UserAgent,
		cacheTTL:   opts.CacheTTL,
		http:       &http.Client{Timeout: 10 * time.Second},
		globalRL:   rate.NewLimiter(rate.Limit(opts.GlobalQPS), opts.GlobalBurst),
		perIPQPS:   opts.PerIPQPS,
		perIPBurst: opts.PerIPBurst,
		cache:      newMemCache(),
	}
}

// SeedCache pre-populates the in-memory cache from Firestore warm-up entries.
func (c *Client) SeedCache(tuid string, payload []byte, expiresAt time.Time) {
	c.cache.seed(tuid, payload, expiresAt)
}

// Search proxies IFDB search and returns up to limit results.
func (c *Client) Search(ctx context.Context, clientIP, query string, limit int) (*SearchResponse, bool, error) {
	cacheKey := "search:" + query + ":" + strconv.Itoa(limit)
	if data, hit := c.cache.get(cacheKey); hit {
		var resp SearchResponse
		if err := json.Unmarshal(data, &resp); err == nil {
			return &resp, true, nil
		}
	}

	if err := c.checkRate(ctx, clientIP); err != nil {
		return nil, false, err
	}

	rawURL := fmt.Sprintf("%s/search?searchfor=%s&json&ifiction&limit=%d",
		c.baseURL, url.QueryEscape(query), limit)

	var resp SearchResponse
	err := c.doWithRetry(ctx, rawURL, func(body []byte) error {
		return parseSearchResponse(body, query, &resp)
	})
	if err != nil {
		return nil, false, err
	}

	if data, err := json.Marshal(resp); err == nil {
		c.cache.set(cacheKey, data, time.Now().Add(c.cacheTTL))
	}
	return &resp, false, nil
}

// GetGame fetches a single game by IFDB TUID.
func (c *Client) GetGame(ctx context.Context, clientIP, tuid string) (*Game, bool, error) {
	if data, hit := c.cache.get(tuid); hit {
		var g Game
		if err := parseGameResponse(data, &g); err == nil {
			return &g, true, nil
		}
	}

	result, err, _ := c.sf.Do(tuid, func() (interface{}, error) {
		if err := c.checkRate(ctx, clientIP); err != nil {
			return nil, err
		}
		rawURL := fmt.Sprintf("%s/viewgame?id=%s&json", c.baseURL, url.QueryEscape(tuid))
		var g Game
		var rawBody []byte
		err := c.doWithRetry(ctx, rawURL, func(body []byte) error {
			rawBody = body
			return parseGameResponse(body, &g)
		})
		if err != nil {
			return nil, err
		}
		if len(rawBody) > 0 {
			c.cache.set(tuid, rawBody, time.Now().Add(c.cacheTTL))
		}
		return &g, nil
	})
	if err != nil {
		return nil, false, err
	}
	return result.(*Game), false, nil
}

// ErrRateLimit is returned when a rate limiter denies the request.
type ErrRateLimit struct{}

func (ErrRateLimit) Error() string { return "rate limit exceeded" }

// ErrUpstream is returned when IFDB returns a persistent 5xx or bad response.
type ErrUpstream struct{ Msg string }

func (e ErrUpstream) Error() string { return e.Msg }

// ErrNotFound is returned when IFDB has no game for the given TUID.
type ErrNotFound struct{}

func (ErrNotFound) Error() string { return "game not found" }

func (c *Client) checkRate(ctx context.Context, clientIP string) error {
	if !c.globalRL.Allow() {
		return ErrRateLimit{}
	}
	if !c.ipLimiter(clientIP).Allow() {
		return ErrRateLimit{}
	}
	return nil
}

func (c *Client) ipLimiter(ip string) *rate.Limiter {
	if v, ok := c.perIPRL.Load(ip); ok {
		return v.(*rate.Limiter)
	}
	l := rate.NewLimiter(rate.Limit(c.perIPQPS), c.perIPBurst)
	actual, _ := c.perIPRL.LoadOrStore(ip, l)
	return actual.(*rate.Limiter)
}

func (c *Client) doWithRetry(ctx context.Context, rawURL string, parse func([]byte) error) error {
	const maxRetries = 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("User-Agent", c.userAgent)

		resp, err := c.http.Do(req)
		if err != nil {
			if attempt == maxRetries {
				return ErrUpstream{Msg: "ifdb upstream unreachable: " + err.Error()}
			}
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		resp.Body.Close()
		if err != nil {
			return ErrUpstream{Msg: "reading ifdb response: " + err.Error()}
		}

		if resp.StatusCode >= 500 {
			if attempt == maxRetries {
				return ErrUpstream{Msg: fmt.Sprintf("ifdb upstream %d after %d retries", resp.StatusCode, maxRetries)}
			}
			continue
		}
		if resp.StatusCode == 404 {
			return ErrNotFound{}
		}
		if resp.StatusCode != 200 {
			return ErrUpstream{Msg: fmt.Sprintf("ifdb upstream %d", resp.StatusCode)}
		}

		if !utf8.Valid(body) {
			slog.Error("IFDB returned invalid UTF-8", "url", rawURL)
			return ErrUpstream{Msg: "ifdb upstream malformed response"}
		}

		if err := parse(body); err != nil {
			slog.Error("IFDB response parse error", "url", rawURL, "err", err)
			return ErrUpstream{Msg: "ifdb upstream malformed response"}
		}
		return nil
	}
	return ErrUpstream{Msg: "ifdb upstream failed after retries"}
}

// ---- parsers ----

func parseSearchResponse(body []byte, query string, out *SearchResponse) error {
	var raw ifdbSearchResult
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("unmarshal search: %w", err)
	}
	out.Query = query
	out.Results = make([]SearchResult, 0, len(raw.Games))
	for _, e := range raw.Games {
		if !devsysSupported(e.Devsys) {
			continue
		}
		out.Results = append(out.Results, searchEntryToResult(e))
	}
	out.Count = len(out.Results)
	return nil
}

func parseGameResponse(body []byte, out *Game) error {
	var raw ifdbViewgameResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("unmarshal game: %w", err)
	}
	if raw.IFDB.TUID == "" {
		return ErrNotFound{}
	}
	*out = viewgameToGame(raw)
	return nil
}

func searchEntryToResult(e ifdbSearchEntry) SearchResult {
	sr := SearchResult{
		ID:      e.TUID,
		Title:   e.Title,
		Authors: splitAuthors(e.Author),
		Rating:  e.AverageRating,
		Formats: []string{},
	}
	if y, err := strconv.Atoi(e.Published.Machine); err == nil && y > 0 {
		sr.Year = y
	}
	if e.CoverArtLink != "" {
		sr.CoverArtURL = e.CoverArtLink
	} else if e.HasCoverArt && e.TUID != "" {
		sr.CoverArtURL = "https://ifdb.org/coverart?id=" + e.TUID
	}
	return sr
}

func viewgameToGame(raw ifdbViewgameResponse) Game {
	g := Game{
		ID:            raw.IFDB.TUID,
		Title:         raw.Bibliographic.Title,
		Authors:       splitAuthors(raw.Bibliographic.Author),
		DownloadLinks: make([]DownloadLink, 0),
		Formats:       []string{},
	}
	if y, err := strconv.Atoi(raw.Bibliographic.FirstPublished); err == nil && y > 0 {
		g.Year = y
	}
	if raw.Bibliographic.Description != "" {
		g.Description = stripHTML(raw.Bibliographic.Description)
	}
	if raw.IFDB.CoverArt != nil && raw.IFDB.CoverArt.URL != "" {
		g.CoverArtURL = raw.IFDB.CoverArt.URL
	}
	var unsupported []string
	for _, l := range raw.IFDB.Downloads.Links {
		if l.URL == "" {
			continue
		}
		fmt := detectFormat(l.URL, l.Format, l.FileType)
		if fmt == "" {
			// Collect the raw format/fileType for the unsupported-format error.
			added := false
			for _, candidate := range []string{l.Format, l.FileType} {
				if candidate != "" {
					unsupported = appendUnique(unsupported, candidate)
					added = true
				}
			}
			// When both format and fileType are blank, fall back to the URL path extension.
			if !added {
				if u, err := url.Parse(l.URL); err == nil {
					if ext := path.Ext(strings.ToLower(u.Path)); ext != "" {
						unsupported = appendUnique(unsupported, strings.TrimPrefix(ext, "."))
					}
				}
			}
			continue
		}
		g.DownloadLinks = append(g.DownloadLinks, DownloadLink{URL: l.URL, Format: fmt, Size: l.Size})
		g.Formats = appendUnique(g.Formats, fmt)
	}
	g.UnsupportedFormats = unsupported
	return g
}

// splitAuthors splits an IFDB author string like "Alice, Bob, and Carol" into a slice.
func splitAuthors(single string) []string {
	if single == "" {
		return []string{}
	}
	// Normalize ", and " and " and " separators.
	s := strings.ReplaceAll(single, ", and ", " and ")
	parts := strings.Split(s, " and ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		// Also split on commas for lists without "and".
		for _, sub := range strings.Split(p, ",") {
			sub = strings.TrimSpace(sub)
			if sub != "" {
				out = append(out, sub)
			}
		}
	}
	return out
}


func detectFormat(rawURL, format, fileType string) string {
	// Try explicit format/fileType fields first.
	for _, candidate := range []string{format, fileType} {
		candidate = strings.TrimPrefix(strings.ToLower(candidate), ".")
		if candidate == "" {
			continue
		}
		// Map IFDB TADS format strings to our canonical extensions.
		switch candidate {
		case "tads3", "tads 3":
			return "t3"
		case "tads2", "tads 2", "tads":
			return "gam"
		}
		for _, kf := range knownFormats {
			if candidate == kf {
				return kf
			}
		}
	}
	// Fall back to URL file extension (handles IFDB "zcode" format → .z5 URL, etc.).
	u, err := url.Parse(rawURL)
	if err == nil {
		path := strings.ToLower(u.Path)
		for _, ext := range knownFormats {
			if strings.HasSuffix(path, "."+ext) {
				return ext
			}
		}
	}
	return ""
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// devsysSupported returns true when the IFDB devsys value is playable on this
// platform. Matching is case-insensitive and prefix-based (see
// supportedDevSysPrefixes). An empty devsys (unknown) is included optimistically.
// Multi-value entries like "The Quill, Inform 6, ZIL" are split on comma;
// the game is included when at least one part matches.
func devsysSupported(devsys string) bool {
	if devsys == "" {
		return true
	}
	for _, part := range strings.Split(devsys, ",") {
		norm := strings.ToLower(strings.TrimSpace(part))
		for _, prefix := range supportedDevSysPrefixes {
			if strings.HasPrefix(norm, prefix) {
				return true
			}
		}
	}
	return false
}

func stripHTML(s string) string {
	return htmlTagRE.ReplaceAllString(s, "")
}

// ExtractClientIP returns the request IP for rate-limiting.
// Prefers the first entry in X-Forwarded-For; falls back to RemoteAddr.
func ExtractClientIP(xForwardedFor, remoteAddr string) string {
	if xForwardedFor != "" {
		parts := strings.SplitN(xForwardedFor, ",", 2)
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// PreferredFormat returns the first compatible format from links per the
// architecture preference order: z5, z8, zblorb, ulx, gblorb, t3, gam.
func PreferredFormat(links []DownloadLink) (DownloadLink, bool) {
	order := []string{"z5", "z8", "zblorb", "ulx", "gblorb", "t3", "gam"}
	for _, pref := range order {
		for _, l := range links {
			if l.Format == pref {
				return l, true
			}
		}
	}
	// Any supported format.
	for _, l := range links {
		for _, kf := range knownFormats {
			if l.Format == kf {
				return l, true
			}
		}
	}
	return DownloadLink{}, false
}
