package sqlite

import (
	"database/sql/driver"

	"github.com/gluonfield/jazmem/pkg/terms"
	"modernc.org/sqlite"
)

// Search index triggers delete rows by recomputing search_terms, so a change in
// its output needs a migration that rebuilds the search indexes.
func init() {
	if err := sqlite.RegisterDeterministicScalarFunction("search_terms", 1, searchTerms); err != nil {
		panic(err)
	}
}

func searchTerms(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	text, _ := args[0].(string)
	return terms.Text(text), nil
}
