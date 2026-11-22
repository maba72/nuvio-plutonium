package pluto

import "testing"

func TestSearchItemsRanksAndDeduplicates(t *testing.T) {
	data := map[string]any{
		"categories": []any{
			map[string]any{
				"name": "Movies",
				"items": []any{
					map[string]any{"_id": "1", "type": "movie", "name": "Space Odyssey", "genre": "Sci-Fi", "summary": "A journey"},
					map[string]any{"_id": "2", "type": "movie", "name": "Space Cowboys", "genre": "Adventure", "summary": "Space adventure"},
					map[string]any{"_id": "3", "type": "movie", "name": "The Incredibles", "genre": "Action", "summary": "A family of space heroes"},
					// Duplicate of item 1, as Pluto categories often repeat items.
					map[string]any{"_id": "1", "type": "movie", "name": "Space Odyssey", "genre": "Sci-Fi", "summary": "A journey"},
					map[string]any{"_id": "4", "type": "series", "name": "Unrelated", "genre": "Drama", "summary": "Nothing"},
				},
			},
		},
	}

	results := SearchItems(data, "space", 10)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(results), results)
	}
	// Title-prefix matches rank ahead of a description-only match.
	if results[0]["name"] != "Space Odyssey" && results[0]["name"] != "Space Cowboys" {
		t.Fatalf("unexpected top result: %v", results[0]["name"])
	}
	if results[2]["name"] != "The Incredibles" {
		t.Fatalf("description match should be last, got %v", results[2]["name"])
	}
	for _, result := range results {
		if result["id"] == "pluto:series:4" {
			t.Fatalf("unrelated item should not match")
		}
	}
}

func TestSearchItemsEmptyQuery(t *testing.T) {
	if results := SearchItems(map[string]any{}, "   ", 10); len(results) != 0 {
		t.Fatalf("empty query should return no results, got %+v", results)
	}
}

func TestSearchItemsLimit(t *testing.T) {
	data := map[string]any{
		"categories": []any{map[string]any{"items": []any{
			map[string]any{"_id": "1", "type": "movie", "name": "Alpha"},
			map[string]any{"_id": "2", "type": "movie", "name": "Alpine"},
			map[string]any{"_id": "3", "type": "movie", "name": "Also"},
		}}},
	}
	if results := SearchItems(data, "al", 2); len(results) != 2 {
		t.Fatalf("got %d results, want limit of 2", len(results))
	}
}
