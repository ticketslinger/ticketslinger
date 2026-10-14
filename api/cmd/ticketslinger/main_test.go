package main

import "testing"

func TestVersionIsSet(t *testing.T) {
	if version == "" {
		t.Fatal("version must default to a non-empty string")
	}
}
