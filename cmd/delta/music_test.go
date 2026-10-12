package main

import "testing"

func TestMusicArgs(t *testing.T) {
	for _, args := range [][]string{
		{"search", "x"}, {"get", "x"}, {"config"},
		{"song"}, {"album", " "}, {"re-login", "now"},
		{"song", "--format", "flac", "A - B"}, {"album", "A - B", "-n", "3"},
	} {
		if err := runMusic(args); err == nil {
			t.Errorf("runMusic(%q) accepted", args)
		}
	}
}
