package main

import (
	"os"
	"strings"
	"testing"
)

func TestVersionEmbedded(t *testing.T) {
	want, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if version != strings.TrimSpace(string(want)) || version == "" {
		t.Fatalf("embedded version %q does not match VERSION", version)
	}
}
