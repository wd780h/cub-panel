package panel

import "testing"

func TestParseMoney(t *testing.T) {
	good := map[string]int64{
		"10":     1000,
		"10.5":   1050,
		"10.50":  1050,
		"0.05":   5,
		"-3":     -300,
		"-0.01":  -1,
		" 12.34": 1234,
	}
	for in, want := range good {
		got, err := parseMoney(in)
		if err != nil || got != want {
			t.Errorf("parseMoney(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "abc", "1.234", "1,5", "1e3"} {
		if _, err := parseMoney(in); err == nil {
			t.Errorf("parseMoney(%q) should fail", in)
		}
	}
}

func TestFmtMoney(t *testing.T) {
	cases := map[int64]string{0: "0.00", 5: "0.05", 1050: "10.50", -1234: "-12.34"}
	for in, want := range cases {
		if got := fmtMoney(in); got != want {
			t.Errorf("fmtMoney(%d) = %s, want %s", in, got, want)
		}
	}
}
