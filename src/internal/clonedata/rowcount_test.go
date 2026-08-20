package clonedata

import (
	"reflect"
	"testing"
)

func TestParseTableList(t *testing.T) {
	out := "outcomes\nworkunits\n\ntags\n"
	got := ParseTableList(out)
	want := []string{"outcomes", "workunits", "tags"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseTableList(%q) = %v, want %v", out, got, want)
	}
}

func TestParseCount(t *testing.T) {
	n, err := ParseCount(" 42 \n")
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Errorf("ParseCount = %d, want 42", n)
	}
	if _, err := ParseCount("not a number"); err == nil {
		t.Fatal("expected an error for non-numeric input, got nil")
	}
}

// TestRowCountDiff_DynamicTableCoverage is the regression guard for I-F: a
// table present on only one side (the exact shape of "a new table the
// verification list should have caught but a hand-picked list wouldn't")
// must be flagged, not silently ignored.
func TestRowCountDiff_DynamicTableCoverage(t *testing.T) {
	source := map[string]int64{"outcomes": 10, "workunits": 20, "new_table": 5}
	target := map[string]int64{"outcomes": 10, "workunits": 19}

	got := RowCountDiff(source, target)
	want := []string{
		"new_table: source has 5 rows, target table missing",
		"workunits: source has 20 rows, target has 19",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RowCountDiff = %v, want %v", got, want)
	}
}

func TestRowCountDiff_ExactMatchIsEmpty(t *testing.T) {
	source := map[string]int64{"outcomes": 10, "workunits": 20}
	target := map[string]int64{"outcomes": 10, "workunits": 20}
	if got := RowCountDiff(source, target); len(got) != 0 {
		t.Errorf("RowCountDiff = %v, want empty", got)
	}
}

func TestRowCountDiff_TargetOnlyTableFlagged(t *testing.T) {
	source := map[string]int64{"outcomes": 10}
	target := map[string]int64{"outcomes": 10, "extra": 3}
	got := RowCountDiff(source, target)
	want := []string{"extra: target has 3 rows, source table missing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RowCountDiff = %v, want %v", got, want)
	}
}
