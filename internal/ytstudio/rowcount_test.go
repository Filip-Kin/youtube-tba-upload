package ytstudio

import "testing"

func TestVisibleRowCount(t *testing.T) {
	cases := map[string]int{
		"0 visible, 0 checked":  0,
		"12 visible, 1 checked": 12,
		"not-found":             -1,
		"":                      -1,
	}
	for in, want := range cases {
		if got := visibleRowCount(in); got != want {
			t.Errorf("visibleRowCount(%q) = %d, want %d", in, got, want)
		}
	}
}
