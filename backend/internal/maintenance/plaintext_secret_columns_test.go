package maintenance

// plaintext_secret_columns_test.go is the third category of the secret
// inventory (#511).
//
// rekey_coverage_test.go answers two questions: which AAD-bound columns the
// rekey sweep covers, and which internal/crypto.Encrypt call sites exist. Both
// are about columns that ARE encrypted.
//
// It had no answer for a column that holds a credential-shaped value and is
// NOT encrypted. drift_runs.callback_token and health_runs.callback_token are
// two, and they sat outside every inventory -- so anyone auditing this service
// found two TEXT NOT NULL columns holding a value the code calls a token, with
// nothing explaining why they are readable. That reads as an oversight. It is
// not one.
//
// WHY THEY ARE CORRECTLY PLAINTEXT. Both are consumed by an atomic
// compare-and-clear:
//
//	UPDATE drift_runs SET callback_token='' WHERE id=$1 AND callback_token=$2
//
// which is what makes the machine callback one-shot: a replay finds the token
// already cleared and is rejected. ENCRYPTING THEM WOULD BREAK THAT. AES-GCM
// uses a fresh nonce per seal, so the same token encrypts differently every
// time and the WHERE clause can never match. Preserving the one-shot guarantee
// under encryption means either a deterministic scheme (worse) or
// read-compare-write in the application (no longer atomic, so the replay race
// reopens).
//
// So the point of this file is not to protect those two columns. It is to make
// their plaintextness a RECORDED DECISION, and to make the next one a decision
// too.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// plaintextSecretColumns are credential-shaped columns deliberately stored in
// the clear, with the reason.
//
// An entry here is a claim that encryption would be WRONG, not merely absent.
// "We have not got to it yet" is not a reason -- that belongs in an issue, and
// the column belongs in unboundEncryptSites once it is sealed.
var plaintextSecretColumns = map[string]string{
	"drift_runs.callback_token": "single-use nonce, not a stored credential. Consumed by an atomic " +
		"compare-and-clear (DriftRepository.ConsumeCallbackToken: UPDATE ... SET callback_token='' " +
		"WHERE id=$1 AND callback_token=$2), which is what makes the CI callback one-shot. " +
		"Encrypting it breaks that: AES-GCM re-nonces per seal, so the equality can never match. " +
		"Also json:\"-\", and blanked from list responses.",
	"health_runs.callback_token": "as above -- HealthRepository.ConsumeCallbackToken, same " +
		"compare-and-clear, same one-shot guarantee, same reason encryption would break it.",
}

// credentialNamePattern is what makes a column worth classifying.
//
// Matched on the NAME, which is a heuristic and is meant to be: the question is
// "would an auditor reading this schema wonder whether this holds a secret?",
// and a name is exactly what an auditor reads. A false positive costs one line
// of declaration; a false negative is a credential nobody classified.
var credentialNamePattern = regexp.MustCompile(`(?i)(token|secret|password|credential|private_key|api_key|passphrase)`)

// alreadyProtectedPattern marks columns whose name says they are sealed or
// digested, and which the other two inventories therefore cover.
//
// Trusted on the name alone HERE, deliberately: whether a column named
// encrypted_* is genuinely written through crypto.Encrypt is what
// TestRekeyCoverage_EveryUnboundEncryptSiteIsDeclared checks, from the write
// side. Duplicating that check here would give two half-answers instead of two
// whole ones.
var alreadyProtectedPattern = regexp.MustCompile(`(?i)(encrypted|sealed|_hash$|hashed)`)

// minCredentialColumns is the empty-universe floor. The schema carried nine
// credential-named columns when this was written.
const minCredentialColumns = 6

// checkCredentialScanNotVacuous is the empty-universe guard, a pure function so
// it can be falsified DIRECTLY.
//
// A floor cannot be falsified by removing it: the schema is healthy, so nothing
// else fails and the mutation looks harmless. Handing it an empty scan is the
// only way to prove it would object -- the same treatment checkNotVacuous gets
// in the schemaguard package, and for the same reason.
func checkCredentialScanNotVacuous(found map[string]string) error {
	if len(found) < minCredentialColumns {
		return fmt.Errorf("the migration scan found only %d credential-named columns (floor %d): "+
			"it has stopped matching, so this test would accept any inventory as complete",
			len(found), minCredentialColumns)
	}
	return nil
}

// TestEveryCredentialShapedColumnIsClassified checks both directions.
func TestEveryCredentialShapedColumnIsClassified(t *testing.T) {
	found := scanCredentialColumns(t)

	if err := checkCredentialScanNotVacuous(found); err != nil {
		t.Fatalf("%v", err)
	}

	var protected, plaintext []string
	for col := range found {
		if alreadyProtectedPattern.MatchString(col) {
			protected = append(protected, col)
			continue
		}
		plaintext = append(plaintext, col)
	}
	sort.Strings(protected)
	sort.Strings(plaintext)
	t.Logf("credential-named columns: %d (%d named encrypted/hashed, %d plaintext)",
		len(found), len(protected), len(plaintext))

	// Direction 1: nothing credential-shaped is plaintext without a reason.
	for _, col := range plaintext {
		if _, ok := plaintextSecretColumns[col]; !ok {
			t.Errorf("%s (%s) holds a credential-shaped value, is not named as encrypted, and is "+
				"not declared in plaintextSecretColumns.\n"+
				"Either seal it -- and add the call site to unboundEncryptSites -- or declare here "+
				"WHY storing it in the clear is correct. A credential nobody classified reads as an "+
				"oversight to the next person auditing this schema, which is how these two "+
				"callback_token columns came to be filed as a finding (#511).", col, found[col])
		}
	}

	// Direction 2: no stale claims. An entry naming a column that no longer
	// exists, or one that has since been encrypted, is a considered-looking
	// decision about something that is not there.
	for col, why := range plaintextSecretColumns {
		if _, ok := found[col]; !ok {
			t.Errorf("plaintextSecretColumns declares %s, which no migration defines. Renamed, "+
				"dropped, or a typo -- either way the entry is now fiction.", col)
			continue
		}
		if alreadyProtectedPattern.MatchString(col) {
			t.Errorf("plaintextSecretColumns declares %s, but its name says it is encrypted. "+
				"If it was sealed, remove the entry and declare the write site instead.", col)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("plaintextSecretColumns[%s] has no reason. An entry with no reason is an "+
				"exemption nobody can re-read, which is the thing this file exists to prevent.", col)
		}
	}
}

// scanCredentialColumns walks the migrations and returns every column whose
// name suggests it holds a credential, mapped to the file that introduced it.
//
// Line-oriented rather than a SQL parse: these migrations declare one column
// per line, and a parser would be a large dependency for a question a name
// answers. It tracks CREATE TABLE bodies and ALTER TABLE ... ADD COLUMN.
func scanCredentialColumns(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join(moduleRoot(t), "internal", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	reCreate := regexp.MustCompile(`(?i)^CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+([a-z_][a-z0-9_."]*)\s*\(`)
	reAlterAdd := regexp.MustCompile(`(?i)^ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_."]*)\s+ADD\s+COLUMN(?:\s+IF\s+NOT\s+EXISTS)?\s+([a-z_][a-z0-9_]*)`)
	reIdent := regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

	out := map[string]string{}
	record := func(table, col, file string) {
		if !credentialNamePattern.MatchString(col) {
			return
		}
		key := bare(table) + "." + col
		if _, seen := out[key]; !seen {
			out[key] = file
		}
	}

	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name)) // #nosec G304 -- test-only, directory listing
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		table := ""
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "--") {
				continue
			}
			if mm := reCreate.FindStringSubmatch(line); mm != nil {
				table = mm[1]
				continue
			}
			if strings.HasPrefix(line, ");") {
				table = ""
				continue
			}
			if mm := reAlterAdd.FindStringSubmatch(line); mm != nil {
				record(mm[1], mm[2], name)
				continue
			}
			if table == "" {
				continue
			}
			first := strings.TrimRight(strings.Trim(strings.Fields(line)[0], `"`), ",")
			if reIdent.MatchString(first) {
				record(table, first, name)
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			t.Fatalf("scan %s: %v", name, err)
		}
	}
	return out
}

// bare strips a schema qualifier and quoting from a table name.
func bare(t string) string {
	t = strings.Trim(t, `"`)
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return strings.Trim(t, `"`)
}

// TestCredentialScanFloorRefusesAnEmptyUniverse falsifies the floor directly.
func TestCredentialScanFloorRefusesAnEmptyUniverse(t *testing.T) {
	if err := checkCredentialScanNotVacuous(map[string]string{}); err == nil {
		t.Error("an empty scan was accepted. Every assertion in this file iterates the scan result, " +
			"so an empty one passes them all while classifying nothing -- which is exactly what a " +
			"scanner that has stopped matching produces.")
	}
	full := map[string]string{}
	for i := 0; i < minCredentialColumns; i++ {
		full[fmt.Sprintf("t%d.secret", i)] = "x.sql"
	}
	if err := checkCredentialScanNotVacuous(full); err != nil {
		t.Errorf("a scan at the floor was rejected: %v", err)
	}
}
