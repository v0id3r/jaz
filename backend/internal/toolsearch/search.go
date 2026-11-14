// Package toolsearch ranks deferred tool metadata with BM25 over the tool
// name, description and input schema text.
package toolsearch

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/gluonfield/jazmem/pkg/terms"
	unidecode "github.com/mozillazg/go-unidecode"
	"github.com/rivo/uniseg"
)

var stopwords = newStopwords()

// Entry is the searchable text statistics of one tool.
type Entry struct {
	name       string
	termCounts map[string]int
	termTotal  int
}

// NewEntry indexes a tool name plus free-text parts such as its description
// and schema text. The name is indexed verbatim and with underscores
// expanded to spaces.
func NewEntry(name string, parts ...string) Entry {
	texts := append([]string{name, strings.ReplaceAll(name, "_", " ")}, parts...)
	tokens := Tokenize(strings.Join(texts, " "))
	return Entry{name: name, termCounts: termCounts(tokens), termTotal: len(tokens)}
}

func (e Entry) Name() string { return e.name }

// Scored is an entry together with its BM25 score for one query.
type Scored struct {
	Entry Entry
	Score float64
}

// Corpus ranks a set of entries with BM25.
type Corpus struct {
	entries      []Entry
	averageTerms float64
}

func (c *Corpus) Add(entries ...Entry) {
	for _, entry := range entries {
		c.entries = append(c.entries, entry)
		c.averageTerms += float64(entry.termTotal)
	}
	if len(c.entries) > 0 {
		c.averageTerms /= float64(len(c.entries))
	}
}

// Score ranks every entry sharing at least one query term, best first,
// ties broken by tool name.
func (c *Corpus) Score(query string) []Scored {
	queryTerms := Tokenize(query)
	if len(queryTerms) == 0 || len(c.entries) == 0 {
		return nil
	}
	documentFrequency := map[string]int{}
	for _, term := range uniqueTerms(queryTerms) {
		for _, entry := range c.entries {
			if entry.termCounts[term] > 0 {
				documentFrequency[term]++
			}
		}
	}
	scored := make([]Scored, 0, len(c.entries))
	for _, entry := range c.entries {
		if score := bm25(entry, queryTerms, documentFrequency, c.averageTerms, len(c.entries)); score > 0 {
			scored = append(scored, Scored{Entry: entry, Score: score})
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score == scored[j].Score {
			return scored[i].Entry.name < scored[j].Entry.name
		}
		return scored[i].Score > scored[j].Score
	})
	return scored
}

func bm25(entry Entry, queryTerms []string, documentFrequency map[string]int, averageTerms float64, documents int) float64 {
	const (
		k1 = 1.2
		b  = 0.75
	)
	if entry.termTotal == 0 || averageTerms == 0 {
		return 0
	}
	n := float64(documents)
	lengthFactor := k1 * (1 - b + b*float64(entry.termTotal)/averageTerms)
	var score float64
	for _, term := range queryTerms {
		tf := float64(entry.termCounts[term])
		if tf == 0 {
			continue
		}
		df := float64(documentFrequency[term])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		score += idf * (tf * (k1 + 1)) / (tf + lengthFactor)
	}
	return score
}

// Tokenize lowercases and transliterates text, splits it into words, drops
// stopwords and stems the remaining terms.
func Tokenize(text string) []string {
	text = strings.ToLower(unidecode.Unidecode(text))
	tokens := []string{}
	for state := -1; text != ""; {
		word, rest, newState := uniseg.FirstWordInString(text, state)
		state = newState
		if rest == text {
			break
		}
		text = rest
		if !wordToken(word) || stopwords[word] {
			continue
		}
		tokens = append(tokens, terms.Term(word))
	}
	return tokens
}

// AppendSchemaText appends the searchable text of a JSON schema: property
// names, descriptions, items and anyOf branches.
func AppendSchemaText(value any, parts *[]string) {
	switch schema := value.(type) {
	case map[string]any:
		if description, ok := schema["description"].(string); ok {
			*parts = append(*parts, description)
		}
		if properties, ok := schema["properties"].(map[string]any); ok {
			for name, child := range properties {
				*parts = append(*parts, name)
				AppendSchemaText(child, parts)
			}
		}
		AppendSchemaText(schema["items"], parts)
		AppendSchemaText(schema["anyOf"], parts)
	case []any:
		for _, child := range schema {
			AppendSchemaText(child, parts)
		}
	}
}

func wordToken(token string) bool {
	for _, r := range token {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func termCounts(tokens []string) map[string]int {
	counts := map[string]int{}
	for _, token := range tokens {
		counts[token]++
	}
	return counts
}

func uniqueTerms(tokens []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

func newStopwords() map[string]bool {
	words := strings.Fields(`i me my myself we our ours ourselves you you're you've you'll you'd your yours yourself yourselves he him his himself she she's her hers herself it it's its itself they them their theirs themselves what which who whom this that that'll these those am is are was were be been being have has had having do does did doing a an the and but if or because as until while of at by for with about against between into through during before after above below to from up down in out on off over under again further then once here there when where why how all any both each few more most other some such no nor not only own same so than too very s t can will just don don't should should've now d ll m o re ve y ain aren aren't couldn couldn't didn didn't doesn doesn't hadn hadn't hasn hasn't haven haven't isn isn't ma mightn mightn't mustn mustn't needn needn't shan shan't shouldn shouldn't wasn wasn't weren weren't won won't wouldn wouldn't`)
	stopwords := make(map[string]bool, len(words))
	for _, word := range words {
		stopwords[word] = true
	}
	return stopwords
}
