// Package addon serves the Nuvio/Stremio catalog, metadata, streams, and
// ad-free HLS endpoints.
package addon

// Row is one visible catalog row.
type Row struct {
	ID       string
	Name     string
	Category string
	Type     string
}

// Rows preserves the seven Pluto catalogs that were already registered in
// Nuvio. Keep the IDs stable so existing Home rows keep working.
var Rows = []Row{
	{ID: "pluto-movies", Name: "Pluto Movies", Category: "Most Popular Movies", Type: "movie"},
	{ID: "pluto-series", Name: "Pluto TV Series", Category: "Top TV Series", Type: "series"},
	{ID: "pluto-top-movies", Name: "Top Movies on Pluto", Category: "Top Titles", Type: "movie"},
	{ID: "pluto-top-series", Name: "Top Series on Pluto", Category: "Top Titles", Type: "series"},
	{ID: "pluto-new-movies", Name: "New Movies on Pluto", Category: "New Movies This Month", Type: "movie"},
	{ID: "pluto-leaving-movies", Name: "Leaving Pluto Soon", Category: "Leaving Soon", Type: "movie"},
	{ID: "pluto-leaving-series", Name: "Leaving Pluto Soon (TV)", Category: "Leaving Soon", Type: "series"},
}

var rowByID = func() map[string]Row {
	rows := make(map[string]Row, len(Rows))
	for _, row := range Rows {
		rows[row.ID] = row
	}
	return rows
}()

// Manifest returns the addon manifest.
func Manifest(version string) map[string]any {
	catalogs := make([]map[string]any, 0, len(Rows))
	for _, row := range Rows {
		catalogs = append(catalogs, map[string]any{
			"type": row.Type,
			"id":   row.ID,
			"name": row.Name,
		})
	}
	return map[string]any{
		"id":      "org.maba.pluto-catalog",
		"version": version,
		"name":    "Pluto TV Catalog",
		"description": "Browse Pluto TV's free on-demand movies and series in Nuvio. " +
			"Playback uses an ad-free HLS timeline with Pluto's captions retimed to match it.",
		"types": []string{"movie", "series"},
		"resources": []map[string]any{
			{"name": "catalog", "types": []string{"movie", "series"}},
			{"name": "meta", "types": []string{"movie", "series"}, "idPrefixes": []string{"pluto:"}},
			{"name": "stream", "types": []string{"movie", "series"}, "idPrefixes": []string{"pluto:"}},
		},
		"catalogs": catalogs,
	}
}
