package app

import (
	"net/http/httptest"
	"testing"
)

func TestPageBoundsAndLegacy(t *testing.T) {
	for _, tc := range []struct {
		q             string
		page, size    int
		explicit, bad bool
	}{{"", 1, 100, false, false}, {"?page=2", 2, 20, true, false}, {"?page_size=50", 1, 50, true, false}, {"?page_size=100", 1, 100, true, false}, {"?page=0", 0, 0, true, true}, {"?page_size=101", 0, 0, true, true}, {"?page=x", 0, 0, true, true}} {
		p, s, x, e := pageRequest(httptest.NewRequest("GET", "/"+tc.q, nil))
		if (e != nil) != tc.bad {
			t.Fatal(tc, e)
		}
		if !tc.bad && (p != tc.page || s != tc.size || x != tc.explicit) {
			t.Fatal(tc, p, s, x)
		}
	}
}
