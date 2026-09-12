package main

// SWT-40 Part D review fix 2: `opsctl capture-rules gate [--dry-run] [--shadow]`.
// --shadow reads shadow holds, which the gate never resolves, so it is a
// dry-run-only flag; refused otherwise rather than silently ignored.

import "testing"

func TestParseCaptureRulesGate(t *testing.T) {
	cases := []struct {
		argv    []string
		want    gateOpts
		wantErr bool
	}{
		{nil, gateOpts{}, false},
		{[]string{"--dry-run"}, gateOpts{dryRun: true}, false},
		{[]string{"--dry-run", "--shadow"}, gateOpts{dryRun: true, shadow: true}, false},
		{[]string{"--dry-run", "--limit", "7"}, gateOpts{dryRun: true, limit: 7}, false},
		{[]string{"--shadow"}, gateOpts{}, true},
		{[]string{"--bogus"}, gateOpts{}, true},
	}
	for _, c := range cases {
		got, err := parseCaptureRulesGate(c.argv)
		if (err != nil) != c.wantErr {
			t.Errorf("parseCaptureRulesGate(%v) err = %v, wantErr %v", c.argv, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseCaptureRulesGate(%v) = %+v, want %+v", c.argv, got, c.want)
		}
	}
}
