package sqlite

import (
	"database/sql/driver"

	"github.com/gluonfield/jazmem/pkg/terms"
	"modernc.org/sqlite"
)

// Search indexes are built with the jaz tokenizer, and search_terms remains
// for migrations 0083 and 0085 down.
func init() {
	if err := sqlite.RegisterFTS5Tokenizer("jaz", func([]string) (sqlite.FTS5Tokenizer, error) {
		return terms.FTS5Tokenizer{}, nil
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
