package main

import (
	"bytes"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestDecompressValueUsesLZ4ContentSize(t *testing.T) {
	want := bytes.Repeat([]byte("json-like repeated payload"), 4096)
	compressed, err := compressValue(want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := decompressValue(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("decompressed value does not match input")
	}
	if cap(got) != len(want) {
		t.Fatalf("decompressed capacity = %d, want exact size %d", cap(got), len(want))
	}
}

func TestDecompressValueReadsLegacyLZ4Frame(t *testing.T) {
	want := bytes.Repeat([]byte("legacy LZ4 frame"), 4096)
	var compressed bytes.Buffer
	writer := lz4.NewWriter(&compressed)
	if _, err := writer.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := decompressValue(compressed.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("decompressed legacy value does not match input")
	}
}
