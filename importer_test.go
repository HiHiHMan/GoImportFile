package main

import "testing"

func TestQuoteIdentifier(t *testing.T) {
	got := quoteIdentifier("name]with]bracket")
	want := "[name]]with]]bracket]"
	if got != want { t.Fatalf("quoteIdentifier() = %q, want %q", got, want) }
}
