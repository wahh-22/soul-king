package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"testing"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want options
	}{
		{"no flags plays for real", nil, options{}},
		{"demo mode", []string{"--demo"}, options{demo: true}},
		{"version flag", []string{"--version"}, options{version: true}},
		{"single-dash version", []string{"-version"}, options{version: true}},
		{"calm starts the effects off", []string{"--demo", "--calm"}, options{demo: true, calm: true}},
		{"local files only", []string{"--local"}, options{local: true}},
		{"apple music login", []string{"--apple-music-login"}, options{login: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFlags(tt.args, io.Discard)
			if err != nil || got != tt.want {
				t.Fatalf("parseFlags(%q) = %+v, %v; want %+v", tt.args, got, err, tt.want)
			}
		})
	}
}

func TestParseFlagsRejectsUnknownFlags(t *testing.T) {
	var usage bytes.Buffer
	if _, err := parseFlags([]string{"--nope"}, &usage); err == nil {
		t.Fatal("parseFlags(--nope) succeeded; want error")
	}
	if usage.Len() == 0 {
		t.Fatal("no usage printed for an unknown flag")
	}
}

func TestParseFlagsHelp(t *testing.T) {
	if _, err := parseFlags([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(-h) error = %v; want flag.ErrHelp", err)
	}
}

// version is overridden at link time (-ldflags "-X main.version=x.y.z");
// unstamped builds must still say something sensible.
func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("version = %q; want %q for an unstamped test build", version, "dev")
	}
}

// Off a terminal (a pipe, a file, release.sh's check) --version prints
// the bare version line, byte for byte as before the emblem.
func TestPrintVersion(t *testing.T) {
	var out bytes.Buffer
	printVersion(&out, false)
	if got, want := out.String(), version+"\n"; got != want {
		t.Fatalf("printVersion wrote %q; want %q", got, want)
	}
}

// On a terminal it prints the Braille logo and the version under the
// bars, in the wordmark's column (the first after the blank column
// parting it from the head).
func TestPrintVersionOnATerminal(t *testing.T) {
	art := func(_, ver string) string {
		return "   ⢀⣠⡴⠶⠒⠒⠶⢦⣄⡀\n" +
			"  ⣴⢟⡥⠚⠉⠉⠉⠉⠓⢬⡻⣦\n" +
			" ⣼⢣⠎        ⠱⡜⣧\n" +
			"⣿⣿⣼⢀        ⡀⣧⣿⣿ ⣿⣆⢿⢸⡇⢸⡇⠴⣿ ⠠⢾⡇ ⢾⣉⡉⠈⢹⡏⠁⣾⢉⣉⢸⣷⡸⡇⣾⠉⣿⢸⡇\n" +
			"⣿⣿⣿⢐⠨⠨⢐  ⡂⠅⠅⡂⣿⣿⣿ ⣿⠘⣿⠘⢧⣸⡇⣀⣿⣀⢀⣸⣇⡀⣀⣀⡿⢀⣸⣇⡀⢿⣀⣿⢸⡇⢻⡇⣿⠉⣿⠸⣇⣀⡀\n" +
			"⠿⠿⢿⠐⠨⠨⢐  ⡂⠅⠅⠂⡿⠿⠿             ⣠⡶⢂⣴⠖⣠⡶⢂⣴⠖⣠⡶⠂\n" +
			"   ⠳⡄      ⢠⠞               ⠚⠋⠐⠛⠁⠚⠋⠐⠛⠁⠚⠋\n" +
			"    ⠘⢆⣀⣀⣀⣀⡰⠃     " + ver + "\n"
	}
	for _, tt := range []struct{ version, want string }{
		{"dev", art("NU11SIGNAL", "dev")},
		{"0.3.0", art("NU11SIGNAL", "v0.3.0")},
	} {
		saved := version
		version = tt.version
		var out bytes.Buffer
		printVersion(&out, true)
		version = saved
		if got := out.String(); got != tt.want {
			t.Errorf("printVersion(%s) on a terminal wrote\n%s\nwant\n%s", tt.version, got, tt.want)
		}
	}
}

func TestRunVersionAsksWhetherStdoutIsATerminal(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		var out bytes.Buffer
		code := run([]string{"--version"}, deps{stdout: &out, stderr: io.Discard, stdoutTerminal: func() bool { return terminal }})
		if code != 0 {
			t.Fatalf("run(--version) = %d; want 0", code)
		}
		if got := out.String() == version+"\n"; got == terminal {
			t.Errorf("terminal %v: run(--version) wrote %q", terminal, out.String())
		}
	}
	var out bytes.Buffer
	if run([]string{"--version"}, deps{stdout: &out, stderr: io.Discard}); out.String() != version+"\n" {
		t.Errorf("without a terminal check run(--version) wrote %q; want the bare version", out.String())
	}
}

func TestWordmarkAt(t *testing.T) {
	tests := []struct {
		name      string
		rows      []string
		col, rest int
		ok        bool
	}{
		{
			name: "a free row under the wordmark",
			rows: []string{"HH WORD", "HH WORD", "HH", "HH"},
			col:  3, rest: 2, ok: true,
		},
		{
			// The wordmark reaches the last row: below is len(rows), and
			// the version needs a row of its own.
			name: "no free row under the bars",
			rows: []string{"HH", "HH WORD", "HH WORD"},
			col:  3, rest: 3, ok: true,
		},
		{
			// Nothing parts a head from a wordmark: no column to align to.
			name: "no blank column",
			rows: []string{"HEADWORD", "HEADWORD"},
			ok:   false,
		},
		{
			name: "no art at all",
			rows: nil,
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col, below, ok := wordmarkAt(tt.rows)
			if ok != tt.ok || (ok && (col != tt.col || below != tt.rest)) {
				t.Fatalf("wordmarkAt(%q) = %d, %d, %v; want %d, %d, %v", tt.rows, col, below, ok, tt.col, tt.rest, tt.ok)
			}
		})
	}
}

// The version line is never dropped: under the wordmark when the art has
// one (in a row of its own when the wordmark reaches the last row), on its
// own line under the art otherwise.
func TestLogoWithVersion(t *testing.T) {
	tests := []struct {
		name string
		rows []string
		want string
	}{
		{"in the free row", []string{"HH WORD", "HH"}, "HH WORD\nHH v1.2.3\n"},
		{"in an added row", []string{"HH", "HH WORD"}, "HH\nHH WORD\n   v1.2.3\n"},
		{"on its own line without a wordmark", []string{"HEADWORD"}, "HEADWORD\nv1.2.3\n"},
		{"alone without art", nil, "v1.2.3\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logoWithVersion(tt.rows, "v1.2.3"); got != tt.want {
				t.Fatalf("logoWithVersion(%q) = %q; want %q", tt.rows, got, tt.want)
			}
		})
	}
}
