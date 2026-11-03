package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{nil, 2, "", "Usage:"},
		{[]string{"version"}, 0, "protocol 1", ""},
		{[]string{"help"}, 0, "Commands:", ""},
		{[]string{"run"}, 1, "", "not implemented"},
		{[]string{"bogus"}, 2, "", "unknown command"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if got := Run(c.args, &out, &errb); got != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, got, c.code)
		}
		if !strings.Contains(out.String(), c.stdout) || !strings.Contains(errb.String(), c.stderr) {
			t.Errorf("%v: unexpected output stdout=%q stderr=%q", c.args, out.String(), errb.String())
		}
	}
}
