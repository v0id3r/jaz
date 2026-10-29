package usage

import (
	"encoding/binary"
	"testing"
	"time"
)

// Slim tzdata lists no transitions past the point a zone's POSIX rule takes
// over, which sends every later date through Go's rule-based zone bounds.
func TestDayBoundaryInRuleOnlyZones(t *testing.T) {
	for _, test := range []struct {
		abbr, rule string
		offset     int32
		date, want string
	}{
		{"EST", "EST5EDT,M3.2.0,M11.1.0", -5 * 3600, "2024-12-31", "2024-12-31T05:00:00Z"},
		{"EST", "EST5EDT,M3.2.0,M11.1.0", -5 * 3600, "2025-01-01", "2025-01-01T05:00:00Z"},
		{"EST", "EST5EDT,M3.2.0,M11.1.0", -5 * 3600, "2024-03-11", "2024-03-11T04:00:00Z"},
		{"CET", "CET-1CEST,M3.5.0,M10.5.0/3", 3600, "2024-12-31", "2024-12-30T23:00:00Z"},
		{"CET", "CET-1CEST,M3.5.0,M10.5.0/3", 3600, "2025-01-01", "2024-12-31T23:00:00Z"},
		{"CET", "CET-1CEST,M3.5.0,M10.5.0/3", 3600, "2025-01-02", "2025-01-01T23:00:00Z"},
	} {
		block := []byte("TZif2\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
		for _, count := range []uint32{0, 0, 0, 0, 1, 4} {
			block = binary.BigEndian.AppendUint32(block, count)
		}
		block = append(append(binary.BigEndian.AppendUint32(block, uint32(test.offset)), 0, 0), test.abbr+"\x00"...)
		loc, err := time.LoadLocationFromTZData(test.rule, append(append(block, block...), "\n"+test.rule+"\n"...))
		if err != nil {
			t.Fatal(err)
		}
		date, _ := time.Parse(DateLayout, test.date)
		got := make(chan time.Time, 1)
		go func() { got <- dayBoundary(date, loc) }()
		select {
		case boundary := <-got:
			if boundary.UTC().Format(time.RFC3339) != test.want {
				t.Errorf("%s dayBoundary(%s) = %s, want %s", test.abbr, test.date, boundary.UTC().Format(time.RFC3339), test.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s dayBoundary(%s) did not return", test.abbr, test.date)
		}
	}
}
