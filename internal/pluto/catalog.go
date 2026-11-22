package pluto

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var yearRE = regexp.MustCompile(`(19|20)\d{2}`)

// FindCategory returns the items of the first category whose name matches
// case-insensitively.
func FindCategory(data map[string]any, name string) []any {
	for _, raw := range asSlice(data["categories"]) {
		category := asMap(raw)
		if category == nil {
			continue
		}
		if strings.EqualFold(asString(category["name"]), name) {
			return asSlice(category["items"])
		}
	}
	return nil
}

// FindItem returns an item by Pluto ID across every category.
func FindItem(data map[string]any, itemID string) map[string]any {
	for _, rawCategory := range asSlice(data["categories"]) {
		category := asMap(rawCategory)
		if category == nil {
			continue
		}
		for _, rawItem := range asSlice(category["items"]) {
			item := asMap(rawItem)
			if item != nil && idString(item["_id"]) == itemID {
				return item
			}
		}
	}
	return nil
}

// CatalogItem converts a Pluto catalog entry into a Stremio/Nuvio meta preview.
func CatalogItem(item map[string]any, mediaType string) map[string]any {
	itemID := idString(item["_id"])
	name := asString(item["name"])
	if itemID == "" || name == "" {
		return nil
	}
	prefix := "pluto:movie:"
	if mediaType == "series" {
		prefix = "pluto:series:"
	}
	result := map[string]any{
		"id":          prefix + itemID,
		"type":        mediaType,
		"name":        name,
		"poster":      posterFor(item),
		"background":  backgroundFor(item),
		"description": firstNonEmpty(asString(item["summary"]), asString(item["description"])),
		"genres":      genresFor(item),
		"posterShape": "poster",
	}
	if year := itemYear(item); year != "" {
		result["releaseInfo"] = year
	}
	if rating := ratingFor(item); rating != "" {
		result["ageRating"] = rating
	}
	if runtime := runtimeMinutes(item["duration"]); runtime != "" {
		result["runtime"] = runtime
	}
	return result
}

type searchHit struct {
	meta  map[string]any
	score int
}

// SearchItems returns catalog items whose title, genre, or description match
// the (case-insensitive) query. Results are deduplicated by Pluto ID and
// ranked: exact title, title prefix, title match, genre match, description
// match. The cached categories response is reused, so search needs no extra
// Pluto API call and works entirely on the device.
func SearchItems(data map[string]any, query string, limit int) []map[string]any {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" || limit <= 0 {
		return []map[string]any{}
	}
	hits := make([]searchHit, 0)
	seen := make(map[string]struct{})
	for _, rawCategory := range asSlice(data["categories"]) {
		category := asMap(rawCategory)
		if category == nil {
			continue
		}
		for _, rawItem := range asSlice(category["items"]) {
			item := asMap(rawItem)
			if item == nil {
				continue
			}
			itemType := asString(item["type"])
			if itemType != "movie" && itemType != "series" {
				continue
			}
			meta := CatalogItem(item, itemType)
			if meta == nil {
				continue
			}
			id, _ := meta["id"].(string)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			score := searchScore(query, meta)
			if score <= 0 {
				continue
			}
			seen[id] = struct{}{}
			hits = append(hits, searchHit{meta: meta, score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		left, _ := hits[i].meta["name"].(string)
		right, _ := hits[j].meta["name"].(string)
		return strings.ToLower(left) < strings.ToLower(right)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.meta)
	}
	return out
}

func searchScore(query string, meta map[string]any) int {
	name := strings.ToLower(asString(meta["name"]))
	switch {
	case name == query:
		return 100
	case strings.HasPrefix(name, query):
		return 80
	case strings.Contains(name, query):
		return 60
	}
	if genres, ok := meta["genres"].([]string); ok {
		for _, genre := range genres {
			if strings.Contains(strings.ToLower(genre), query) {
				return 40
			}
		}
	}
	if strings.Contains(strings.ToLower(asString(meta["description"])), query) {
		return 20
	}
	return 0
}

// MetaForItem converts a Pluto item into a Nuvio metadata object.
func MetaForItem(item map[string]any, mediaType string) map[string]any {
	itemID := idString(item["_id"])
	prefix := "pluto:movie:"
	if mediaType == "series" {
		prefix = "pluto:series:"
	}
	meta := map[string]any{
		"id":          prefix + itemID,
		"type":        mediaType,
		"name":        firstNonEmpty(asString(item["name"]), "Untitled"),
		"poster":      posterFor(item),
		"background":  backgroundFor(item),
		"description": firstNonEmpty(asString(item["description"]), asString(item["summary"])),
		"genres":      genresFor(item),
		"cast":        peopleFor(item, "actors"),
		"director":    peopleFor(item, "directors"),
	}
	if year := itemYear(item); year != "" {
		meta["releaseInfo"] = year
	}
	if rating := ratingFor(item); rating != "" {
		meta["ageRating"] = rating
	}
	if runtime := runtimeMinutes(item["duration"]); runtime != "" {
		meta["runtime"] = runtime
	}
	return meta
}

// SeriesMeta adds seasons and episodes to a series metadata object.
func SeriesMeta(ctx context.Context, client *Client, item map[string]any) (map[string]any, error) {
	meta := MetaForItem(item, "series")
	data, err := client.SeriesSeasons(ctx, idString(item["_id"]))
	if err != nil {
		return nil, err
	}
	videos := make([]map[string]any, 0, 32)
	years := make([]int, 0, 8)
	for _, rawSeason := range asSlice(data["seasons"]) {
		season := asMap(rawSeason)
		if season == nil {
			continue
		}
		seasonNumber := numberValue(season["number"])
		for _, rawEpisode := range asSlice(season["episodes"]) {
			episode := asMap(rawEpisode)
			if episode == nil {
				continue
			}
			if episode["season"] == nil {
				episode["season"] = seasonNumber
			}
			video := EpisodeMeta(episode)
			if video == nil {
				continue
			}
			videos = append(videos, video)
			if year := firstYear(episode); year != "" {
				if parsed, err := strconv.Atoi(year); err == nil {
					years = append(years, parsed)
				}
			}
		}
	}
	sort.SliceStable(videos, func(i, j int) bool {
		si, _ := intValue(videos[i]["season"])
		sj, _ := intValue(videos[j]["season"])
		if si != sj {
			return si < sj
		}
		ei, _ := intValue(videos[i]["episode"])
		ej, _ := intValue(videos[j]["episode"])
		return ei < ej
	})
	if len(videos) > 0 {
		meta["videos"] = videos
		behaviorHints, _ := meta["behaviorHints"].(map[string]any)
		if behaviorHints == nil {
			behaviorHints = map[string]any{}
		}
		behaviorHints["defaultVideoId"] = videos[0]["id"]
		meta["behaviorHints"] = behaviorHints
	}
	if _, ok := meta["releaseInfo"]; !ok && len(years) > 0 {
		sort.Ints(years)
		meta["releaseInfo"] = strconv.Itoa(years[0])
	}
	return meta, nil
}

// EpisodeMeta converts a Pluto episode into a Nuvio video object.
func EpisodeMeta(episode map[string]any) map[string]any {
	episodeID := idString(episode["_id"])
	if episodeID == "" {
		return nil
	}
	result := map[string]any{
		"id":        "pluto:episode:" + episodeID,
		"title":     firstNonEmpty(asString(episode["name"]), "Episode"),
		"overview":  firstNonEmpty(asString(episode["description"]), asString(episode["summary"])),
		"thumbnail": firstNonEmpty(posterFor(episode), backgroundFor(episode)),
	}
	if season := numberValue(episode["season"]); season != nil {
		result["season"] = season
	}
	if number := numberValue(episode["number"]); number != nil {
		result["episode"] = number
	}
	if year := firstYear(episode); year != "" {
		result["released"] = year
	}
	if runtime := runtimeMinutes(episode["duration"]); runtime != "" {
		result["runtime"] = runtime
	}
	return result
}

func posterFor(item map[string]any) string {
	return firstNonEmpty(
		coverURL(item, "347:500"),
		imageURL(item["featuredImage"]),
		imageURL(item["poster16_9"]),
	)
}

func backgroundFor(item map[string]any) string {
	return firstNonEmpty(
		coverURL(item, "16:9"),
		imageURL(item["featuredImage"]),
		imageURL(item["poster16_9"]),
	)
}

func coverURL(item map[string]any, aspect string) string {
	for _, raw := range asSlice(item["covers"]) {
		cover := asMap(raw)
		if cover == nil {
			continue
		}
		if asString(cover["aspectRatio"]) == aspect {
			if value := asString(cover["url"]); value != "" {
				return value
			}
		}
	}
	return ""
}

func imageURL(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		return firstNonEmpty(asString(typed["path"]), asString(typed["url"]))
	default:
		return ""
	}
}

func genresFor(item map[string]any) []string {
	switch typed := item["genre"].(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			return []string{strings.TrimSpace(typed)}
		}
	case []any:
		out := make([]string, 0, len(typed))
		for _, value := range typed {
			if text := asString(value); text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return []string{}
}

func peopleFor(item map[string]any, key string) []string {
	clip := asMap(item["clip"])
	if clip == nil {
		return []string{}
	}
	out := make([]string, 0)
	for _, raw := range asSlice(clip[key]) {
		if person := asMap(raw); person != nil {
			if name := firstNonEmpty(asString(person["name"]), asString(person["fullName"])); name != "" {
				out = append(out, name)
				continue
			}
		}
		if name := asString(raw); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func itemYear(item map[string]any) string {
	if year := firstYear(item); year != "" {
		return year
	}
	years := make([]int, 0)
	for _, rawSeason := range asSlice(item["seasons"]) {
		season := asMap(rawSeason)
		if season == nil {
			continue
		}
		for _, rawEpisode := range asSlice(season["episodes"]) {
			episode := asMap(rawEpisode)
			if episode == nil {
				continue
			}
			if year := firstYear(episode); year != "" {
				if parsed, err := strconv.Atoi(year); err == nil {
					years = append(years, parsed)
				}
			}
		}
	}
	if len(years) == 0 {
		return ""
	}
	sort.Ints(years)
	return strconv.Itoa(years[0])
}

func firstYear(item map[string]any) string {
	clip := asMap(item["clip"])
	values := []any{item["releaseDate"], item["slug"]}
	if clip != nil {
		values = append([]any{clip["originalReleaseDate"]}, values...)
	}
	for _, value := range values {
		text := asString(value)
		if text == "" {
			continue
		}
		if match := yearRE.FindString(text); match != "" {
			return match
		}
	}
	return ""
}

func runtimeMinutes(value any) string {
	milliseconds, ok := floatValue(value)
	if !ok || milliseconds <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0f min", math.Round(milliseconds/60000))
}

func ratingFor(item map[string]any) string {
	value := item["rating"]
	if value == nil {
		return ""
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return ""
	}
	return text
}

func asMap(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

func asSlice(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return nil
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case nil:
		return ""
	default:
		return ""
	}
}

func idString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}

func floatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func numberValue(value any) any {
	if value == nil {
		return nil
	}
	if number, ok := floatValue(value); ok {
		if math.Trunc(number) == number {
			return int(number)
		}
		return number
	}
	return nil
}

func intValue(value any) (int, bool) {
	if number, ok := floatValue(value); ok {
		return int(number), true
	}
	return 0, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
