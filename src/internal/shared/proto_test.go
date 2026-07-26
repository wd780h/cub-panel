package shared

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const secret = "0123456789abcdef0123456789abcdef"

// signed builds a request carrying a valid signature.
func signed(t *testing.T, method, path string, body []byte, ts int64, nonce string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	tss := strconv.FormatInt(ts, 10)
	r.Header.Set(HeaderTimestamp, tss)
	r.Header.Set(HeaderNonce, nonce)
	r.Header.Set(HeaderSignature, Sign(secret, method, path, tss, nonce, body))
	return r
}

func TestVerifyAcceptsValidSignature(t *testing.T) {
	body := []byte(`{"name":"hm-test"}`)
	r := signed(t, "POST", "/v1/instances", body, time.Now().Unix(), "n1")
	if err := Verify(secret, r, body); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	body := []byte(`{"cpu":1}`)
	r := signed(t, "POST", "/v1/instances", body, time.Now().Unix(), "n2")
	if err := Verify(secret, r, []byte(`{"cpu":64}`)); err == nil {
		t.Fatal("a tampered body was accepted")
	}
}

func TestVerifyRejectsTamperedPath(t *testing.T) {
	body := []byte(`{}`)
	r := signed(t, "DELETE", "/v1/instances/mine", body, time.Now().Unix(), "n3")
	r.URL.Path = "/v1/instances/yours"
	if err := Verify(secret, r, body); err == nil {
		t.Fatal("a tampered path was accepted")
	}
}

func TestVerifyRejectsTamperedMethod(t *testing.T) {
	body := []byte(`{}`)
	r := signed(t, "GET", "/v1/instances/x/state", body, time.Now().Unix(), "n4")
	r.Method = "DELETE"
	if err := Verify(secret, r, body); err == nil {
		t.Fatal("a tampered method was accepted")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	body := []byte(`{}`)
	r := signed(t, "GET", "/v1/health", body, time.Now().Unix(), "n5")
	if err := Verify("a-completely-different-secret-value", r, body); err == nil {
		t.Fatal("a signature under the wrong secret was accepted")
	}
}

func TestVerifyRejectsClockSkew(t *testing.T) {
	body := []byte(`{}`)
	for _, off := range []time.Duration{-2 * MaxClockSkew, 2 * MaxClockSkew} {
		r := signed(t, "GET", "/v1/health", body, time.Now().Add(off).Unix(), "n6")
		if err := Verify(secret, r, body); err == nil {
			t.Errorf("offset %v was accepted", off)
		}
	}
}

func TestVerifyRejectsMissingHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/health", nil)
	if err := Verify(secret, r, nil); err == nil {
		t.Fatal("an unsigned request was accepted")
	}
}

func TestSignIsDeterministicAndDistinct(t *testing.T) {
	a := Sign(secret, "GET", "/v1/health", "100", "n", nil)
	if b := Sign(secret, "GET", "/v1/health", "100", "n", nil); a != b {
		t.Error("Sign is not deterministic")
	}
	// Changing any single input must change the signature.
	for name, got := range map[string]string{
		"method": Sign(secret, "POST", "/v1/health", "100", "n", nil),
		"path":   Sign(secret, "GET", "/v1/other", "100", "n", nil),
		"ts":     Sign(secret, "GET", "/v1/health", "101", "n", nil),
		"nonce":  Sign(secret, "GET", "/v1/health", "100", "m", nil),
		"body":   Sign(secret, "GET", "/v1/health", "100", "n", []byte("x")),
	} {
		if got == a {
			t.Errorf("changing %s did not change the signature", name)
		}
	}
}
