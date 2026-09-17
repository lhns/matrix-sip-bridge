package connector

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The repository is public. Every example, test fixture, ADR and config comment
// has to use placeholders — example.com from RFC 2606, +1555…, 192.0.2.x from
// RFC 5737 — and never a value of the deployment this bridge was written for.
//
// The guard therefore matches the *shapes* those values have, with an allowlist
// of the placeholders that are legitimate. It cannot be a list of the real
// values: this file is public too, so such a list would publish exactly what it
// guards. Nothing below may name a real hostname, number or address.
//
// The guard is repo-wide rather than aimed at the example config, because the
// place a real number actually ends up is a worked example in a README or an
// ADR, written in a hurry to explain something else.
//
// A value no pattern can recognise — a bare extension, an internal host with no
// dot — comes from outside the repo; see forbiddenValuesEnv.

// allowedHostSuffixes are the hostnames that may appear: the documentation
// domains, and the infrastructure this repo genuinely refers to. A candidate
// matches if it equals an entry or is a subdomain of one.
var allowedHostSuffixes = []string{
	"example.com", "example.org", "example.net",
	"github.com", "ghcr.io", "gcr.io",
	"mau.fi", "maunium.net", "matrix.org",
}

// scannedTLDs is what makes a dotted token a hostname rather than a Go selector
// expression. TLDs that read as Go identifiers are left out — .in, .at, .be,
// .me, .to, .it, .is, .id, .md, .mx, .sh, .no, .us, and .info because of
// log.Info: they fire on ordinary code, and a guard that cries wolf gets
// deleted.
var scannedTLDs = map[string]bool{
	"com": true, "org": true, "net": true, "io": true, "dev": true,
	"cloud": true, "biz": true, "xyz": true,
	"de": true, "ch": true, "eu": true, "uk": true, "fr": true, "nl": true,
	"es": true, "se": true, "dk": true, "fi": true, "cz": true, "pl": true,
	"pt": true, "ie": true, "ca": true, "cc": true, "li": true, "lu": true,
	"tv": true, "ru": true,
}

// allowedNumberPrefixes are the fictional ranges examples may use, matched
// against a candidate normalised to digits with any national or international
// trunk prefix removed. +1555… is the one to reach for; the other two are the
// non-NANP examples the E.164 tests need.
var allowedNumberPrefixes = []string{
	"1555", "555",
	"442071234567",
	"33123456789",
}

// allowedAddrPrefixes: RFC 5737 documentation ranges, loopback, and the
// wildcard and unspecified addresses. Anything else — RFC 1918, carrier NAT, a
// public literal — is a deployment address.
var allowedAddrPrefixes = []string{"192.0.2.", "198.51.100.", "203.0.113.", "127."}

var allowedAddrs = map[string]bool{"0.0.0.0": true, "255.255.255.255": true}

// forbiddenValuesEnv holds exact values that no pattern can catch, newline- or
// comma-separated; a purely numeric entry is matched as a whole digit token, so
// a four-digit extension does not fire on every port number. It is an env var
// rather than a file in the repo for the reason given at the top. A local
// checkout can also put them in forbidden-values.env at the repo root, which
// .gitignore already covers.
const forbiddenValuesEnv = "MATRIX_SIP_BRIDGE_FORBIDDEN_VALUES"

const forbiddenValuesFile = "forbidden-values.env"

// scannedExtensions are the text files worth reading. Everything else in the
// tree is either generated, binary, or the licence.
var scannedExtensions = map[string]bool{
	".go": true, ".yaml": true, ".yml": true, ".md": true, ".sql": true,
}

var scannedNames = map[string]bool{"Dockerfile": true, "Makefile": true}

func TestRepositoryHasNoSiteSpecificValues(t *testing.T) {
	root := repoRoot(t)
	for _, f := range scanTree(t, root, nil) {
		t.Errorf("%s: %s", f.file, f.what)
	}
}

// TestRepositoryHasNoForbiddenExactValues covers what the patterns cannot. It
// skips when the list is not configured; run with -v to see whether it ran.
func TestRepositoryHasNoForbiddenExactValues(t *testing.T) {
	root := repoRoot(t)
	values := forbiddenExactValues(t, root)
	if len(values) == 0 {
		t.Skipf("no exact values configured: set %s or write %s", forbiddenValuesEnv, forbiddenValuesFile)
	}
	t.Logf("checking %d exact values", len(values))
	for _, f := range scanTree(t, root, values) {
		t.Errorf("%s: %s", f.file, f.what)
	}
}

func forbiddenExactValues(t *testing.T, root string) []string {
	t.Helper()
	raw := os.Getenv(forbiddenValuesEnv)
	if strings.TrimSpace(raw) == "" {
		data, err := os.ReadFile(filepath.Join(root, forbiddenValuesFile))
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("reading %s: %v", forbiddenValuesFile, err)
		}
		raw = string(data)
	}
	var values []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		if v := strings.TrimSpace(field); v != "" && !strings.HasPrefix(v, "#") {
			values = append(values, v)
		}
	}
	return values
}

type finding struct{ file, what string }

// scanTree walks root and reports every leak. exact is the optional list from
// outside the repo; with it nil only the patterns run.
func scanTree(t *testing.T, root string, exact []string) []finding {
	t.Helper()
	var found []finding
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
		rel = filepath.ToSlash(rel)
		for _, what := range checkContent(rel, string(data), exact) {
			found = append(found, finding{rel, what})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	return found
}

// selfPath is exempt from the pattern scan: it carries the allowlists and the
// fixtures that prove the patterns fire.
const selfPath = "pkg/connector/generic_test.go"

func checkContent(name, content string, exact []string) []string {
	if name == selfPath {
		return nil
	}
	var out []string
	for _, host := range findForeignHosts(content) {
		out = append(out, fmt.Sprintf("hostname %q is not a documentation or infrastructure domain", host))
	}
	for _, num := range findForeignNumbers(content) {
		out = append(out, fmt.Sprintf("phone number %q is not from a fictional range", num))
	}
	for _, addr := range findForeignAddrs(content) {
		out = append(out, fmt.Sprintf("IP literal %q is not from RFC 5737", addr))
	}
	for _, value := range exact {
		hit := strings.Contains(content, value)
		if isDigits(value) {
			hit = containsNumberToken(content, value)
		}
		if hit {
			out = append(out, "leaks a configured site-specific value")
		}
	}
	return out
}

var hostPattern = regexp.MustCompile(`(?i)[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+`)

func findForeignHosts(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range hostPattern.FindAllStringIndex(content, -1) {
		if m[0] > 0 && isHostChar(content[m[0]-1]) {
			continue
		}
		host := strings.ToLower(content[m[0]:m[1]])
		if !scannedTLDs[host[strings.LastIndexByte(host, '.')+1:]] {
			continue
		}
		if host == "localhost" || seen[host] {
			continue
		}
		allowed := false
		for _, suffix := range allowedHostSuffixes {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				allowed = true
				break
			}
		}
		if !allowed {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out
}

// isHostChar reports whether b could be part of a hostname, so that a match
// starting mid-token is not read as one.
func isHostChar(b byte) bool {
	return b == '.' || b == '-' || b == '_' || isDigit(b) ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

var addrPattern = regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}`)

func findForeignAddrs(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range addrPattern.FindAllStringIndex(content, -1) {
		if m[0] > 0 && (isDigit(content[m[0]-1]) || content[m[0]-1] == '.') {
			continue
		}
		// A dotted quad written next to more digits is not an address: it is a
		// number with separators, e.g. +1.555.123.4567.
		if m[1] < len(content) && (isDigit(content[m[1]]) || content[m[1]] == '.') {
			continue
		}
		addr := content[m[0]:m[1]]
		if seen[addr] || allowedAddrs[addr] || !isDottedQuad(addr) {
			continue
		}
		allowed := false
		for _, prefix := range allowedAddrPrefixes {
			if strings.HasPrefix(addr, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

// isDottedQuad rejects the same shape with an octet out of range, which is how
// a phone number with dots for separators reads.
func isDottedQuad(addr string) bool {
	for _, octet := range strings.Split(addr, ".") {
		n := 0
		for i := 0; i < len(octet); i++ {
			n = n*10 + int(octet[i]-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// findForeignNumbers reports phone-number-shaped digit strings that are not
// from a fictional range: a leading + with its separators collapsed, or a bare
// run of at least minBareDigits digits standing on its own.
//
// Two shapes are excluded before the allowlist is consulted, because they are
// how durations, timestamps and test scaffolding are written and are not
// numbers anyone could dial: five or more identical digits in a row, and a
// strictly sequential run (0123456789…).
const minBareDigits = 7

func findForeignNumbers(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, cand := range numberCandidates(content) {
		digits := strings.TrimLeft(strings.TrimPrefix(cand, "00"), "0")
		if len(digits) < minBareDigits || hasRepeatedRun(digits, 5) || isSequential(digits) {
			continue
		}
		allowed := false
		for _, prefix := range allowedNumberPrefixes {
			if strings.HasPrefix(digits, prefix) {
				allowed = true
				break
			}
		}
		if !allowed && !seen[cand] {
			seen[cand] = true
			out = append(out, cand)
		}
	}
	return out
}

// numberCandidates collects the digits of every dialable-looking string. A +
// form absorbs the separators a number is written with, and is capped at the
// E.164 maximum so that a second number behind a separator is not swallowed
// into the first; the bare scan then sees it, because scanning resumes right
// after the first digit run.
func numberCandidates(content string) []string {
	const maxE164 = 15
	var out []string
	for i := 0; i < len(content); i++ {
		c := content[i]
		if c == '+' && i+1 < len(content) && isDigit(content[i+1]) {
			var digits strings.Builder
			for j := i + 1; j < len(content) && digits.Len() < maxE164; j++ {
				if isDigit(content[j]) {
					digits.WriteByte(content[j])
					continue
				}
				if !strings.ContainsRune(" ()-./", rune(content[j])) {
					break
				}
			}
			out = append(out, digits.String())
			continue
		}
		if !isDigit(c) || (i > 0 && isIdentChar(content[i-1])) {
			continue
		}
		end := i
		for end < len(content) && isDigit(content[end]) {
			end++
		}
		if end-i >= minBareDigits && (end == len(content) || !isIdentChar(content[end])) {
			out = append(out, content[i:end])
		}
		i = end - 1
	}
	return out
}

func hasRepeatedRun(digits string, n int) bool {
	run := 1
	for i := 1; i < len(digits); i++ {
		if digits[i] == digits[i-1] {
			if run++; run >= n {
				return true
			}
			continue
		}
		run = 1
	}
	return false
}

func isSequential(digits string) bool {
	for i := 1; i < len(digits); i++ {
		if (digits[i]-'0')%10 != (digits[i-1]-'0'+1)%10 {
			return false
		}
	}
	return true
}

// containsNumberToken reports whether needle appears in content without a digit
// on either side of it. Exact numeric values are matched this way because they
// are short enough to occur inside an unrelated digit string.
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

func isIdentChar(b byte) bool {
	return b == '_' || isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

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

// TestGuardCatchesPlantedValues is the other half of the guard: that it fires.
// The planted values are invented, and belong to nobody — the point is their
// shape.
func TestGuardCatchesPlantedValues(t *testing.T) {
	root := t.TempDir()
	planted := "peer: sip:+41791234567@pbx.internal-trunk.ch\nlisten: 172.16.5.4:5060\next 9911\n"
	if err := os.WriteFile(filepath.Join(root, "example.yaml"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	found := scanTree(t, root, []string{"9911"})
	want := []string{"pbx.internal-trunk.ch", "41791234567", "172.16.5.4", "configured site-specific value"}
	for _, w := range want {
		hit := false
		for _, f := range found {
			hit = hit || strings.Contains(f.what, w)
		}
		if !hit {
			t.Errorf("planted %q was not reported; findings: %v", w, found)
		}
	}
}

// TestGuardAcceptsPlaceholders keeps the patterns from crying wolf on the forms
// the repo is supposed to use.
func TestGuardAcceptsPlaceholders(t *testing.T) {
	ok := []string{
		"sip:+15551234567@pbx.example.com", "+1 (555) 123-4567", "0015551234567",
		"+442071234567", "+33123456789", "c=IN IP4 192.0.2.10", "198.51.100.7",
		"127.0.0.1:5060", "0.0.0.0", "+1.555.123.4567",
		"github.com/lhns/matrix-sip-bridge",
		"ghcr.io/lhns/matrix-sip-bridge", "go.mau.fi/mautrix-bridgev2",
		"time.UnixMilli(1700000000000)", "const long = \"7200000\"",
		"tt.in", "h.mx.portal", "strings.Contains", "network.sip.domain",
		"s.log.Info(\"call ended\")",
	}
	for _, s := range ok {
		if got := checkContent("fixture.md", s, nil); len(got) != 0 {
			t.Errorf("%q: unexpected findings %v", s, got)
		}
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
