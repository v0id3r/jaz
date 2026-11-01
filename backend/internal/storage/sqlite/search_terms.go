package sqlite

import (
	"database/sql/driver"
	"unicode"

	"github.com/gluonfield/jazmem/pkg/terms"
	"modernc.org/sqlite"
)

// Search indexes are built with the jaz tokenizer, and search_terms remains
// for migrations 0083 and 0085 down. Either changing its output needs a
// migration that rebuilds the search indexes, since FTS5 deletes rows by
// tokenizing them again.
func init() {
	if err := sqlite.RegisterFTS5Tokenizer("jaz", func([]string) (sqlite.FTS5Tokenizer, error) {
		return searchTokenizer{}, nil
	}); err != nil {
		panic(err)
	}
	if err := sqlite.RegisterDeterministicScalarFunction("search_terms", 1, searchTerms); err != nil {
		panic(err)
	}
}

func searchTerms(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	text, _ := args[0].(string)
	return terms.Text(text), nil
}

// searchTokenizer emits each word folded by terms.Fold and, at the same
// position, its terms.Term stem. Prefix queries match the typed form only.
type searchTokenizer struct{}

func (searchTokenizer) Tokenize(text string, flags int, emit func(token string, tflags, start, end int) error) error {
	start := -1
	for i, r := range text + " " {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start < 0 {
			continue
		}
		word := text[start:i]
		folded := terms.Fold(word)
		if err := emit(folded, 0, start, i); err != nil {
			return err
		}
		if stem := terms.Term(word); flags&sqlite.FTS5TokenizePrefix == 0 && stem != folded {
			if err := emit(stem, sqlite.FTS5TokenColocated, start, i); err != nil {
				return err
			}
		}
		start = -1
	}
	return nil
}
