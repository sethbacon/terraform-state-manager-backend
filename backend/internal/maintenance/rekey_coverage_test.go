package maintenance

// rekey_coverage_test.go is an inventory guard over the claim
// `rekey-targets verify` makes.
//
// That claim is load-bearing and unusually expensive to get wrong: a zero exit
// is what tells an operator TSM_ENCRYPTION_KEY_PREVIOUS can be deleted, and
// deleting it while a secret is still sealed under it destroys that secret. The
// gate is only as wide as the registry the sweep walks, and the registry is a
// hand-maintained list -- so a new encrypted column added anywhere in this
// service silently narrows a gate that keeps reporting success.
//
// This service encrypts secrets at rest in TWO different ways, and the
// difference decides what the gate can honestly certify:
//
//	identity/crypto.TokenCipher  AAD-bound, dual-key (TSM_ENCRYPTION_KEY_PREVIOUS
//	                             is the decryption fallback). ONE column today:
//	                             notification_channels.encrypted_target. This is
//	                             what the sweep covers.
//	internal/crypto              nil AAD, dual-key on READ since #368: Decrypt
//	                             retries TSM_ENCRYPTION_KEY_PREVIOUS. Every other
//	                             stored credential. A key change no longer makes
//	                             these unreadable at the restart -- but nothing
//	                             re-encrypts them either, so each stays on the OLD
//	                             key until it is next saved, and dropping the
//	                             previous key destroys every one that has not been.
//
// So the two inventories below answer two different questions, and both are
// checked in BOTH directions because a one-way check rots:
//
//   - sweptAADContexts / unsweptAADContexts -- of the columns that CAN require
//     the previous key, which does the sweep reach? This is the gate's scope.
//   - unboundEncryptSites -- which credentials does the gate say nothing about?
//     A new one appearing here must be a decision, not an accident, because
//     docs/secrets-rotation.md promises a specific list of things an operator
//     has to re-enter by hand after a key change.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sweptAADContexts maps an AAD context function to the registered column whose
// rows RekeyChannelTargets re-encrypts through it. One entry per column.
var sweptAADContexts = map[string]string{
	"TargetContext": "notification_channels.encrypted_target",
}

// unsweptAADContexts are AAD-bound columns no sweep touches, and why.
//
// Empty, and that is the finding rather than an oversight: this service seals
// exactly one column with an AAD, and the sweep covers it. An entry here would
// be a column that can require TSM_ENCRYPTION_KEY_PREVIOUS while the gate keeps
// reporting zero -- which is precisely the situation that must never be silent.
var unsweptAADContexts = map[string]string{}

// unboundEncryptSites is every internal/crypto.Encrypt call site: the secrets
// sealed with a nil AAD, and never re-encrypted after a key rotation.
//
// These are NOT covered by the rekey gate, and the reason CHANGED with #368
// without this comment changing with it -- which is the failure this file exists
// to prevent, one level up.
//
// It used to be that internal/crypto had no previous-key fallback to retire, so
// a rotation made these unreadable at the restart. Decrypt now retries
// TSM_ENCRYPTION_KEY_PREVIOUS, so they survive the rotation. What they still lack
// is anything that RE-ENCRYPTS them: each stays sealed under whichever key was
// current when it was last written, indefinitely, so dropping the previous key
// destroys every one that has not been saved since.
//
// So the gate cannot cover them for a different reason than before -- not
// "they break immediately" but "nothing converts them, and no command reports
// which are still on the old key." They are inventoried here so the table in
// docs/secrets-rotation.md cannot silently fall out of date, and so that adding
// a ninth is a decision.
//
// Giving these the dual-key treatment means moving them onto TokenCipher with a
// per-row AAD, which is a change to the storage format of eight columns and
// belongs to its own issue rather than being smuggled in behind a rotation fix.
//
// Keyed by file and enclosing function, which is what the scan can see and what
// stays stable across edits inside the function.
var unboundEncryptSites = map[string]string{
	// Shared by CreateCISource and UpdateCISource (Phase 1b, drift-fleet-scale.md):
	// the scan keys by enclosing function, not caller, so moving the encrypt
	// calls into one function both handlers call is one entry, not two.
	// workload_identity stores no encrypted column, so it adds nothing here.
	"internal/api/ci_sources.go:applyCISourceAuthMethod": "ci_sources.encrypted_token / encrypted_client_secret / " +
		"encrypted_app_private_key -- CI provider credentials",
	"internal/api/drift.go:CreatePipeline":        "drift pipeline_connections.encrypted_token",
	"internal/api/drift.go:UpdatePipeline":        "drift pipeline_connections.encrypted_token",
	"internal/api/notifications.go:PutSMTPConfig": "system_settings notifications_config -> SMTP password",
	"internal/api/setup/oidc.go:SaveOIDCConfig":   "oidc_configs.client_secret_encrypted",
	"internal/api/setup/sources.go:SaveSource":    "state_sources.encrypted_credentials (first-run setup path)",
	"internal/api/sources.go:CreateSource":        "state_sources.encrypted_credentials",
	"internal/api/sources.go:UpdateSource":        "state_sources.encrypted_credentials",
}

// scanned is what one walk of the source tree found.
type scanned struct {
	// aadContexts maps an exported *Context function used as the AAD argument
	// of a seal or open, to where it was first seen.
	aadContexts map[string]string
	// unboundEncrypts maps "file:enclosingFunc" for each internal/crypto.Encrypt
	// call, to the same key (kept as a map for set semantics).
	unboundEncrypts map[string]string
}

// scanSecretSites walks internal/ and records both families.
//
// AAD functions are matched on the ARGUMENT POSITION of a *WithContext* call
// rather than on where they are declared: what matters is that a ciphertext
// somewhere is bound by it, not which package it lives in (TargetContext is in
// the shared identity module, not this repo). The bare name "Context" is
// excluded -- that is ctx plumbing -- and so are unexported names, which are the
// registry's own col.context indirection rather than an AAD derivation.
func scanSecretSites(t *testing.T) scanned {
	t.Helper()
	root := moduleRoot(t)
	out := scanned{aadContexts: map[string]string{}, unboundEncrypts: map[string]string{}}

	err := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // unparseable files are not this test's business
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			site := rel + ":" + fn.Name.Name
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// Encrypt AND EncryptFor. #277 bound these values to a purpose,
				// which changed the function name and nothing this inventory is
				// about: the columns are still sealed by internal/crypto, still
				// never re-encrypted, and so still stay on whichever key was
				// current when each was last written.
				//
				// Matching only "Encrypt" made this guard report an EMPTY set the
				// moment the writers moved -- and an empty set is how an
				// inventory stops covering anything while still passing.
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "crypto" &&
					(sel.Sel.Name == "Encrypt" || sel.Sel.Name == "EncryptFor") {
					out.unboundEncrypts[site] = site
				}
				if !strings.Contains(sel.Sel.Name, "WithContext") {
					return true
				}
				for _, arg := range call.Args {
					inner, ok := arg.(*ast.CallExpr)
					if !ok {
						continue
					}
					var name string
					switch f := inner.Fun.(type) {
					case *ast.Ident:
						name = f.Name
					case *ast.SelectorExpr:
						name = f.Sel.Name
					}
					if name == "Context" || !strings.HasSuffix(name, "Context") {
						continue
					}
					if r := name[0]; r < 'A' || r > 'Z' {
						continue // col.context(...), not an exported AAD derivation
					}
					if _, seen := out.aadContexts[name]; !seen {
						out.aadContexts[name] = rel
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

func TestRekeyCoverage_EveryAADContextIsDeclaredSweptOrNot(t *testing.T) {
	found := scanSecretSites(t).aadContexts
	if len(found) == 0 {
		t.Fatal("the scan found no AAD context functions at all; it has stopped matching " +
			"and would certify any registry as complete")
	}

	for name, where := range found {
		_, swept := sweptAADContexts[name]
		_, unswept := unsweptAADContexts[name]
		switch {
		case swept && unswept:
			t.Errorf("%s is listed as both swept and unswept", name)
		case !swept && !unswept:
			t.Errorf("%s binds a secret (first seen in %s) and is in neither inventory.\n"+
				"Either register a column for it in bindtargets.go and add it to sweptAADContexts, "+
				"or add it to unsweptAADContexts with why a rotation does not need to reach it -- "+
				"and say so in docs/secrets-rotation.md, because `rekey-targets verify` returning "+
				"zero is what an operator deletes TSM_ENCRYPTION_KEY_PREVIOUS on.", name, where)
		}
	}

	for name := range sweptAADContexts {
		if _, ok := found[name]; !ok {
			t.Errorf("sweptAADContexts lists %s but nothing seals with it any more. "+
				"Remove the entry -- a stale inventory is worse than none.", name)
		}
	}
	for name := range unsweptAADContexts {
		if _, ok := found[name]; !ok {
			t.Errorf("unsweptAADContexts lists %s but nothing seals with it any more. "+
				"Remove the entry -- a stale inventory is worse than none.", name)
		}
	}
}

// The other direction: the swept inventory and the registry must describe the
// same set of columns. Without this a column could be dropped from the registry
// -- narrowing the gate -- while the inventory still claimed it was covered.
func TestRekeyCoverage_SweptInventoryMatchesTheRegistry(t *testing.T) {
	registered := map[string]bool{}
	for _, col := range columns {
		registered[col.name] = true
	}
	if len(registered) == 0 {
		t.Fatal("the sweep registry is empty; rekey-targets would certify a database it never read")
	}

	claimed := map[string]string{}
	for fn, colName := range sweptAADContexts {
		if !registered[colName] {
			t.Errorf("sweptAADContexts says %s covers %q, which is not a registered column", fn, colName)
			continue
		}
		if other, dup := claimed[colName]; dup {
			t.Errorf("%s and %s both claim to cover %q; one column, one entry", other, fn, colName)
			continue
		}
		claimed[colName] = fn
	}

	for _, col := range columns {
		if _, ok := claimed[col.name]; !ok {
			t.Errorf("column %q is registered but no sweptAADContexts entry claims it.\n"+
				"Every registered column is re-encrypted by RekeyChannelTargets, so it belongs in the "+
				"inventory the rotation gate's scope is read from.", col.name)
		}
	}
}

// The gate's blind spot, kept explicit. Every internal/crypto.Encrypt site is a
// credential a key change destroys and a human has to re-enter, and
// docs/secrets-rotation.md names them. A new one must not appear without that
// list being revisited, and a listed one must not linger after the code moves.
func TestRekeyCoverage_EveryUnboundEncryptSiteIsDeclared(t *testing.T) {
	found := scanSecretSites(t).unboundEncrypts
	if len(found) == 0 {
		t.Fatal("the scan found no internal/crypto seal sites at all; it has stopped matching and " +
			"would accept any inventory as complete")
	}

	for site := range found {
		if _, ok := unboundEncryptSites[site]; !ok {
			t.Errorf("%s seals a secret with internal/crypto (nil AAD) and is not in "+
				"unboundEncryptSites.\nThat cipher reads through TSM_ENCRYPTION_KEY_PREVIOUS "+
				"but nothing re-encrypts it, so this value stays on whichever key was current "+
				"when it was written -- and `rekey-targets verify` reports zero without ever "+
				"looking at it.\nDeclare it with the column it writes, and add that column to "+
				"the rotation table in docs/secrets-rotation.md.", site)
		}
	}
	for site, why := range unboundEncryptSites {
		if _, ok := found[site]; !ok {
			t.Errorf("unboundEncryptSites lists %s (%s) but nothing there calls crypto.Encrypt any "+
				"more. Remove the entry -- a stale inventory is worse than none.", site, why)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("unboundEncryptSites[%s] has no reason; the entry must say which column it "+
				"writes, because that is what an operator re-enters by hand.", site)
		}
	}
}

// moduleRoot walks up to the module root so the test runs from its own package
// directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the module root (no go.mod found walking up)")
	return ""
}
