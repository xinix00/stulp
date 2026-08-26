package main

import (
	"os"
	"testing"
)

func TestVersionStamp(t *testing.T) {
	// De testhook laat een gerichte test ook de linker-override bewijzen.
	want := os.Getenv("STULP_TEST_EXPECT_VERSION")
	if want == "" {
		want = "v0.8.9"
	}
	if version != want {
		t.Fatalf("version = %q, want %s", version, want)
	}
}
