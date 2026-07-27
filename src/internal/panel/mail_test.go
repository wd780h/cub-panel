package panel

import "testing"

func TestRandomDigits(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, err := randomDigits(6)
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 6 {
			t.Fatalf("len=%d s=%q", len(s), s)
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				t.Fatalf("non-digit in %q", s)
			}
		}
	}
}

func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"ab@ex.com":      "a***@ex.com",
		"alice@ex.com":   "a***e@ex.com",
		"a@ex.com":       "a@ex.com",
		"user.name@x.co": "u***e@x.co",
	}
	for in, want := range cases {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEncodeSubject(t *testing.T) {
	if got := encodeSubject("hello"); got != "hello" {
		t.Fatalf("ascii: %q", got)
	}
	got := encodeSubject("注册验证码")
	if got == "注册验证码" || got[:10] != "=?UTF-8?B?" {
		t.Fatalf("non-ascii encoding: %q", got)
	}
}
