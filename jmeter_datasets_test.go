package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const csvSteps = `[{"type":"http","name":"Login","method":"POST","url":"http://node-app:3000/login?u=${user}","body":"{\"p\":\"${pass}\"}"}]`

func datasetsJSON(t *testing.T, csv map[string]interface{}) string {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{"csv": csv})
	if err != nil {
		t.Fatalf("marshal datasets: %v", err)
	}
	return string(b)
}

func csvDataSetBlock(t *testing.T, jmx string) string {
	t.Helper()
	start := strings.Index(jmx, "<CSVDataSet")
	if start < 0 {
		t.Fatalf("plan has no CSVDataSet:\n%s", jmx)
	}
	end := strings.Index(jmx[start:], "</CSVDataSet>")
	if end < 0 {
		t.Fatalf("unterminated CSVDataSet:\n%s", jmx)
	}
	return jmx[start : start+end+len("</CSVDataSet>")]
}

// The defect: a scenario with a dataset produced a plan with no CSVDataSet, so ${user}
// reached the target as literal text and nothing said so.
func TestGeneratedPlanEmitsCSVDataSet(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass",
		"inline":        "alice,secret1\nbob,secret2\n",
	}))
	if ds == nil || !ds.emits() {
		t.Fatalf("dataset should emit: %#v", ds)
	}
	jmx := generateJMXFromUpsertData("csv-plan", "http://node-app:3000/", "GET", "", 4, 60,
		json.RawMessage(csvSteps), nil, ds)
	block := csvDataSetBlock(t, jmx)
	t.Logf("emitted CSVDataSet:\n%s", block)
	for _, want := range []string{
		`testclass="CSVDataSet"`,
		`<stringProp name="filename">data.csv</stringProp>`,
		`<stringProp name="fileEncoding">UTF-8</stringProp>`,
		`<stringProp name="variableNames">user,pass</stringProp>`,
		`<stringProp name="delimiter">,</stringProp>`,
		`<boolProp name="ignoreFirstLine">false</boolProp>`,
		`<boolProp name="quotedData">true</boolProp>`,
		`<boolProp name="recycle">true</boolProp>`,
		`<boolProp name="stopThread">false</boolProp>`,
		`<stringProp name="shareMode">shareMode.all</stringProp>`,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("CSVDataSet missing %q\n%s", want, block)
		}
	}
	// Config element must sit at Test Plan level, before the first ThreadGroup.
	if strings.Index(jmx, "<CSVDataSet") > strings.Index(jmx, "<ThreadGroup") {
		t.Fatal("CSVDataSet must precede the first ThreadGroup")
	}
	if !strings.Contains(jmx, "</CSVDataSet>\n      <hashTree/>") {
		t.Fatalf("CSVDataSet needs its sibling <hashTree/>:\n%s", jmx)
	}
	if strings.Count(jmx, "<CSVDataSet") != 1 {
		t.Fatalf("expected exactly one CSVDataSet, got %d", strings.Count(jmx, "<CSVDataSet"))
	}
}

func TestNoDatasetMeansNoCSVDataSet(t *testing.T) {
	if ds := perfCSVDatasetFromJSON("{}"); ds != nil {
		t.Fatalf("empty datasets_json should yield no dataset: %#v", ds)
	}
	jmx := generateJMXFromUpsertData("plain", "http://node-app:3000/", "GET", "", 2, 30,
		json.RawMessage(csvSteps), nil, nil)
	if strings.Contains(jmx, "CSVDataSet") {
		t.Fatalf("plan without dataset must not declare CSVDataSet:\n%s", jmx)
	}
}

// Arrivals mode fans several thread groups out of one plan; the dataset belongs to the plan.
func TestArrivalsPlanEmitsSingleCSVDataSet(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "inline": "alice\nbob\n",
	}))
	segs := []arrivalSegment{
		{DelaySec: 0, RampSec: 10, Arrivals: 5},
		{DelaySec: 10, RampSec: 10, Arrivals: 5},
	}
	jmx := generateJMXArrivalsFromUpsert("arr", "http://node-app:3000/", "GET", "",
		json.RawMessage(csvSteps), segs, 20, ds, nil)
	if got := strings.Count(jmx, "<CSVDataSet"); got != 1 {
		t.Fatalf("want 1 CSVDataSet for all segments, got %d\n%s", got, jmx)
	}
	if strings.Count(jmx, "<ThreadGroup ") != 2 {
		t.Fatalf("want 2 arrival thread groups, got %d", strings.Count(jmx, "<ThreadGroup "))
	}
	if strings.Index(jmx, "<CSVDataSet") > strings.Index(jmx, "<ThreadGroup") {
		t.Fatal("CSVDataSet must precede the arrival thread groups")
	}
}

func TestDelimiterPropagatesToPlanAndDataFile(t *testing.T) {
	cases := []struct {
		name      string
		delimiter string
		inline    string
		wantProp  string
		wantFile  string
	}{
		{"semicolon", ";", "alice;secret1\nbob;secret2", ";", "alice;secret1\nbob;secret2\n"},
		{"pipe", "|", "alice|secret1", "|", "alice|secret1\n"},
		{"tab", "\t", "alice\tsecret1", `\t`, "alice\tsecret1\n"},
		{"named tab", "tab", "alice\tsecret1", `\t`, "alice\tsecret1\n"},
		{"default comma", "", "alice,secret1", ",", "alice,secret1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
				"variableNames": "user,pass", "delimiter": tc.delimiter, "inline": tc.inline,
			}))
			if ds == nil {
				t.Fatal("dataset expected")
			}
			jmx := generateJMXFromUpsertData("delim", "http://node-app:3000/", "GET", "", 2, 30,
				json.RawMessage(csvSteps), nil, ds)
			want := `<stringProp name="delimiter">` + tc.wantProp + `</stringProp>`
			if !strings.Contains(jmx, want) {
				t.Fatalf("plan missing %q\n%s", want, csvDataSetBlock(t, jmx))
			}
			content, _ := ds.workerCSV(0, 1)
			if content != tc.wantFile {
				t.Fatalf("data.csv = %q want %q", content, tc.wantFile)
			}
		})
	}
}

func TestInvalidDelimiterFallsBackWithWarning(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "delimiter": "::", "inline": "alice",
	}))
	if ds == nil || ds.Delimiter != "," {
		t.Fatalf("want comma fallback, got %#v", ds)
	}
	if len(ds.Warnings) == 0 || !strings.Contains(strings.Join(ds.Warnings, " "), "single character") {
		t.Fatalf("expected delimiter warning, got %#v", ds.Warnings)
	}
}

func TestInlineHeaderHandling(t *testing.T) {
	// Header row matching the declared names is data, not a row.
	withHeader := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass", "inline": "user,pass\nalice,secret1\nbob,secret2",
	}))
	if withHeader.rowCount() != 2 {
		t.Fatalf("header row should be dropped, rows=%d", withHeader.rowCount())
	}
	content, _ := withHeader.workerCSV(0, 1)
	if strings.HasPrefix(content, "user,pass") {
		t.Fatalf("data.csv must not carry the header:\n%s", content)
	}
	// No declared names → first row names the columns.
	derived := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"inline": "user,pass\nalice,secret1",
	}))
	if strings.Join(derived.columns(), ",") != "user,pass" || derived.rowCount() != 1 {
		t.Fatalf("derived columns=%v rows=%d", derived.columns(), derived.rowCount())
	}
	// Column names with no data rows cannot bind anything.
	empty := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass", "inline": "user,pass\n",
	}))
	if empty.emits() {
		t.Fatal("header-only inline CSV must not emit a CSVDataSet")
	}
}

func TestWorkerCSVShardsRowsAcrossWorkers(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "inline": "a\nb\nc\nd\ne",
	}))
	seen := map[string]bool{}
	for w := 0; w < 2; w++ {
		content, sharded := ds.workerCSV(w, 2)
		if !sharded {
			t.Fatalf("worker %d should get a shard", w)
		}
		for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
			if seen[line] {
				t.Fatalf("row %q handed to more than one worker", line)
			}
			seen[line] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("want all 5 rows across shards, got %d", len(seen))
	}
	// Fewer rows than workers: every worker keeps the full file rather than an empty one.
	small := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "inline": "a\nb",
	}))
	content, sharded := small.workerCSV(2, 4)
	if sharded || strings.TrimSpace(content) != "a\nb" {
		t.Fatalf("small dataset should not shard: %q sharded=%v", content, sharded)
	}
}

func TestOverriddenDatasetDefaults(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "inline": "alice",
		"recycle": false, "stop_thread": true, "share_mode": "thread", "quoted": false,
	}))
	jmx := generateJMXFromUpsertData("ovr", "http://node-app:3000/", "GET", "", 1, 10,
		json.RawMessage(csvSteps), nil, ds)
	block := csvDataSetBlock(t, jmx)
	for _, want := range []string{
		`<boolProp name="recycle">false</boolProp>`,
		`<boolProp name="stopThread">true</boolProp>`,
		`<boolProp name="quotedData">false</boolProp>`,
		`<stringProp name="shareMode">shareMode.thread</stringProp>`,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("missing %q\n%s", want, block)
		}
	}
}

func TestRecycleAndStopThreadAreExclusive(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "inline": "alice", "recycle": true, "stop_thread": true,
	}))
	if ds.StopThread {
		t.Fatal("stop_thread must be dropped when recycle is on")
	}
	if len(ds.Warnings) == 0 {
		t.Fatal("expected a warning about the conflict")
	}
}

// Plans stored before this fix (and raw imported JMX) must still get the element.
func TestInjectJMXCSVDataSetIntoStoredPlan(t *testing.T) {
	stored := generateJMXFromUpsertData("legacy", "http://node-app:3000/", "GET", "", 3, 30,
		json.RawMessage(csvSteps), nil, nil)
	if strings.Contains(stored, "CSVDataSet") {
		t.Fatal("fixture should start without a CSVDataSet")
	}
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass", "inline": "alice,secret1",
	}))
	injected, ok := injectJMXCSVDataSet(stored, ds)
	if !ok {
		t.Fatal("injection should report a change")
	}
	if strings.Index(injected, "<CSVDataSet") > strings.Index(injected, "<ThreadGroup") {
		t.Fatal("injected element must precede the ThreadGroup")
	}
	if !strings.Contains(injected, `<stringProp name="variableNames">user,pass</stringProp>`) {
		t.Fatalf("injected element malformed:\n%s", csvDataSetBlock(t, injected))
	}
	// Idempotent: never a second copy.
	again, ok := injectJMXCSVDataSet(injected, ds)
	if ok || strings.Count(again, "<CSVDataSet") != 1 {
		t.Fatalf("injection must be idempotent (ok=%v count=%d)", ok, strings.Count(again, "<CSVDataSet"))
	}
}

// The Dashboard posts the stored plan back on every save, so a delimiter or column edit must
// re-wire the plan instead of leaving it on the previous element.
func TestSyncJMXCSVDataSetRewiresStalePlan(t *testing.T) {
	oldDS := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass", "delimiter": ",", "inline": "alice,secret1",
	}))
	stored := generateJMXFromUpsertData("stale", "http://node-app:3000/", "GET", "", 2, 30,
		json.RawMessage(csvSteps), nil, oldDS)
	if !strings.Contains(stored, `<stringProp name="delimiter">,</stringProp>`) {
		t.Fatal("fixture should start comma-delimited")
	}
	newDS := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass,token", "delimiter": ";", "inline": "alice;secret1;t1",
	}))
	synced, changed := syncJMXCSVDataSet(stored, newDS)
	if !changed {
		t.Fatal("sync should report a change")
	}
	if strings.Count(synced, "<CSVDataSet") != 1 {
		t.Fatalf("want exactly one element, got %d\n%s", strings.Count(synced, "<CSVDataSet"), synced)
	}
	block := csvDataSetBlock(t, synced)
	if !strings.Contains(block, `<stringProp name="delimiter">;</stringProp>`) ||
		!strings.Contains(block, `<stringProp name="variableNames">user,pass,token</stringProp>`) {
		t.Fatalf("stale wiring survived:\n%s", block)
	}
	if strings.Contains(synced, "<hashTree/>\n      <hashTree/>") {
		t.Fatalf("orphan hashTree left behind:\n%s", synced)
	}
	// Still a well-formed plan the importer can read back.
	if _, _, err := parseJMXToScenario([]byte(synced), "stale"); err != nil {
		t.Fatalf("synced plan no longer parses: %v", err)
	}
	// A scenario with no dataset must not have its plan rewritten.
	if out, changed := syncJMXCSVDataSet(stored, nil); changed || out != stored {
		t.Fatal("nil dataset must leave the plan untouched")
	}
}

func TestStripJMXCSVDataSetsRemovesSiblingHashTree(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "inline": "alice",
	}))
	withDS := generateJMXFromUpsertData("s", "http://node-app:3000/", "GET", "", 1, 10,
		json.RawMessage(csvSteps), nil, ds)
	without := generateJMXFromUpsertData("s", "http://node-app:3000/", "GET", "", 1, 10,
		json.RawMessage(csvSteps), nil, nil)
	stripped, removed := stripJMXCSVDataSets(withDS)
	if !removed {
		t.Fatal("expected a removal")
	}
	if stripped != without {
		t.Fatalf("strip should restore the dataset-free plan\n got:\n%s\nwant:\n%s", stripped, without)
	}
}

// A plan pasted as raw JMX binds its own CSV columns; validate must not call them unbound.
func TestScenarioUnboundVariablesHonoursPlanCSVColumns(t *testing.T) {
	sc := map[string]interface{}{
		"steps_json": `[{"type":"http","name":"Get","url":"http://node-app:3000/${user}?t=${ghost}"}]`,
		"jmx_xml":    `<stringProp name="variableNames">user,pass</stringProp>`,
	}
	unbound, _ := scenarioUnboundVariables(sc)
	if strings.Join(unbound, ",") != "ghost" {
		t.Fatalf("unbound = %v (want ghost only)", unbound)
	}
}

// Import → edit → export must keep the dataset wired with the same attributes.
func TestImportExportRoundTripPreservesCSVDataSet(t *testing.T) {
	imported := `<?xml version="1.0" encoding="UTF-8"?>
<jmeterTestPlan version="1.2" properties="5.0" jmeter="5.5">
  <hashTree>
    <TestPlan guiclass="TestPlanGui" testclass="TestPlan" testname="rt" enabled="true"/>
    <hashTree>
      <CSVDataSet guiclass="TestBeanGUI" testclass="CSVDataSet" testname="Users" enabled="true">
        <stringProp name="filename">users.csv</stringProp>
        <stringProp name="fileEncoding">UTF-8</stringProp>
        <stringProp name="variableNames">user,pass</stringProp>
        <stringProp name="delimiter">;</stringProp>
        <boolProp name="ignoreFirstLine">true</boolProp>
        <boolProp name="quotedData">false</boolProp>
        <boolProp name="recycle">false</boolProp>
        <boolProp name="stopThread">true</boolProp>
        <stringProp name="shareMode">shareMode.group</stringProp>
      </CSVDataSet>
      <hashTree/>
      <ThreadGroup guiclass="ThreadGroupGui" testclass="ThreadGroup" testname="VUs" enabled="true">
        <stringProp name="ThreadGroup.num_threads">5</stringProp>
        <stringProp name="ThreadGroup.ramp_time">10</stringProp>
        <stringProp name="ThreadGroup.duration">60</stringProp>
        <elementProp name="ThreadGroup.main_controller" elementType="LoopController">
          <boolProp name="LoopController.continue_forever">true</boolProp>
          <stringProp name="LoopController.loops">-1</stringProp>
        </elementProp>
      </ThreadGroup>
      <hashTree>
        <HTTPSamplerProxy guiclass="HttpTestSampleGui" testclass="HTTPSamplerProxy" testname="Login" enabled="true">
          <stringProp name="HTTPSampler.domain">node-app</stringProp>
          <stringProp name="HTTPSampler.port">3000</stringProp>
          <stringProp name="HTTPSampler.protocol">http</stringProp>
          <stringProp name="HTTPSampler.path">/login?u=${user}</stringProp>
          <stringProp name="HTTPSampler.method">POST</stringProp>
        </HTTPSamplerProxy>
        <hashTree/>
      </hashTree>
    </hashTree>
  </hashTree>
</jmeterTestPlan>`
	sc, _, err := parseJMXToScenario([]byte(imported), "rt")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	dsJSON, _ := json.Marshal(sc["datasets"])
	ds := perfCSVDatasetFromJSON(string(dsJSON))
	if ds == nil {
		t.Fatalf("import lost the dataset: %s", dsJSON)
	}
	if ds.Filename != "users.csv" || strings.Join(ds.columns(), ",") != "user,pass" {
		t.Fatalf("import mangled filename/columns: %#v", ds)
	}
	if ds.Delimiter != ";" || ds.Recycle || !ds.StopThread || ds.QuotedData || !ds.IgnoreFirstLine {
		t.Fatalf("import lost flags: %#v", ds)
	}
	if ds.ShareMode != perfCSVShareGroup {
		t.Fatalf("share mode = %q", ds.ShareMode)
	}

	stepsJSON, _ := json.Marshal(sc["steps"])
	exported := generateJMXFromUpsertData("rt", "http://node-app:3000/login", "POST", "",
		int(asFloatOr(sc["vus"], 10)), int(asFloatOr(sc["duration_seconds"], 60)), stepsJSON, nil, ds)
	block := csvDataSetBlock(t, exported)
	t.Logf("round-tripped CSVDataSet:\n%s", block)
	for _, want := range []string{
		`<stringProp name="filename">users.csv</stringProp>`,
		`<stringProp name="variableNames">user,pass</stringProp>`,
		`<stringProp name="delimiter">;</stringProp>`,
		`<boolProp name="ignoreFirstLine">true</boolProp>`,
		`<boolProp name="quotedData">false</boolProp>`,
		`<boolProp name="recycle">false</boolProp>`,
		`<boolProp name="stopThread">true</boolProp>`,
		`<stringProp name="shareMode">shareMode.group</stringProp>`,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("export lost %q\n%s", want, block)
		}
	}
	// Second lap: re-importing the export yields the same dataset.
	sc2, _, err := parseJMXToScenario([]byte(exported), "rt2")
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	ds2JSON, _ := json.Marshal(sc2["datasets"])
	ds2 := perfCSVDatasetFromJSON(string(ds2JSON))
	if ds2 == nil {
		t.Fatalf("re-import lost the dataset: %s", ds2JSON)
	}
	if ds2.Filename != ds.Filename || ds2.Delimiter != ds.Delimiter || ds2.Recycle != ds.Recycle ||
		ds2.StopThread != ds.StopThread || ds2.QuotedData != ds.QuotedData ||
		ds2.IgnoreFirstLine != ds.IgnoreFirstLine || ds2.ShareMode != ds.ShareMode ||
		strings.Join(ds2.columns(), ",") != strings.Join(ds.columns(), ",") {
		t.Fatalf("round trip drifted:\n first=%#v\nsecond=%#v", ds, ds2)
	}
}

func TestTabDelimiterRoundTrip(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass", "delimiter": "\t", "inline": "alice\tsecret1",
	}))
	jmx := generateJMXFromUpsertData("tabs", "http://node-app:3000/", "GET", "", 1, 10,
		json.RawMessage(csvSteps), nil, ds)
	if !strings.Contains(jmx, `<stringProp name="delimiter">\t</stringProp>`) {
		t.Fatalf("tab must be written as the \\t escape:\n%s", csvDataSetBlock(t, jmx))
	}
	sc, _, err := parseJMXToScenario([]byte(jmx), "tabs")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	dsJSON, _ := json.Marshal(sc["datasets"])
	back := perfCSVDatasetFromJSON(string(dsJSON))
	if back == nil || back.Delimiter != "\t" {
		t.Fatalf("tab delimiter lost on import: %#v", back)
	}
}

func TestScenarioUnboundVariables(t *testing.T) {
	sc := map[string]interface{}{
		"target_url":    "http://node-app:3000/",
		"datasets_json": datasetsJSON(t, map[string]interface{}{"variableNames": "user,pass", "inline": "alice,secret1"}),
		"steps_json": `[
			{"type":"http","name":"Login","method":"POST","url":"http://node-app:3000/login?u=${user}",
			 "body":"{\"p\":\"${pass}\",\"cart\":\"${cart_id}\",\"missing\":\"${nowhere}\"}",
			 "headers":{"X-Run":"${LOAD_RUN_ID}","X-Prop":"${__P(OPA_THREADS,1)}","X-Ghost":"${alsomissing}"},
			 "children":[{"type":"extract","name":"cart","var":"cart_id","engine":"jsonpath","expression":"$.id"}]}
		]`,
	}
	unbound, resolvable := scenarioUnboundVariables(sc)
	if !resolvable {
		t.Fatal("columns are known here")
	}
	if strings.Join(unbound, ",") != "alsomissing,nowhere" {
		t.Fatalf("unbound = %v (want alsomissing,nowhere)", unbound)
	}
}

func TestScenarioUnboundVariablesCleanScenario(t *testing.T) {
	sc := map[string]interface{}{
		"target_url":    "http://node-app:3000/${user}",
		"datasets_json": datasetsJSON(t, map[string]interface{}{"variableNames": "user", "inline": "alice"}),
		"steps_json":    `[{"type":"foreach","name":"each","input_var":"ids","return_var":"id","children":[{"type":"http","name":"Get","url":"http://node-app:3000/items/${id}?u=${user}"}]}]`,
	}
	unbound, resolvable := scenarioUnboundVariables(sc)
	if !resolvable || len(unbound) != 0 {
		t.Fatalf("expected no unbound tokens, got %v", unbound)
	}
}

// Raw JMX may bind its own Arguments / User Defined Variables — those are not unbound.
func TestScenarioUnboundVariablesHonoursJMXArguments(t *testing.T) {
	sc := map[string]interface{}{
		"steps_json": `[{"type":"http","name":"Get","url":"http://${host}:3000/${page}"}]`,
		"jmx_xml":    `<stringProp name="Argument.name">host</stringProp>`,
	}
	unbound, _ := scenarioUnboundVariables(sc)
	if strings.Join(unbound, ",") != "page" {
		t.Fatalf("unbound = %v (want page only)", unbound)
	}
}

// An external CSV keeps its column names on the runner, so nothing can be asserted.
func TestScenarioUnboundVariablesUnresolvableExternalCSV(t *testing.T) {
	sc := map[string]interface{}{
		"datasets_json": datasetsJSON(t, map[string]interface{}{"filename": "/data/users.csv"}),
		"steps_json":    `[{"type":"http","name":"Get","url":"http://node-app:3000/${user}"}]`,
	}
	unbound, resolvable := scenarioUnboundVariables(sc)
	if resolvable {
		t.Fatal("external CSV columns are not resolvable here")
	}
	if len(unbound) != 0 {
		t.Fatalf("must not guess unbound tokens: %v", unbound)
	}
}

func TestUnboundVariableTriageEntry(t *testing.T) {
	entry := unboundVariableTriage([]string{"user", "token"})
	if entry["severity"] != "unbound_variable" || entry["ok"] != false {
		t.Fatalf("triage entry = %#v", entry)
	}
	if !strings.Contains(entry["error"].(string), "user") || !strings.Contains(entry["error"].(string), "token") {
		t.Fatalf("error should name the tokens: %#v", entry["error"])
	}
	if hint, _ := entry["hint"].(string); !strings.Contains(hint, "variableNames") {
		t.Fatalf("hint should point at the fix: %q", hint)
	}
}

func TestValidateSeedRowFromDataset(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user,pass", "inline": "alice,secret1\nbob,secret2",
	}))
	row := ds.firstRow()
	if row["user"] != "alice" || row["pass"] != "secret1" {
		t.Fatalf("seed row = %#v", row)
	}
	if got := expandPerfVars("http://node-app:3000/login?u=${user}", row); got != "http://node-app:3000/login?u=alice" {
		t.Fatalf("dry run should substitute dataset values, got %q", got)
	}
}

func TestDatasetSummaryReportsWiring(t *testing.T) {
	ds := perfCSVDatasetFromJSON(datasetsJSON(t, map[string]interface{}{
		"variableNames": "user", "delimiter": ";", "inline": "alice\nbob",
	}))
	sum := ds.summary()
	if sum["emitted"] != true || sum["rows"] != 2 || sum["delimiter"] != ";" || sum["filename"] != perfCSVDataFile {
		t.Fatalf("summary = %#v", sum)
	}
	if sum["honesty"] == "" {
		t.Fatal("summary must keep the runner-local honesty caveat")
	}
}
