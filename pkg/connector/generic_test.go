package connector

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repository is public. Every example, test fixture, ADR and config comment
// has to use placeholders — example.com, +15551234567, 192.0.2.x from RFC 5737
// — and never the values of the deployment this bridge was written for.
//
// The guard is repo-wide rather than aimed at the example config, because the
// place a real number actually ends up is a worked example in a README or an
// ADR, written in a hurry to explain something else.
var forbiddenSubstrings = []string{
	"lhns.de",
	"xmpp.lhns.de",
	"rfn.de",
	"eventphone",
	"02041976840",
	"4915112",
	"10.1.",
	"10.199.",
	"62.3.50",
}

// forbiddenNumbers are short enough to occur by accident inside an unrelated
// digit string, so they are matched as whole tokens: a hit only counts if it is
// not part of a longer run of digits. Matched as substrings they would fire on
// any example number that happened to contain them, and a guard that cries wolf
// gets deleted.
var forbiddenNumbers = []string{"4812", "4813"}

// scannedExtensions are the text files worth reading. Everything else in the
// tree is either generated, binary, or the licence.
var scannedExtensions = map[string]bool{
	".go": true, ".yaml": true, ".yml": true, ".md": true, ".sql": true,
}

var scannedNames = map[string]bool{"Dockerfile": true, "Makefile": true}

func TestRepositoryHasNoSiteSpecificValues(t *testing.T) {
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		// LICENSE and NOTICE are where the copyright holder belongs.
		if name == "LICENSE" || name == "NOTICE" {
			return nil
		}
		if !scannedNames[name] && !scannedExtensions[filepath.Ext(name)] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		checkContent(t, rel, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}

func checkContent(t *testing.T, name, content string) {
	t.Helper()
	// This file lists the forbidden values in order to look for them.
	if name == filepath.Join("pkg", "connector", "generic_test.go") {
		return
	}
	for _, forbidden := range forbiddenSubstrings {
		if strings.Contains(content, forbidden) {
			t.Errorf("%s leaks a site-specific value: %q", name, forbidden)
		}
	}
	for _, number := range forbiddenNumbers {
		if containsNumberToken(content, number) {
			t.Errorf("%s leaks a site-specific number: %q", name, number)
		}
	}
}

// containsNumberToken reports whether needle appears in content without a digit
// on either side of it.
func containsNumberToken(content, needle string) bool {
	for i := 0; i+len(needle) <= len(content); i++ {
		if content[i:i+len(needle)] != needle {
			continue
		}
		if i > 0 && isDigit(content[i-1]) {
			continue
		}
		if end := i + len(needle); end < len(content) && isDigit(content[end]) {
			continue
		}
		return true
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}

// The example config is shipped as the base for config upgrades, so it must
// also positively use the placeholder domain rather than merely avoid the real
// one.
func TestExampleConfigUsesPlaceholders(t *testing.T) {
	if !strings.Contains(ExampleConfig, "example.com") {
		t.Error("example config should use example.com hostnames")
	}
}
