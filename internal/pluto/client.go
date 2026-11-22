// Package pluto talks to Pluto TV's catalog, boot, and HLS stitcher services.
package pluto

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"nuvio-plutonium/internal/cache"
)

const (
	UserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
	categoriesEP = "https://api.pluto.tv/v3/vod/categories"
	seriesEP     = "https://api.pluto.tv/v3/vod/series/"
	bootEP       = "https://boot.pluto.tv/v4/start"
)

// HTTPError is returned for non-2xx upstream responses.
type HTTPError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *HTTPError) Error() string {
	snippet := strings.TrimSpace(e.Body)
	if len(snippet) > 240 {
		snippet = snippet[:240]
	}
	if snippet == "" {
		return fmt.Sprintf("upstream HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("upstream HTTP %d: %s", e.StatusCode, snippet)
}

// Retryable reports whether a request may be retried.
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// Client is a Pluto TV API and CDN client.
type Client struct {
	HTTP    *http.Client
	XFF     string
	Country string
	Logger  *slog.Logger

	cache *cache.Cache
}

type Options struct {
	XFF     string
	Country string
	Logger  *slog.Logger
}

func New(opts Options) *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   96,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	country := strings.TrimSpace(opts.Country)
	if country == "" {
		country = "US"
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		HTTP:    &http.Client{Transport: transport, Timeout: 75 * time.Second},
		XFF:     strings.TrimSpace(opts.XFF),
		Country: country,
		Logger:  logger,
		cache:   cache.New(),
	}
}

// Boot is a Pluto session used to build stitcher URLs.
type Boot struct {
	Token          string
	Stitcher       string
	StitcherParams string
}

// Boot fetches (or reuses) a Pluto session.
func (c *Client) Boot(ctx context.Context) (*Boot, error) {
	key := "boot:" + c.Country
	value, err := c.cache.GetOrLoad(key, 5*time.Minute, func() (any, error) {
		params := url.Values{}
		params.Set("appName", "web")
		params.Set("appVersion", "1")
		params.Set("deviceVersion", "0.0.0")
		params.Set("deviceModel", "web")
		params.Set("deviceMake", "firefox")
		params.Set("deviceType", "web")
		params.Set("clientID", newUUID())
		params.Set("clientModelNumber", "1.0.0")
		params.Set("serverSideAds", "false")
		params.Set("country", c.Country)

		body, err := c.GetBytes(ctx, bootEP+"?"+params.Encode(), 30*time.Second)
		if err != nil {
			return nil, err
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("decode boot response: %w", err)
		}
		token, _ := data["sessionToken"].(string)
		if token == "" {
			return nil, errors.New("pluto boot response had no session token")
		}
		servers, _ := data["servers"].(map[string]any)
		stitcher, _ := servers["stitcher"].(string)
		if stitcher == "" {
			return nil, errors.New("pluto boot response had no stitcher")
		}
		stitcherParams, _ := data["stitcherParams"].(string)
		return &Boot{
			Token:          token,
			Stitcher:       stitcher,
			StitcherParams: strings.Trim(stitcherParams, "?&"),
		}, nil
	})
	if err != nil {
		return nil, err
	}
	boot, ok := value.(*Boot)
	if !ok || boot == nil {
		return nil, errors.New("invalid cached boot session")
	}
	return boot, nil
}

// MasterURL builds the raw Pluto stitcher master URL for a movie or episode.
func (c *Client) MasterURL(ctx context.Context, mediaID string) (string, error) {
	boot, err := c.Boot(ctx)
	if err != nil {
		return "", err
	}
	host := stitcherHost(boot.Stitcher)
	if host == "" {
		return "", errors.New("pluto stitcher host was empty")
	}
	query := url.Values{}
	if boot.StitcherParams != "" {
		if parsed, err := url.ParseQuery(boot.StitcherParams); err == nil {
			query = parsed
		}
	}
	query.Set("jwt", boot.Token)
	query.Set("masterJWTPassthrough", "1")
	return (&url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     "/v2/stitch/hls/episode/" + url.PathEscape(mediaID) + "/master.m3u8",
		RawQuery: query.Encode(),
	}).String(), nil
}

func stitcherHost(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return strings.Trim(value, "/")
}

// Categories returns Pluto's on-demand categories with their items.
func (c *Client) Categories(ctx context.Context) (map[string]any, error) {
	key := "categories:" + c.Country
	value, err := c.cache.GetOrLoad(key, 15*time.Minute, func() (any, error) {
		query := url.Values{}
		query.Set("includeItems", "true")
		query.Set("deviceType", "web")
		query.Set("country", c.Country)
		body, err := c.GetBytes(ctx, categoriesEP+"?"+query.Encode(), 45*time.Second)
		if err != nil {
			return nil, err
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("decode categories response: %w", err)
		}
		if _, ok := data["categories"].([]any); !ok {
			return nil, errors.New("pluto categories response had an unexpected shape")
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	data, ok := value.(map[string]any)
	if !ok || data == nil {
		return nil, errors.New("invalid cached categories response")
	}
	return data, nil
}

// SeriesSeasons returns a Pluto series' seasons and episodes.
func (c *Client) SeriesSeasons(ctx context.Context, seriesID string) (map[string]any, error) {
	key := "series:" + c.Country + ":" + seriesID
	value, err := c.cache.GetOrLoad(key, 15*time.Minute, func() (any, error) {
		query := url.Values{}
		query.Set("includeItems", "true")
		query.Set("deviceType", "web")
		query.Set("country", c.Country)
		endpoint := seriesEP + url.PathEscape(seriesID) + "/seasons?" + query.Encode()
		body, err := c.GetBytes(ctx, endpoint, 30*time.Second)
		if err != nil {
			return nil, err
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("decode seasons response: %w", err)
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	data, ok := value.(map[string]any)
	if !ok || data == nil {
		return nil, errors.New("invalid cached seasons response")
	}
	return data, nil
}

// GetText fetches a URL as UTF-8 text.
func (c *Client) GetText(ctx context.Context, rawURL string, timeout time.Duration) (string, error) {
	body, err := c.GetBytes(ctx, rawURL, timeout)
	if err != nil {
		return "", err
	}
	text := string(body)
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	return text, nil
}

// GetBytes fetches a URL and returns its body.
func (c *Client) GetBytes(ctx context.Context, rawURL string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "*/*")
	if c.XFF != "" {
		req.Header.Set("X-Forwarded-For", c.XFF)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        rawURL,
			Body:       string(body),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// CacheLen reports the number of cached API responses.
func (c *Client) CacheLen() int {
	return c.cache.Len()
}

// CacheStats exposes the upstream cache for the health endpoint.
func (c *Client) CacheStats() map[string]int {
	return map[string]int{"entries": c.cache.Len()}
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 32)
	hex.Encode(dst, b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", dst[0:8], dst[8:12], dst[12:16], dst[16:20], dst[20:32])
}
