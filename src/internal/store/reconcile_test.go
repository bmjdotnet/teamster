package store_test

import (
	"reflect"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

func TestUniqueIDChunks(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		size int
		want [][]string
	}{
		{"nil", nil, 3, nil},
		{"only empties", []string{"", ""}, 3, nil},
		{"dedupe keeps first-seen order", []string{"b", "a", "b", "", "c", "a"}, 10, [][]string{{"b", "a", "c"}}},
		{"exact multiple", []string{"a", "b", "c", "d"}, 2, [][]string{{"a", "b"}, {"c", "d"}}},
		{"remainder chunk", []string{"a", "b", "c"}, 2, [][]string{{"a", "b"}, {"c"}}},
		{"non-positive size degrades to 1", []string{"a", "b"}, 0, [][]string{{"a"}, {"b"}}},
	}
	for _, c := range cases {
		if got := store.UniqueIDChunks(c.ids, c.size); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
