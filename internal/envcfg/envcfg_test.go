package envcfg

import "testing"

func TestClean(t *testing.T) {
	cases := map[string]string{
		`myserver.local:43219`:   `myserver.local:43219`,
		`"myserver.local:43219"`: `myserver.local:43219`,
		`'myserver.local:43219'`: `myserver.local:43219`,
		`  "padded"  `:           `padded`,
		`""`:                     ``,
		`"unbalanced`:            `"unbalanced`,
		`""nested""`:             `nested`,
		``:                       ``,
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}
