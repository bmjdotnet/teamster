// Package sqlite dialect gotcha: modernc.org/sqlite's automatic
// decltype-based conversion (store.go's New doc comment: "the driver
// auto-converts [DATETIME columns] to/from Go time.Time on Scan/bind") only
// fires for a PLAIN column reference, because it works by sniffing the
// declared column type from the query's result-set metadata
// (sqlite3_column_decltype), which is only populated for a bare column, not
// for the result of an aggregate or expression (MAX(...), MIN(...),
// COALESCE(...), etc — even COALESCE(MAX(a.col), b.col) over two DATETIME
// columns loses it). A SELECT ... AS a value the driver can't map to a
// decltype comes back as the on-disk text form ("_time_format=sqlite" in
// New's DSN construction: "YYYY-MM-DD HH:MM:SS.SSS...+-HH:MM") instead of a
// time.Time, and Scan into time.Time/sql.NullTime fails outright rather than
// silently degrading.
//
// aggTime is the fix: a sql.Scanner that accepts either a time.Time (in case
// the driver ever does convert) or that on-disk text form, so any query
// whose SELECT list computes a datetime via MAX/MIN/COALESCE/etc can still
// scan cleanly.
package sqlite

import (
	"fmt"
	"time"
)

// sqliteTimeLayout matches the on-disk text format New's "_time_format=sqlite"
// DSN parameter writes (sqlite.org/lang_datefunc.html format 4), which is
// what a computed/aggregated SELECT column returns as its driver.Value when
// the driver cannot sniff a DATETIME decltype for it.
const sqliteTimeLayout = "2006-01-02 15:04:05.999999999-07:00"

// aggTime scans a DATETIME-valued aggregate or expression result (MAX, MIN,
// COALESCE over DATETIME columns, ...) that the driver could not
// automatically convert to time.Time. Valid is false only when the
// underlying SQL value was NULL (e.g. MAX() over zero rows); Time is the
// UTC-normalized value otherwise.
type aggTime struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (a *aggTime) Scan(src any) error {
	if src == nil {
		a.Time, a.Valid = time.Time{}, false
		return nil
	}
	switch v := src.(type) {
	case time.Time:
		a.Time, a.Valid = v.UTC(), true
		return nil
	case string:
		t, err := time.Parse(sqliteTimeLayout, v)
		if err != nil {
			return fmt.Errorf("aggTime: parse sqlite datetime %q: %w", v, err)
		}
		a.Time, a.Valid = t.UTC(), true
		return nil
	case []byte:
		t, err := time.Parse(sqliteTimeLayout, string(v))
		if err != nil {
			return fmt.Errorf("aggTime: parse sqlite datetime %q: %w", v, err)
		}
		a.Time, a.Valid = t.UTC(), true
		return nil
	default:
		return fmt.Errorf("aggTime: unsupported source type %T", src)
	}
}
