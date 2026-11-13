package main

import (
	"fmt"
	"io"

	"github.com/wins/jaz/backend/internal/buildinfo"
)

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "jaz %s (commit %s)\n", buildinfo.Version(), buildinfo.Commit())
}
