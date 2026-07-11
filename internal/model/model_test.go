package model

import "testing"

func TestUSDNanosRoundTrip(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"0":           "0",
		"13.44311715": "13.44311715",
		"0.042626150": "0.04262615",
		"1.000000001": "1.000000001",
	}
	for input, expected := range cases {
		nanos, err := ParseUSDNanos(input)
		if err != nil {
			t.Fatalf("ParseUSDNanos(%q): %v", input, err)
		}
		if got := FormatUSDNanos(nanos); got != expected {
			t.Errorf("round trip %q = %q, want %q", input, got, expected)
		}
	}
}

func TestDeviceNameValidation(t *testing.T) {
	t.Parallel()
	if err := ValidateDeviceName("Studio Mac mini"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDeviceName("   "); err == nil {
		t.Fatal("expected empty name rejection")
	}
}
