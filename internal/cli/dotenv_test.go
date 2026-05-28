package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotenvBasic(t *testing.T) {
	in := strings.NewReader("FOO=bar\nBAZ=qux\n")
	got, err := parseDotenv(in)
	if err != nil {
		t.Fatalf("parseDotenv() error = %v", err)
	}
	if got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Fatalf("parseDotenv() = %v", got)
	}
}

func TestParseDotenvCommentsBlankLinesAndExport(t *testing.T) {
	in := strings.NewReader("# a comment\n\n   \nexport FOO=bar\n  # indented comment\nBAZ=qux\n")
	got, err := parseDotenv(in)
	if err != nil {
		t.Fatalf("parseDotenv() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parseDotenv() len = %d, want 2: %v", len(got), got)
	}
	if got["FOO"] != "bar" {
		t.Fatalf("export prefix not stripped: %v", got)
	}
	if got["BAZ"] != "qux" {
		t.Fatalf("parseDotenv() BAZ = %q", got["BAZ"])
	}
}

func TestParseDotenvQuotes(t *testing.T) {
	in := strings.NewReader(`A="double quoted"
B='single quoted'
C=unquoted value
D=""
E='# not a comment'
`)
	got, err := parseDotenv(in)
	if err != nil {
		t.Fatalf("parseDotenv() error = %v", err)
	}
	if got["A"] != "double quoted" {
		t.Fatalf("A = %q", got["A"])
	}
	if got["B"] != "single quoted" {
		t.Fatalf("B = %q", got["B"])
	}
	if got["C"] != "unquoted value" {
		t.Fatalf("C = %q", got["C"])
	}
	if got["D"] != "" {
		t.Fatalf("D = %q, want empty", got["D"])
	}
	if got["E"] != "# not a comment" {
		t.Fatalf("E = %q", got["E"])
	}
}

func TestParseDotenvTrimsUnquotedValueAndKey(t *testing.T) {
	in := strings.NewReader("  FOO  =  bar  \n")
	got, err := parseDotenv(in)
	if err != nil {
		t.Fatalf("parseDotenv() error = %v", err)
	}
	if got["FOO"] != "bar" {
		t.Fatalf("FOO = %q, want trimmed 'bar'", got["FOO"])
	}
}

func TestParseDotenvSkipsLinesWithoutEquals(t *testing.T) {
	in := strings.NewReader("NOTAVAR\nFOO=bar\n")
	got, err := parseDotenv(in)
	if err != nil {
		t.Fatalf("parseDotenv() error = %v", err)
	}
	if _, ok := got["NOTAVAR"]; ok {
		t.Fatalf("parseDotenv() kept malformed line: %v", got)
	}
	if got["FOO"] != "bar" {
		t.Fatalf("FOO = %q", got["FOO"])
	}
}

func TestParseDotenvSkipsEmptyKey(t *testing.T) {
	in := strings.NewReader("=value\nFOO=bar\n")
	got, err := parseDotenv(in)
	if err != nil {
		t.Fatalf("parseDotenv() error = %v", err)
	}
	if _, ok := got[""]; ok {
		t.Fatalf("parseDotenv() kept empty key: %v", got)
	}
	if got["FOO"] != "bar" {
		t.Fatalf("FOO = %q", got["FOO"])
	}
}

func TestLoadDotenvFileMissingIsNotError(t *testing.T) {
	dir := t.TempDir()
	got, err := loadDotenvFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("loadDotenvFile() missing error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("loadDotenvFile() missing = %v, want empty", got)
	}
}

func TestLoadDotenvFileReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("FOO=bar\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	got, err := loadDotenvFile(path)
	if err != nil {
		t.Fatalf("loadDotenvFile() error = %v", err)
	}
	if got["FOO"] != "bar" {
		t.Fatalf("loadDotenvFile() = %v", got)
	}
}

func TestMergeDotenvProcessEnvWins(t *testing.T) {
	base := []string{"FOO=process", "OTHER=keep"}
	dotenv := map[string]string{"FOO": "dotenv", "NEW": "fromenv"}
	merged := mergeDotenvEnv(base, dotenv)

	if !envHas(merged, "FOO=process") {
		t.Fatalf("process env should win for FOO; got %v", merged)
	}
	if envHas(merged, "FOO=dotenv") {
		t.Fatalf("dotenv overrode process env for FOO; got %v", merged)
	}
	if !envHas(merged, "OTHER=keep") {
		t.Fatalf("merged dropped existing var; got %v", merged)
	}
	if !envHas(merged, "NEW=fromenv") {
		t.Fatalf("merged missing new dotenv var; got %v", merged)
	}
}
