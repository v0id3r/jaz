package toolsearch

import (
	"encoding/json"
	"testing"
)

func TestScoreRanksByTermFrequencyAndName(t *testing.T) {
	corpus := &Corpus{}
	corpus.Add(
		NewEntry("zeta_browse", "Browse the archive of records."),
		NewEntry("alpha_archive", "Archive records and files daily."),
		NewEntry("mid_records_hub", "records records records vault."),
	)
	ranked := corpus.Score("records")
	if len(ranked) != 3 {
		t.Fatalf("ranked = %d, want every record tool", len(ranked))
	}
	want := []string{"mid_records_hub", "zeta_browse", "alpha_archive"}
	for i, name := range want {
		if ranked[i].Entry.Name() != name {
			t.Fatalf("ranked[%d] = %q, want %q (order %v)", i, ranked[i].Entry.Name(), name, names(ranked))
		}
	}
	if ranked[0].Score <= ranked[1].Score || ranked[1].Score <= ranked[2].Score {
		t.Fatalf("scores not descending: %v", ranked)
	}
}

func TestScorePrefersRareTerms(t *testing.T) {
	corpus := &Corpus{}
	corpus.Add(
		NewEntry("common_one", "Shared team workspace."),
		NewEntry("common_two", "Shared team workspace."),
		NewEntry("rare_one", "Shared vault."),
	)
	ranked := corpus.Score("shared vault")
	if len(ranked) != 3 || ranked[0].Entry.Name() != "rare_one" {
		t.Fatalf("rare term should rank first: %v", names(ranked))
	}
}

func TestScoreMatchesSchemaTerms(t *testing.T) {
	var schema any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"attendee":{"type":"string","description":"Event attendee email."}},"required":["attendee"]}`), &schema); err != nil {
		t.Fatal(err)
	}
	parts := []string{"Create an event."}
	AppendSchemaText(schema, &parts)
	corpus := &Corpus{}
	corpus.Add(NewEntry("calendar_create", parts...))
	for _, query := range []string{"attendee", "email", "events"} {
		if ranked := corpus.Score(query); len(ranked) != 1 || ranked[0].Entry.Name() != "calendar_create" {
			t.Fatalf("query %q ranked %v, want calendar_create", query, names(corpus.Score(query)))
		}
	}
}

func TestScoreIndexesNameUnderscores(t *testing.T) {
	corpus := &Corpus{}
	corpus.Add(NewEntry("memory_page_get"))
	if ranked := corpus.Score("memory page"); len(ranked) != 1 || ranked[0].Entry.Name() != "memory_page_get" {
		t.Fatalf("name terms not indexed: %v", names(ranked))
	}
}

func TestScoreSkipsEmptyQueries(t *testing.T) {
	corpus := &Corpus{}
	corpus.Add(NewEntry("memory_page_get", "Read pages."))
	for _, query := range []string{"", "   ", "the and of"} {
		if ranked := corpus.Score(query); len(ranked) != 0 {
			t.Fatalf("query %q ranked %v, want none", query, names(ranked))
		}
	}
}

func names(ranked []Scored) []string {
	out := make([]string, 0, len(ranked))
	for _, scored := range ranked {
		out = append(out, scored.Entry.Name())
	}
	return out
}
