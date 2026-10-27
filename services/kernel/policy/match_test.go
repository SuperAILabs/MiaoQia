package policy

import "testing"

func TestRegionWithinAndCategoryCovered(t *testing.T) {
	for _, c := range []struct {
		loc     string
		regions []string
		want    bool
	}{
		{"330106", []string{"330000"}, true},
		{"330106", []string{"330100"}, true},
		{"330106", []string{"330106"}, true},
		{"330106", []string{"000000"}, true},
		{"330206", []string{"330100"}, false},
		{"110105", []string{"330000"}, false},
		{"", []string{"000000"}, false},
		{"330106", nil, false},
	} {
		if got := RegionWithin(c.loc, c.regions); got != c.want {
			t.Errorf("RegionWithin(%q, %v) = %v", c.loc, c.regions, got)
		}
	}
	for _, c := range []struct {
		code string
		set  []string
		want bool
	}{
		{"CAT-PIPE-PE", []string{"CAT-PIPE"}, true},
		{"CAT-PIPE-PE", []string{"CAT-PIPE-PE"}, true},
		{"CAT-PIPE", []string{"CAT-PIPE-PE"}, false},
		{"CAT-PIPE-PEX", []string{"CAT-PIPE-PE"}, false},
		{"", []string{"CAT"}, false},
	} {
		if got := CategoryCovered(c.code, c.set); got != c.want {
			t.Errorf("CategoryCovered(%q, %v) = %v", c.code, c.set, got)
		}
	}
}
