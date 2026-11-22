// Package tmdb provides a minimal TMDb client for enriching Pluto TV items
// with cast photos (profile images). TMDb does not require attribution for
// image URLs, but an API key is required for all requests.
package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	baseURL     = "https://api.themoviedb.org/3"
	imageBase   = "https://image.tmdb.org/t/p/w200"
	searchEP    = baseURL + "/search/multi"
	creditsEP   = baseURL + "/movie/%d/credits"
	tvCreditsEP = baseURL + "/tv/%d/credits"
	clientTimeout = 15 * time.Second
)

// Client is a TMDb API client.
type Client struct {
	HTTP    *http.Client
	APIKey  string
	Logger  Logger
}

// Logger is a minimal logging interface.
type Logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

// CastMember is one cast member with an optional profile photo.
type CastMember struct {
	Name   string `json:"name"`
	Poster string `json:"poster,omitempty"`
	Role   string `json:"role,omitempty"`
}

// New creates a TMDb client.
func New(apiKey string, logger Logger) *Client {
	return &Client{
		HTTP:  &http.Client{Timeout: clientTimeout},
		APIKey: apiKey,
		Logger: logger,
	}
}

// noopLogger discards all log output.
type noopLogger struct{}

func (noopLogger) Debug(msg string, args ...any) {}
func (noopLogger) Warn(msg string, args ...any)  {}

// searchResult is one entry from TMDb multi-search.
type searchResult struct {
	ID          int    `json:"id"`
	MediaType   string `json:"media_type"`
	Title       string `json:"title"`
	Name        string `json:"name"`
	ReleaseDate string `json:"release_date"`
	FirstAirDate string `json:"first_air_date"`
	Popularity  float64 `json:"popularity"`
}

// searchResponse is the TMDb multi-search response.
type searchResponse struct {
	Results []searchResult `json:"results"`
}

// creditPerson is one cast member from TMDb credits.
type creditPerson struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	OriginalName  string `json:"original_name"`
	ProfilePath   string `json:"profile_path"`
	Character     string `json:"character"`
	KnownForDepartment string `json:"known_for_department"`
}

// creditsResponse is the TMDb credits response.
type creditsResponse struct {
	Cast []creditPerson `json:"cast"`
}

// FindID searches Tmd by title and year, returning the best matching ID and media type.
func (c *Client) FindID(ctx context.Context, title, year string) (int, string, error) {
	if c.APIKey == "" {
		return 0, "", fmt.Errorf("no api key")
	}
	params := url.Values{}
	params.Set("api_key", c.APIKey)
	params.Set("query", title)
	params.Set("include_adult", "false")

	reqURL := searchEP + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, "", err
	}

	body, err := c.do(req)
	if err != nil {
		return 0, "", err
	}

	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, "", fmt.Errorf("decode search: %w", err)
	}

	bestID := 0
	bestType := ""
	bestPop := -1.0

	for _, r := range resp.Results {
		if r.MediaType != "movie" && r.MediaType != "tv" {
			continue
		}
		// Match year if available
		releaseDate := r.ReleaseDate
		if r.MediaType == "tv" {
			releaseDate = r.FirstAirDate
		}
		if year != "" && releaseDate != "" && !strings.HasPrefix(releaseDate, year) {
			continue
		}
		if r.Popularity > bestPop {
			bestPop = r.Popularity
			bestID = r.ID
			bestType = r.MediaType
		}
	}

	if bestID == 0 {
		return 0, "", fmt.Errorf("no match")
	}
	return bestID, bestType, nil
}

// Cast fetches the cast for a TMDb movie or TV show.
func (c *Client) Cast(ctx context.Context, id int, mediaType string) ([]CastMember, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("no api key")
	}
	var ep string
	if mediaType == "tv" {
		ep = fmt.Sprintf(tvCreditsEP, id)
	} else {
		ep = fmt.Sprintf(creditsEP, id)
	}

	params := url.Values{}
	params.Set("api_key", c.APIKey)
	reqURL := ep + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}

	var resp creditsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode credits: %w", err)
	}

	cast := make([]CastMember, 0, len(resp.Cast))
	for _, p := range resp.Cast {
		if p.KnownForDepartment != "Acting" {
			continue
		}
		cm := CastMember{Name: p.Name, Role: p.Character}
		if p.ProfilePath != "" {
			cm.Poster = imageBase + p.ProfilePath
		}
		cast = append(cast, cm)
	}

	return cast, nil
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("tmdb HTTP %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}