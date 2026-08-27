package maintenance

// purpose_binding_coverage_test.go keeps the purpose binding complete as the
// code grows (#277).
//
// The binding is only worth anything if EVERY reader uses it. One site left on
// the unbound crypto.Decrypt is a column where a moved blob still opens
// silently -- and it would be invisible, because that site keeps working.
//
// Enforced against the source tree rather than against a list someone
// maintains: a list of call sites is exactly the kind of inventory that goes
// stale, which is the lesson of the sibling guards in this package.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// unboundReaderPattern is a call to the purpose-less decrypt.
var unboundReaderPattern = regexp.MustCompile(`\bcrypto\.Decrypt\s*\(`)

// unboundWriterPattern is a call to the purpose-less encrypt.
var unboundWriterPattern = regexp.MustCompile(`\bcrypto\.Encrypt\s*\(`)

// minScannedFiles is the empty-universe floor, checked as a pure function so it
// can be falsified directly rather than by lowering it.
const minScannedFiles = 40

func checkScanNotVacuous(n int) error {
	if n < minScannedFiles {
		return errScanTooSmall
	}
	return nil
}

var errScanTooSmall = &scanError{}

type scanError struct{}

func (e *scanError) Error() string {
	return "the source scan reached too few files: it is not walking the tree, so every assertion " +
		"over it passes without looking at anything"
}

// TestEveryReaderIsPurposeBound is the R1 property.
//
// R1 (this release) switches every READER to DecryptFor while writers still
// produce unbound values. That ordering is not cosmetic: the deployment runs
// multiple replicas, so a writer release reaching a replica that cannot read
// bound values would fail on the first credential saved. Readers must ship
// first, everywhere, before writers ship at all.
func TestEveryReaderIsPurposeBound(t *testing.T) {
	offenders, scanned := scanFor(t, unboundReaderPattern)
	if err := checkScanNotVacuous(scanned); err != nil {
		t.Fatalf("%v", err)
	}
	for _, o := range offenders {
		t.Errorf("%s calls crypto.Decrypt, which passes no purpose.\n"+
			"Use crypto.DecryptFor with the purpose for that column. A reader left unbound is a "+
			"column where a ciphertext moved from another column still opens silently, which is "+
			"the defect #277 is about -- and it stays invisible, because the site keeps working.", o)
	}
}

// TestEveryWriterIsPurposeBound is the R2 property, and it replaces the R1
// ordering guard that used to sit here.
//
// R1 shipped every READER on DecryptFor while writers stayed unbound, because
// the deployment runs 2-3 backend replicas and a bound value written by a new
// replica must never reach an old one that cannot read it. That guard asserted
// the writers were still unbound and FAILED when they were not, so flipping
// them was something someone had to do deliberately. This is that deliberate
// step: R1 is deployed, and the guard is now the mirror image.
//
// From here a new writer that calls the unbound crypto.Encrypt is a column
// whose fresh secrets are not bound to anything -- silently, because the site
// works fine.
func TestEveryWriterIsPurposeBound(t *testing.T) {
	offenders, scanned := scanFor(t, unboundWriterPattern)
	if err := checkScanNotVacuous(scanned); err != nil {
		t.Fatalf("%v", err)
	}
	for _, o := range offenders {
		t.Errorf("%s calls crypto.Encrypt, which binds the value to nothing.\n"+
			"Use crypto.EncryptFor with the purpose for that column. A secret written unbound is "+
			"one a ciphertext from another column could later be swapped for without any "+
			"cryptographic signal (#277).", o)
	}
}

// scanFor walks internal/ and returns the sites matching pat, excluding the
// crypto package itself (which defines both) and test files.
func scanFor(t *testing.T, pat *regexp.Regexp) ([]string, int) {
	t.Helper()
	root := filepath.Join(moduleRoot(t), "internal")
	var out []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "crypto" {
				return filepath.SkipDir // defines both; naming them is its job
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path) // #nosec G304 -- test-only walk of the module
		if readErr != nil {
			return readErr
		}
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if pat.MatchString(line) {
				rel, _ := filepath.Rel(moduleRoot(t), path)
				out = append(out, rel+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out, scanned
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestScanFloorRefusesAnEmptyUniverse falsifies the floor directly: it cannot
// be falsified by lowering it, because the tree is healthy and nothing else
// would fail.
func TestScanFloorRefusesAnEmptyUniverse(t *testing.T) {
	if err := checkScanNotVacuous(0); err == nil {
		t.Error("a scan that reached no files was accepted")
	}
	if err := checkScanNotVacuous(minScannedFiles); err != nil {
		t.Errorf("a scan at the floor was rejected: %v", err)
	}
}
