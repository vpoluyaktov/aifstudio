// Package ifdb provides an HTTP proxy client for the IFDB (ifdb.org) catalogue.
package ifdb

// Game is the normalized representation of an IFDB game entry.
type Game struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Authors       []string       `json:"authors"`
	Year          int            `json:"year,omitempty"`
	Rating        float64        `json:"rating,omitempty"`
	Description   string         `json:"description,omitempty"`
	CoverArtURL   string         `json:"coverArtURL,omitempty"`
	DownloadLinks []DownloadLink `json:"downloadLinks"`
	Formats       []string       `json:"formats"`
	// UnsupportedFormats collects raw format/fileType values from IFDB links whose
	// extension was not recognised by detectFormat. Not serialised to JSON.
	UnsupportedFormats []string `json:"-"`
}

// DownloadLink describes a single downloadable story file.
type DownloadLink struct {
	URL    string `json:"url"`
	Format string `json:"format"`
	Size   int64  `json:"size,omitempty"`
}

// SearchResult is a light-weight Game without description or download links.
type SearchResult struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Authors     []string `json:"authors"`
	Year        int      `json:"year,omitempty"`
	Rating      float64  `json:"rating,omitempty"`
	CoverArtURL string   `json:"coverArtURL,omitempty"`
	Formats     []string `json:"formats"`
}

// SearchResponse is the envelope returned by GET /api/ifdb/search.
type SearchResponse struct {
	Query   string         `json:"query"`
	Count   int            `json:"count"`
	Results []SearchResult `json:"results"`
}

// ---- IFDB raw JSON shapes (actual API as of 2026) ----

// ifdbSearchResult is the raw structure returned by IFDB's search JSON endpoint.
// Top-level key is "games", each entry uses "tuid" for the ID.
type ifdbSearchResult struct {
	Games []ifdbSearchEntry `json:"games"`
}

type ifdbSearchEntry struct {
	TUID          string        `json:"tuid"`
	Title         string        `json:"title"`
	Author        string        `json:"author"`
	Published     ifdbPublished `json:"published"`
	AverageRating float64       `json:"averageRating"`
	HasCoverArt   bool          `json:"hasCoverArt"`
	CoverArtLink  string        `json:"coverArtLink"`
	Devsys        string        `json:"devsys"`
}

type ifdbPublished struct {
	Machine   string `json:"machine"`
	Printable string `json:"printable"`
}

// ifdbViewgameResponse is the raw structure returned by IFDB's viewgame JSON endpoint.
// It has three top-level sections: identification, bibliographic, ifdb.
type ifdbViewgameResponse struct {
	Identification ifdbIdentification `json:"identification"`
	Bibliographic  ifdbBibliographic  `json:"bibliographic"`
	IFDB           ifdbSection        `json:"ifdb"`
}

type ifdbIdentification struct {
	IFIDs  []string `json:"ifids"`
	Format string   `json:"format"`
}

type ifdbBibliographic struct {
	Title          string `json:"title"`
	Author         string `json:"author"`
	FirstPublished string `json:"firstpublished"`
	Description    string `json:"description"`
	Genre          string `json:"genre"`
	Language       string `json:"language"`
}

type ifdbSection struct {
	TUID      string        `json:"tuid"`
	CoverArt  *ifdbCoverArt `json:"coverart"`
	Downloads ifdbDownloads `json:"downloads"`
}

type ifdbCoverArt struct {
	URL string `json:"url"`
}

type ifdbDownloads struct {
	Links []ifdbLink `json:"links"`
}

type ifdbLink struct {
	URL      string `json:"url"`
	Format   string `json:"format"`
	FileType string `json:"fileType"`
	Title    string `json:"title"`
	IsGame   bool   `json:"isGame"`
	Size     int64  `json:"size"`
}
