package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// loginFragmentSteps is a VU tree with one reusable journey and one reference to it.
func loginFragmentSteps() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "fragment", "name": "LoginJourney",
			"children": []interface{}{
				map[string]interface{}{
					"type": "http", "name": "Login", "method": "POST",
					"url": "http://node-app:3000/login", "body": `{"u":"${user}"}`,
					"children": []interface{}{
						map[string]interface{}{
							"type": "extract", "name": "tok", "var": "token",
							"engine": "jsonpath", "expression": "$.token",
						},
					},
				},
			},
		},
		{"type": "include", "name": "UseLogin", "ref": "LoginJourney"},
		{"type": "http", "name": "Home", "method": "GET", "url": "http://node-app:3000/"},
	}
}

func mustJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// importedSteps normalises the nested steps a JMX import returns.
func importedSteps(t *testing.T, sc map[string]interface{}) []map[string]interface{} {
	t.Helper()
	if steps, ok := sc["steps"].([]map[string]interface{}); ok {
		return steps
	}
	var out []map[string]interface{}
	blob, _ := json.Marshal(sc["steps"])
	if json.Unmarshal(blob, &out) != nil {
		t.Fatalf("steps not a step list: %#v", sc["steps"])
	}
	return out
}

func findStep(list []map[string]interface{}, typ, name string) map[string]interface{} {
	for _, s := range list {
		if fmt.Sprint(s["type"]) == typ && (name == "" || fmt.Sprint(s["name"]) == name) {
			return s
		}
	}
	return nil
}

func TestFragmentEmittedAsDisabledTestFragmentContainer(t *testing.T) {
	jmx := generateJMXFromUpsert("shop", "http://node-app:3000/", "GET", "", 4, 60,
		mustJSON(t, loginFragmentSteps()))

	if !strings.Contains(jmx, `<TestFragmentController guiclass="TestFragmentControllerGui" testclass="TestFragmentController" testname="LoginJourney" enabled="false">`) {
		t.Fatalf("want a disabled TestFragmentController container named LoginJourney\n%s", jmx)
	}
	// The container is a Test Plan level sibling of the thread group, so every thread
	// group shares one copy rather than each holding its own.
	fragIdx := strings.Index(jmx, "<TestFragmentController")
	tgIdx := strings.Index(jmx, "<ThreadGroup ")
	if fragIdx < 0 || tgIdx < 0 || fragIdx > tgIdx {
		t.Fatalf("fragment container must precede the thread group (frag=%d tg=%d)\n%s", fragIdx, tgIdx, jmx)
	}
	// Its journey lives inside it.
	fragTree := jmx[fragIdx:tgIdx]
	if !strings.Contains(fragTree, `testname="Login"`) || !strings.Contains(fragTree, "JSONPostProcessor") {
		t.Fatalf("fragment container should hold the Login sampler and its extractor\n%s", fragTree)
	}
}

func TestModuleReferenceResolvesToFragmentContainer(t *testing.T) {
	jmx := generateJMXFromUpsert("shop", "http://node-app:3000/", "GET", "", 4, 60,
		mustJSON(t, loginFragmentSteps()))

	if !strings.Contains(jmx, `<ModuleController guiclass="ModuleControllerGui" testclass="ModuleController" testname="UseLogin" enabled="true">`) {
		t.Fatalf("want a ModuleController for the reference\n%s", jmx)
	}
	// The node path names the Test Plan node then the fragment container, which is how
	// JMeter resolves the target.
	pathStart := strings.Index(jmx, `<collectionProp name="ModuleController.node_path">`)
	if pathStart < 0 {
		t.Fatalf("no node path emitted\n%s", jmx)
	}
	pathEnd := strings.Index(jmx[pathStart:], "</collectionProp>")
	nodePath := jmx[pathStart : pathStart+pathEnd]
	planEntry := fmt.Sprintf(`<stringProp name="%d">shop</stringProp>`, javaStringHashCode("shop"))
	fragEntry := fmt.Sprintf(`<stringProp name="%d">LoginJourney</stringProp>`, javaStringHashCode("LoginJourney"))
	// JMeter skips entry 0 (the tree root) and starts matching at the Test Plan node, so
	// the path must be three deep for a container that sits under the Test Plan. Two
	// entries would shift the match and resolve to nothing.
	if got := strings.Count(nodePath, "<stringProp"); got != 3 {
		t.Fatalf("want a 3 deep node path (root, plan, fragment), got %d\n%s", got, nodePath)
	}
	if got := strings.Count(nodePath, planEntry); got != 2 {
		t.Fatalf("plan node must occupy the skipped root entry and the matched entry, got %d\n%s", got, nodePath)
	}
	if !strings.Contains(nodePath, fragEntry) {
		t.Fatalf("node path does not end at the fragment container\n%s", nodePath)
	}
	if strings.LastIndex(nodePath, planEntry) > strings.Index(nodePath, fragEntry) {
		t.Fatalf("fragment must be the last node path entry\n%s", nodePath)
	}
	// Both ends of the reference agree on the plan node name.
	if !strings.Contains(jmx, `<TestPlan guiclass="TestPlanGui" testclass="TestPlan" testname="shop"`) {
		t.Fatalf("plan node name must match node_path[0]\n%s", jmx)
	}
	// A module reference is a leaf.
	modIdx := strings.Index(jmx, "<ModuleController")
	after := jmx[modIdx:]
	closeIdx := strings.Index(after, "</ModuleController>")
	if !strings.HasPrefix(strings.TrimSpace(after[closeIdx+len("</ModuleController>"):]), "<hashTree/>") {
		t.Fatalf("ModuleController must be followed by an empty hashTree\n%s", after[:closeIdx+200])
	}
	// The referenced steps are not copied next to the reference.
	if got := strings.Count(jmx, `testname="Login"`); got != 1 {
		t.Fatalf("want one Login sampler (in the fragment only), got %d\n%s", got, jmx)
	}
}

func TestFragmentModuleRoundTripImportEditExport(t *testing.T) {
	first := generateJMXFromUpsert("shop", "http://node-app:3000/", "GET", "", 4, 60,
		mustJSON(t, loginFragmentSteps()))

	sc, _, err := parseJMXToScenario([]byte(first), "shop")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	steps := importedSteps(t, sc)

	frag := findStep(steps, "fragment", "LoginJourney")
	if frag == nil {
		t.Fatalf("import lost the fragment definition: %#v", steps)
	}
	fragKids := stepChildren(frag)
	if len(fragKids) != 1 || fmt.Sprint(fragKids[0]["name"]) != "Login" {
		t.Fatalf("fragment children lost: %#v", fragKids)
	}
	ref := findStep(steps, "include", "UseLogin")
	if ref == nil {
		t.Fatalf("import lost the module reference: %#v", steps)
	}
	if fmt.Sprint(ref["ref"]) != "LoginJourney" {
		t.Fatalf("reference lost its target: %#v", ref)
	}

	// Edit the reused journey in one place, then export again.
	fragKids[0]["url"] = "http://node-app:3000/login-v2"
	frag["children"] = fragKids
	second := generateJMXFromUpsert("shop", "http://node-app:3000/", "GET", "", 4, 60, mustJSON(t, steps))

	for _, want := range []string{
		`<TestFragmentController`, `testname="LoginJourney"`, `enabled="false"`,
		`<ModuleController`, `testname="UseLogin"`,
		`<stringProp name="HTTPSampler.path">/login-v2</stringProp>`,
	} {
		if !strings.Contains(second, want) {
			t.Fatalf("round trip lost %q\n%s", want, second)
		}
	}
	if got := strings.Count(second, "<TestFragmentController"); got != 1 {
		t.Fatalf("round trip duplicated the fragment container (%d)\n%s", got, second)
	}
	if got := strings.Count(second, "<ModuleController"); got != 1 {
		t.Fatalf("round trip duplicated the module reference (%d)\n%s", got, second)
	}
	// The edit reached the plan exactly once — one definition, not per-caller copies.
	if got := strings.Count(second, "/login-v2"); got != 1 {
		t.Fatalf("want the edited path once, got %d\n%s", got, second)
	}
}

func TestFragmentParamsScopeAndRoundTrip(t *testing.T) {
	steps := loginFragmentSteps()
	steps = append(steps, map[string]interface{}{
		"type": "include", "name": "UseLoginAsBob", "ref": "LoginJourney",
		"params": map[string]interface{}{"user": "bob", "tier": "gold"},
	})
	jmx := generateJMXFromUpsert("shop", "http://node-app:3000/", "GET", "", 4, 60, mustJSON(t, steps))

	for _, want := range []string{
		`<GenericController guiclass="LogicControllerGui" testclass="GenericController" testname="UseLoginAsBob" enabled="true">`,
		`<stringProp name="opl.module_params">true</stringProp>`,
		`<UserParameters guiclass="UserParametersGui" testclass="UserParameters" testname="Fragment inputs: LoginJourney" enabled="true">`,
		`<collectionProp name="UserParameters.names">`,
		`>tier</stringProp>`, `>user</stringProp>`,
		`>gold</stringProp>`, `>bob</stringProp>`,
		`<collectionProp name="UserParameters.thread_values">`,
		// false keeps it a pre-processor, re-applied per sampler in this reference's
		// scope; true would fire at thread-iteration start and let one reference's
		// values win for every reference.
		`<boolProp name="UserParameters.per_iteration">false</boolProp>`,
	} {
		if !strings.Contains(jmx, want) {
			t.Fatalf("JMX missing %q\n%s", want, jmx)
		}
	}
	// One fragment, two references with different inputs: still a single definition.
	if got := strings.Count(jmx, "<TestFragmentController"); got != 1 {
		t.Fatalf("want one fragment container, got %d", got)
	}
	if got := strings.Count(jmx, "<ModuleController"); got != 2 {
		t.Fatalf("want two module references, got %d", got)
	}
	// The inputs sit inside the wrapper that scopes them to this reference only.
	wrap := strings.Index(jmx, `testname="UseLoginAsBob"`)
	up := strings.Index(jmx, "<UserParameters")
	mod := strings.LastIndex(jmx, "<ModuleController")
	if wrap < 0 || up < wrap || mod < up {
		t.Fatalf("expected wrapper → UserParameters → ModuleController order (%d/%d/%d)\n%s", wrap, up, mod, jmx)
	}

	sc, _, err := parseJMXToScenario([]byte(jmx), "shop")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	imported := importedSteps(t, sc)
	bob := findStep(imported, "include", "UseLoginAsBob")
	if bob == nil {
		t.Fatalf("import lost the parameterised reference: %#v", imported)
	}
	params, ok := bob["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("import lost the fragment inputs: %#v", bob)
	}
	if fmt.Sprint(params["user"]) != "bob" || fmt.Sprint(params["tier"]) != "gold" {
		t.Fatalf("inputs wrong after round trip: %#v", params)
	}
	if fmt.Sprint(bob["ref"]) != "LoginJourney" {
		t.Fatalf("parameterised reference lost its target: %#v", bob)
	}
}

func TestFragmentReferenceFallbackIsReportedInHonestyText(t *testing.T) {
	// Three references the emitter cannot turn into module references, one it can.
	steps := []map[string]interface{}{
		{"type": "fragment", "name": "Shared", "children": []interface{}{
			map[string]interface{}{"type": "http", "name": "Ping", "method": "GET", "url": "http://node-app:3000/ping"},
		}},
		{"type": "fragment", "name": "Twin", "children": []interface{}{
			map[string]interface{}{"type": "http", "name": "A", "method": "GET", "url": "http://node-app:3000/a"},
		}},
		{"type": "fragment", "name": "Twin", "children": []interface{}{
			map[string]interface{}{"type": "http", "name": "B", "method": "GET", "url": "http://node-app:3000/b"},
		}},
		{"type": "include", "name": "UseShared", "ref": "Shared"},
		{"type": "include", "name": "UseTwin", "ref": "Twin"},
		{"type": "include", "name": "UseOverride", "ref": "Shared", "children": []interface{}{
			map[string]interface{}{"type": "http", "name": "Own", "method": "GET", "url": "http://node-app:3000/own"},
		}},
		{"type": "include", "name": "UseGone", "ref": "NotThere"},
	}
	refs := perfFragmentRefs(steps, "shop")
	if len(refs) != 4 {
		t.Fatalf("want 4 references, got %d: %#v", len(refs), refs)
	}
	byStep := map[string]perfFragmentRef{}
	for _, r := range refs {
		byStep[r.Step] = r
	}
	if got := byStep["UseShared"].Mode; got != perfFragmentModeModule {
		t.Fatalf("UseShared mode=%s", got)
	}
	if got := byStep["UseTwin"].Mode; got != perfFragmentModeInline {
		t.Fatalf("duplicate fragment name should force inline expansion, got %s", got)
	}
	if got := byStep["UseOverride"].Mode; got != perfFragmentModeInline {
		t.Fatalf("reference with its own children should stay inline, got %s", got)
	}
	if got := byStep["UseGone"].Mode; got != perfFragmentModeUnresolved {
		t.Fatalf("missing fragment should be unresolved, got %s", got)
	}

	// The honesty text names every fallback, so a plan that quietly stopped being a
	// reference plan cannot be mistaken for one.
	honesty := perfFragmentRefsHonesty(refs)
	for _, want := range []string{
		"Fragment references (4)",
		"1 reuse a shared test fragment",
		"UseShared → module reference to fragment \"Shared\"",
		"fell back to inline expansion",
		"UseTwin → inlined copy",
		"2 fragments share the name Twin",
		"UseOverride → inlined copy",
		"reference carries its own children",
		"could not be resolved",
		"UseGone → not emitted",
		"no fragment named NotThere",
	} {
		if !strings.Contains(honesty, want) {
			t.Fatalf("honesty text missing %q\ngot: %s", want, honesty)
		}
	}

	// And the emitted plan matches what was reported: inline copies, not references.
	jmx := generateJMXFromUpsert("shop", "http://node-app:3000/", "GET", "", 2, 30, mustJSON(t, steps))
	if got := strings.Count(jmx, "<ModuleController"); got != 1 {
		t.Fatalf("want exactly the one resolvable reference as a module, got %d\n%s", got, jmx)
	}
	for _, want := range []string{
		"mode=" + perfFragmentModeInline,
		"mode=" + perfFragmentModeUnresolved,
		`testname="Own"`,
	} {
		if !strings.Contains(jmx, want) {
			t.Fatalf("JMX missing %q\n%s", want, jmx)
		}
	}
	// The ambiguous reference really did inline one of the twins.
	if !strings.Contains(jmx, `testname="B"`) && !strings.Contains(jmx, `testname="A"`) {
		t.Fatalf("ambiguous reference emitted nothing\n%s", jmx)
	}
}

func TestRendezvousStepEmitsSyncTimerWithGroupSize(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "transaction", "name": "Spike", "children": []interface{}{
			map[string]interface{}{"type": "rendezvous", "name": "AllTogether", "group_size": 40, "timeout_ms": 5000},
			map[string]interface{}{"type": "http", "name": "Checkout", "method": "POST", "url": "http://node-app:3000/checkout"},
		}},
	}
	jmx := generateJMXFromUpsert("burst", "http://node-app:3000/", "GET", "", 40, 60, mustJSON(t, steps))

	// Property names are the TestBean ones JMeter reads: groupSize and timeoutInMs.
	// timeoutInMilliseconds would be ignored and leave the burst on wait-forever.
	want := `<SyncTimer guiclass="TestBeanGUI" testclass="SyncTimer" testname="AllTogether" enabled="true">
            <intProp name="groupSize">40</intProp>
            <longProp name="timeoutInMs">5000</longProp>
          </SyncTimer>
          <hashTree/>`
	if !strings.Contains(jmx, want) {
		t.Fatalf("want the synchronising timer with the configured group size; got\n%s", jmx)
	}
	// Inside the transaction it gates, ahead of the sampler.
	tx := strings.Index(jmx, `testname="Spike"`)
	timer := strings.Index(jmx, "<SyncTimer")
	sampler := strings.Index(jmx, `testname="Checkout"`)
	if tx < 0 || timer < tx || sampler < timer {
		t.Fatalf("timer must sit inside the transaction before the sampler (%d/%d/%d)", tx, timer, sampler)
	}

	sc, _, err := parseJMXToScenario([]byte(jmx), "burst")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	imported := importedSteps(t, sc)
	txn := findStep(imported, "transaction", "Spike")
	if txn == nil {
		t.Fatalf("import lost the transaction: %#v", imported)
	}
	rv := findStep(stepChildren(txn), "rendezvous", "AllTogether")
	if rv == nil {
		t.Fatalf("import lost the rendezvous: %#v", stepChildren(txn))
	}
	if int(asFloatOr(rv["group_size"], 0)) != 40 || int(asFloatOr(rv["timeout_ms"], 0)) != 5000 {
		t.Fatalf("rendezvous settings lost: %#v", rv)
	}
}

func TestPlanLevelRendezvousGatesFirstRequest(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "transaction", "name": "Journey", "children": []interface{}{
			map[string]interface{}{"type": "http", "name": "First", "method": "GET", "url": "http://node-app:3000/a"},
			map[string]interface{}{"type": "http", "name": "Second", "method": "GET", "url": "http://node-app:3000/b"},
		}},
	}
	sched := map[string]interface{}{"rendezvous": map[string]interface{}{"group_size": 12, "timeout_ms": 2000}}
	jmx := generateJMXFromUpsertEx("burst", "http://node-app:3000/", "GET", "", 12, 60, mustJSON(t, steps), sched)

	if got := strings.Count(jmx, "<SyncTimer"); got != 1 {
		t.Fatalf("want one plan-level burst, got %d\n%s", got, jmx)
	}
	if !strings.Contains(jmx, `<intProp name="groupSize">12</intProp>`) {
		t.Fatalf("group size not carried into the plan\n%s", jmx)
	}
	// Scoped to the first sampler, so the population starts the journey together
	// instead of every request becoming a barrier.
	first := strings.Index(jmx, `testname="First"`)
	timer := strings.Index(jmx, "<SyncTimer")
	second := strings.Index(jmx, `testname="Second"`)
	if first < 0 || timer < first || second < timer {
		t.Fatalf("timer must be scoped inside the first sampler (%d/%d/%d)\n%s", first, timer, second, jmx)
	}

	_, placement := injectPlanRendezvous(steps, perfRendezvousFromSchedule(sched))
	if placement.Mode != "sampler" || placement.Step != "First" {
		t.Fatalf("placement=%#v", placement)
	}

	// Shorthand form is read too.
	short := perfRendezvousFromSchedule(map[string]interface{}{"rendezvous_group_size": float64(7)})
	if short == nil || short.GroupSize != 7 {
		t.Fatalf("shorthand rendezvous not read: %#v", short)
	}
}

func TestPlanLevelRendezvousFallsBackToThreadGroupAndSaysSo(t *testing.T) {
	// A journey made only of module references has no request of its own to gate.
	steps := []map[string]interface{}{
		{"type": "fragment", "name": "Shared", "children": []interface{}{
			map[string]interface{}{"type": "http", "name": "Ping", "method": "GET", "url": "http://node-app:3000/ping"},
		}},
		{"type": "include", "name": "UseShared", "ref": "Shared"},
	}
	rv := &perfRendezvous{Name: "Burst", GroupSize: 5}
	out, placement := injectPlanRendezvous(steps, rv)
	if placement.Mode != "thread_group" {
		t.Fatalf("placement=%#v", placement)
	}
	if placement.Note == "" || !strings.Contains(placement.Note, "every sampler in the journey becomes a barrier") {
		t.Fatalf("fallback must say what changed: %q", placement.Note)
	}
	if len(out) != len(steps) {
		t.Fatalf("tree should be untouched on fallback: %#v", out)
	}

	sched := map[string]interface{}{"rendezvous": map[string]interface{}{"group_size": 5}}
	jmx := generateJMXFromUpsertEx("burst", "http://node-app:3000/", "GET", "", 5, 30, mustJSON(t, steps), sched)
	tg := strings.Index(jmx, "<ThreadGroup ")
	timer := strings.Index(jmx, "<SyncTimer")
	mod := strings.Index(jmx, "<ModuleController")
	if tg < 0 || timer < tg || mod < timer {
		t.Fatalf("thread-group level timer expected before the reference (%d/%d/%d)\n%s", tg, timer, mod, jmx)
	}
}

func TestRendezvousTriageReportsUnfillableGroup(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "rendezvous", "name": "TooBig", "group_size": 100},
		{"type": "rendezvous", "name": "TooBigTimed", "group_size": 100, "timeout_ms": 1500},
		{"type": "rendezvous", "name": "AllThreads", "group_size": 0},
		{"type": "rendezvous", "name": "Fine", "group_size": 5},
	}
	msgs := perfRendezvousTriage(steps, 10)
	if len(msgs) != 2 {
		t.Fatalf("want 2 warnings, got %d: %#v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "TooBig") || !strings.Contains(msgs[0], "parks for the whole run") {
		t.Fatalf("no-timeout deadlock not reported: %q", msgs[0])
	}
	if !strings.Contains(msgs[1], "1500ms timeout") {
		t.Fatalf("timeout case not reported: %q", msgs[1])
	}
	if len(perfRendezvousTriage(steps, 0)) != 0 {
		t.Fatal("unknown VU count cannot claim a group is unfillable")
	}
}

func TestUnresolvedReferenceReachesValidateAsAFailure(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "include", "name": "UseGone", "ref": "NotThere"},
		{"type": "http", "name": "Home", "method": "GET", "url": "http://node-app:3000/"},
	}
	flat := flattenScenarioSteps(steps)
	miss := findStep(flat, "include", "UseGone")
	if miss == nil {
		t.Fatalf("unresolved reference vanished from the dry run: %#v", flat)
	}
	if ok, _ := miss["ok"].(bool); ok {
		t.Fatalf("unresolved reference must not report ok: %#v", miss)
	}
	if !strings.Contains(fmt.Sprint(miss["error"]), "NotThere") {
		t.Fatalf("error should name the missing fragment: %#v", miss)
	}
}

func TestFragmentInputsBindTokensForValidate(t *testing.T) {
	steps := []map[string]interface{}{
		{"type": "fragment", "name": "Shared", "children": []interface{}{
			map[string]interface{}{"type": "http", "name": "Ping", "method": "GET",
				"url": "http://node-app:3000/ping?u=${user}"},
		}},
		{"type": "include", "name": "UseShared", "ref": "Shared",
			"params": map[string]interface{}{"user": "alice"}},
	}
	stepsJSON, _ := json.Marshal(steps)
	sc := map[string]interface{}{
		"name": "shop", "target_url": "http://node-app:3000/",
		"steps_json": string(stepsJSON), "datasets_json": "{}",
	}
	unbound, resolvable := scenarioUnboundVariables(sc)
	if !resolvable {
		t.Fatal("no dataset in play, tokens should be checkable")
	}
	if len(unbound) != 0 {
		t.Fatalf("fragment inputs bind ${user}; got unbound %#v", unbound)
	}

	// The dry run sees the inputs before the reused journey runs.
	flat := flattenScenarioSteps(steps)
	if len(flat) < 2 || fmt.Sprint(flat[0]["type"]) != "params" {
		t.Fatalf("inputs should be applied ahead of the fragment steps: %#v", flat)
	}
	if fmt.Sprint(flat[1]["name"]) != "Ping" {
		t.Fatalf("fragment steps should follow its inputs: %#v", flat)
	}
}

func TestJavaStringHashCodeMatchesJMeterNaming(t *testing.T) {
	// Values checked against java.lang.String.hashCode.
	for _, tc := range []struct {
		in   string
		want int32
	}{
		{"", 0},
		{"user", 3599307},
		{"Test Plan", 764597751},
		{"LoginJourney", -1562168265},
	} {
		if got := javaStringHashCode(tc.in); got != tc.want {
			t.Fatalf("javaStringHashCode(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
}

func TestFragmentContainerSharedAcrossArrivalThreadGroups(t *testing.T) {
	segs := []arrivalSegment{
		{DelaySec: 0, RampSec: 10, Arrivals: 5},
		{DelaySec: 10, RampSec: 10, Arrivals: 5},
	}
	jmx := generateJMXArrivalsFromUpsert("arr", "http://node-app:3000/", "GET", "",
		mustJSON(t, loginFragmentSteps()), segs, 20, nil, nil)

	if got := strings.Count(jmx, "<ThreadGroup "); got != 2 {
		t.Fatalf("want 2 arrival thread groups, got %d", got)
	}
	// One definition at plan level, one reference per thread group.
	if got := strings.Count(jmx, "<TestFragmentController"); got != 1 {
		t.Fatalf("fragment must not be copied per segment, got %d\n%s", got, jmx)
	}
	if got := strings.Count(jmx, "<ModuleController"); got != 2 {
		t.Fatalf("want one module reference per segment, got %d", got)
	}
	if strings.Index(jmx, "<TestFragmentController") > strings.Index(jmx, "<ThreadGroup ") {
		t.Fatalf("fragment container must precede the thread groups\n%s", jmx)
	}
}

func TestFragmentPlanPassesUnsafeElementScreen(t *testing.T) {
	steps := loginFragmentSteps()
	steps = append(steps,
		map[string]interface{}{"type": "rendezvous", "name": "Burst", "group_size": 4},
		map[string]interface{}{"type": "include", "name": "UseLoginAsBob", "ref": "LoginJourney",
			"params": map[string]interface{}{"user": "bob"}},
	)
	jmx := generateJMXFromUpsert("shop", "http://node-app:3000/", "GET", "", 4, 60, mustJSON(t, steps))
	if jmxContainsUnsafeElements(jmx) {
		t.Fatalf("module/fragment/timer elements must not trip the unsafe screen\n%s", jmx)
	}
}
