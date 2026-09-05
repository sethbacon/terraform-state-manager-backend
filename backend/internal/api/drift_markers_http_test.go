package api

import (
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// The completeness markers say what a drift check did NOT do. They are the
// difference between "we checked and it was clean" and "we never finished
// checking", so they have to survive ingestion and land on the stored record —
// a record that cannot answer "was this complete?" is the fail-open these
// markers exist to close.
//
// Both receivers used to declare a DTO without them and decode with plain
// json.Unmarshal / ShouldBindJSON, so a sender got 200 OK and silent data loss.
// The dispatched jq templates this very package generates post all five.

// TestIngestDrift_PersistsCompletenessMarkers is the round trip: a push carrying
// all five markers must bind them, hand them to the repository, and return them
// on the stored record.
func TestIngestDrift_PersistsCompletenessMarkers(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("FROM drift_records WHERE source_id .+ external_ref").WithArgs("s1", "run-88").
		WillReturnRows(sqlmock.NewRows(driftRecCols)) // no replay
	// The markers must reach the INSERT, not stop at the handler.
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "envs/prod.tfstate", nil, nil, "ingest", "warning",
			3, 0, 0, `[{"address":"aws_instance.web","actions":["create"]}]`, "run-88",
			true, 7, 15, true, true, []string{testActingOrg}).
		WillReturnRows(driftRecRowMarked("r1", "open", "warning", true, 7, 15, true, true))

	body := `{
		"source_id": "s1", "state_key": "envs/prod.tfstate", "external_ref": "run-88",
		"added": 3, "changed": 0, "destroyed": 0,
		"summary": [{"address":"aws_instance.web","actions":["create"]}],
		"truncated": true, "omitted_entries": 7, "omitted_attrs": 15,
		"unparseable": true, "unmasked": true
	}`
	w := e.do(http.MethodPost, "/api/v1/drift/ingest", body)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	for _, want := range []string{
		`"truncated":true`, `"omitted_entries":7`, `"omitted_attrs":15`,
		`"unparseable":true`, `"unmasked":true`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("stored record must report %s; got %s", want, w.Body.String())
		}
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("markers must reach the repository: %v", err)
	}
}

// TestRunResults_PersistsCompletenessMarkers is the same round trip on the
// dispatched-run callback — the endpoint TSM's own generated jq posts to.
func TestRunResults_PersistsCompletenessMarkers(t *testing.T) {
	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(
		sqlmock.NewRows(driftCols).AddRow("d1", "p1", "s1", "envs/prod.tfstate", "", "", "dispatched",
			nil, nil, nil, nil, nil, "", "tok1", "alice", "2026-06-11", "2026-06-11",
			false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
			nil, "", ""))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	// The run's source, loaded under the authority the callback token derived,
	// before anything keyed on it is written (#393).
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "envs/prod.tfstate", "p1", "d1", "run", "warning",
			1, 0, 0, `[{"address":"a.b","actions":["create"]}]`, nil,
			true, 4, 0, false, true, []string{testActingOrg}).
		WillReturnRows(driftRecRowMarked("r1", "open", "warning", true, 4, 0, false, true))

	body := `{"added":1,"changed":0,"destroyed":0,"drifted":true,
		"summary":[{"address":"a.b","actions":["create"]}],
		"truncated":true,"omitted_entries":4,"omitted_attrs":0,
		"unparseable":false,"unmasked":true}`
	w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results", body,
		"X-TSM-Callback-Token", "tok1")
	if w.Code != http.StatusOK {
		t.Fatalf("callback: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("callback markers must reach the repository: %v", err)
	}
}

// TestDriftMarkers_TruncatedIsWidenedNotNarrowed guards that a sender which
// reports omissions but forgets the flag still gets a record that says the
// summary was bounded. The flag can only ever be widened by the receiver:
// under-reporting a bound is the direction that misleads a consumer into
// treating an absent resource as evidence of absence.
func TestDriftMarkers_TruncatedIsWidenedNotNarrowed(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "k", nil, nil, "ingest", "warning", 1, 0, 0, nil, nil,
			true, 2, 0, false, false, []string{testActingOrg}). // truncated derived from omitted_entries
		WillReturnRows(driftRecRowMarked("r1", "open", "warning", true, 2, 0, false, false))

	w := e.do(http.MethodPost, "/api/v1/drift/ingest",
		`{"source_id":"s1","state_key":"k","added":1,"omitted_entries":2,"truncated":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("omissions must imply truncated: %v", err)
	}
}

// TestIngestDrift_ServerParsedPlanOverridesClaimedMarkers pins that when a raw
// plan is supplied, the markers stored are the ones this server derived from
// reading it — not the sender's account of it. Otherwise a producer could assert
// a clean bill of completeness over a plan that contradicts it, or (as here)
// claim unparseable for a plan that parses fine and suppress its own detection.
func TestIngestDrift_ServerParsedPlanOverridesClaimedMarkers(t *testing.T) {
	e := newDriftEnv(t)
	sourceRowFor(e, "s1")
	// The plan is small, readable, and carries an in-place change with NEITHER
	// sensitivity mirror: truncated=false, omitted=0/0, unparseable=false,
	// unmasked=TRUE — the exact inverse of every claim in the body below.
	e.mock.ExpectQuery("INSERT INTO drift_records").
		WithArgs("s1", "k", nil, nil, "ingest", "warning", 0, 1, 0, sqlmock.AnyArg(), nil,
			false, 0, 0, false, true, []string{testActingOrg}).
		WillReturnRows(driftRecRowMarked("r1", "open", "warning", false, 0, 0, false, true))

	body := `{"source_id":"s1","state_key":"k",
		"plan":{"resource_changes":[{"address":"aws_db.x","change":{
			"actions":["update"],"before":{"pw":"a"},"after":{"pw":"b"}}}]},
		"truncated":true,"omitted_entries":99,"omitted_attrs":7,
		"unparseable":true,"unmasked":false}`
	w := e.do(http.MethodPost, "/api/v1/drift/ingest", body)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the parsed plan must decide the markers, not the sender: %v", err)
	}
}

// TestDriftMarkers_UnparseableDoesNotResolve is the fail-open this closes on the
// clean path. A result the sender could not read is not a verified-clean result,
// so it must never auto-resolve a live drift record — otherwise an unreadable
// plan silently closes an open finding.
func TestDriftMarkers_UnparseableDoesNotResolve(t *testing.T) {
	t.Run("ingest", func(t *testing.T) {
		e := newDriftEnv(t)
		sourceRowFor(e, "s1")
		// No ExpectExec for the resolve: issuing one is the failure.
		w := e.do(http.MethodPost, "/api/v1/drift/ingest",
			`{"source_id":"s1","state_key":"k","added":0,"unparseable":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("ingest: %d (%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"unverified"`) {
			t.Errorf("an unreadable result must not be reported clean; got %s", w.Body.String())
		}
		if err := e.mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unparseable must not resolve a record: %v", err)
		}
	})

	t.Run("callback", func(t *testing.T) {
		e := newDriftEnv(t)
		e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(
			sqlmock.NewRows(driftCols).AddRow("d1", "p1", "s1", "envs/prod.tfstate", "", "", "dispatched",
				nil, nil, nil, nil, nil, "", "tok1", "alice", "2026-06-11", "2026-06-11",
				false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
				nil, "", ""))
		e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tok1").
			WillReturnResult(sqlmock.NewResult(0, 1))
		e.mock.ExpectExec("UPDATE drift_runs").WillReturnResult(sqlmock.NewResult(0, 1))
		sourceRowFor(e, "s1")
		// The resolve is QUEUED and then required to go UNUSED. sqlmock does not
		// fail on an unexpected call — it just errors that call — so asserting the
		// absence of a resolve needs this inversion; a plain "no expectation" test
		// stays green with the guard removed and proves nothing.
		e.mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", "envs/prod.tfstate", []string{testActingOrg}).
			WillReturnResult(sqlmock.NewResult(0, 1))

		w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results",
			`{"added":0,"changed":0,"destroyed":0,"drifted":false,"unparseable":true}`,
			"X-TSM-Callback-Token", "tok1")
		if w.Code != http.StatusOK {
			t.Fatalf("callback: %d (%s)", w.Code, w.Body.String())
		}
		// The run itself is still recorded; only the record is left alone.
		if err := e.mock.ExpectationsWereMet(); err == nil {
			t.Error("unparseable callback resolved the drift record; an unreadable " +
				"result must not close a live finding")
		} else if !strings.Contains(err.Error(), "status='resolved'") {
			t.Errorf("the run result itself must still be recorded; unmet: %v", err)
		}
	})
}

// decodedJSONKeys returns every JSON key a struct decodes, flattening embedded
// structs the way encoding/json does — the markers ride in on an embedded
// `completeness`, so a guard that only walked the outer fields would report the
// whole marker set as unknown and then, once "fixed", pass vacuously.
func decodedJSONKeys(rt reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct {
			for k := range decodedJSONKeys(f.Type) {
				keys[k] = true
			}
			continue
		}
		if tag != "" && tag != "-" {
			keys[tag] = true
		}
	}
	return keys
}

// jqPayloadKeys returns the top-level keys of the `jq -n` object literal a
// dispatched template posts to the callback.
func jqPayloadKeys(t *testing.T, tmpl string) []string {
	t.Helper()
	obj := regexp.MustCompile(`\{status:"completed",[^}]*\}`).FindString(tmpl)
	if obj == "" {
		t.Fatalf("template has no jq callback payload literal")
	}
	var keys []string
	for _, m := range regexp.MustCompile(`([a-z_]+):`).FindAllStringSubmatch(obj, -1) {
		keys = append(keys, m[1])
	}
	if len(keys) == 0 {
		t.Fatalf("no keys parsed from payload literal %q", obj)
	}
	return keys
}

// TestDriftCallbackPayload_EveryGeneratedKeyIsDecoded is the mechanism that
// replaces strict decoding.
//
// DisallowUnknownFields is deliberately NOT used on these endpoints: the drift
// callback token is one-shot, so a rejected callback cannot be retried and the
// run would be stranded, and TSM hands users a workflow file they commit to
// their own repo — a newer template posting to an older server must degrade, not
// fail. The forward-compatibility that buys is exactly what let all five markers
// be dropped in silence.
//
// So the loop is closed statically instead, and on the half TSM actually
// controls: every key the templates in THIS package generate must be a key the
// RunResults DTO in THIS package decodes. A producer/consumer skew that TSM
// authored both sides of is a build failure rather than a discovery.
func TestDriftCallbackPayload_EveryGeneratedKeyIsDecoded(t *testing.T) {
	known := decodedJSONKeys(reflect.TypeOf(driftRunResultPayload{}))
	if len(known) == 0 {
		t.Fatal("no json tags found on the callback DTO; the guard would pass vacuously")
	}

	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		keys := jqPayloadKeys(t, tmpl)
		for _, k := range keys {
			if !known[k] {
				t.Errorf("%s template posts %q but the callback DTO drops it silently "+
					"(add the field, or stop generating the key)", name, k)
			}
		}
		// Belt and braces: the markers are the point of this guard, so assert the
		// template really still emits them rather than trusting an empty diff.
		for _, want := range []string{"unparseable", "unmasked", "truncated", "omitted_entries", "omitted_attrs"} {
			if !containsStr(keys, want) {
				t.Errorf("%s template no longer emits %q", name, want)
			}
		}
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
