package main

import (
	"os"
	"testing"
)

func TestParseSocketMode(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    os.FileMode
		wantErr bool
	}{
		{name: "default all users", value: "0666", want: 0666},
		{name: "owner only", value: "0600", want: 0600},
		{name: "invalid digit", value: "0688", wantErr: true},
		{name: "too many permission bits", value: "1777", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseSocketMode(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseSocketMode(%q) error = %v, wantErr %v", test.value, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("parseSocketMode(%q) = %#o, want %#o", test.value, got, test.want)
			}
		})
	}
}
