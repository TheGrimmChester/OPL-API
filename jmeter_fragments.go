package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
)

// Reusable journey modules and synchronised bursts.
//
// A `fragment` step in the VU tree is a definition, never part of the executed flow.
// The emitter hoists every fragment to Test Plan level as a disabled
// TestFragmentController container and turns each `include`/`link` step into a
// ModuleController whose node path points at that container. One journey piece is then
// stored once and reused by reference: editing the fragment changes every caller.
//
// When a reference cannot be pointed at a container — no fragment carries that name, the
// name is ambiguous because two fragments share it, or the reference carries its own
// children — the emitter falls back to expanding the referenced steps inline, which is
// the pre-module behaviour. The two modes do not behave the same: a module reference
// keeps a single definition, an inline expansion is a copy that drifts from it. Which
// mode was used is therefore reported per reference in validate and upsert output rather
// than implied, so a plan can never quietly stop being the plan that was designed.
const (
	perfFragmentModeModule     = "module_reference"
	perfFragmentModeInline     = "inline_expansion"
	perfFragmentModeUnresolved = "unresolved"
)

// perfFragmentRef is what the emitter decided for one include/link step.
type perfFragmentRef struct {
	Step     string   `json:"step"`
	Ref      string   `json:"ref"`
	Mode     string   `json:"mode"`
	NodePath []string `json:"node_path,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Params   []string `json:"params,omitempty"`
}

// perfPlanNodeName is the Test Plan node name. It is the first element of every
// ModuleController node path, so writeJMXPlanOpen and the module emitter must agree on
// it — both go through here.
func perfPlanNodeName(name string) string {
	return nz(name, "OPA Plan")
}

// perfStepType reads a step's type, defaulting to http like the rest of the engine.
func perfStepType(step map[string]interface{}) string {
	typ := strings.TrimSpace(fmt.Sprint(step["type"]))
	if typ == "" || typ == "<nil>" {
		return "http"
	}
	return typ
}

// perfStepName reads a step's name, empty when unset.
func perfStepName(step map[string]interface{}) string {
	name := strings.TrimSpace(fmt.Sprint(step["name"]))
	if name == "<nil>" {
		return ""
	}
	return name
}

// perfIncludeRef resolves which fragment an include/link step names: explicit `ref`,
// then legacy `fragment`, then the step's own name.
func perfIncludeRef(step map[string]interface{}) string {
	for _, key := range []string{"ref", "fragment"} {
		v := strings.TrimSpace(fmt.Sprint(step[key]))
		if v != "" && v != "<nil>" {
			return v
		}
	}
	return perfStepName(step)
}

// perfFragmentDefs collects every fragment definition in a VU tree, depth-first.
func perfFragmentDefs(steps []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	var walk func([]map[string]interface{})
	walk = func(list []map[string]interface{}) {
		for _, s := range list {
			if perfStepType(s) == "fragment" {
				out = append(out, s)
			}
			walk(stepChildren(s))
		}
	}
	walk(steps)
	return out
}

// perfFragmentNameCounts counts fragment definitions per name so an ambiguous
// reference can be detected instead of silently binding to one of them.
func perfFragmentNameCounts(steps []map[string]interface{}) map[string]int {
	counts := map[string]int{}
	for _, f := range perfFragmentDefs(steps) {
		if name := perfStepName(f); name != "" {
			counts[name]++
		}
	}
	return counts
}

// perfStepParams reads a step's `params` map (fragment inputs) as sorted names.
func perfStepParams(step map[string]interface{}) ([]string, map[string]string) {
	raw, ok := step["params"]
	if !ok || raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, nil
	}
	vals := map[string]string{}
	names := make([]string, 0, len(m))
	for k, v := range m {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s := fmt.Sprint(v)
		if s == "<nil>" {
			s = ""
		}
		names = append(names, k)
		vals[k] = s
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	return names, vals
}

// perfFragmentRefDecision is the single place that decides how one include/link step is
// emitted. Both the JMX emitter and the validate/upsert reporting call it, so the
// reported mode is the mode that was emitted rather than a second guess at it.
func perfFragmentRefDecision(step map[string]interface{}, planName string,
	frags map[string]map[string]interface{}, counts map[string]int) perfFragmentRef {

	ref := perfIncludeRef(step)
	names, _ := perfStepParams(step)
	out := perfFragmentRef{
		Step: nz(perfStepName(step), ref), Ref: ref, Params: names,
	}
	switch {
	case ref == "":
		out.Mode = perfFragmentModeUnresolved
		out.Reason = "reference names no fragment (set ref)"
	case len(stepChildren(step)) > 0:
		out.Mode = perfFragmentModeInline
		out.Reason = "reference carries its own children — those steps are expanded inline instead of pointing at the fragment"
	case frags[ref] == nil:
		out.Mode = perfFragmentModeUnresolved
		out.Reason = "no fragment named " + ref + " in this scenario"
	case counts[ref] > 1:
		out.Mode = perfFragmentModeInline
		out.Reason = fmt.Sprintf("%d fragments share the name %s, so a module node path cannot identify one — steps expanded inline", counts[ref], ref)
	default:
		out.Mode = perfFragmentModeModule
		out.NodePath = perfModuleNodePath(planName, ref)
	}
	return out
}

// perfModuleNodePath is the node path JMeter resolves a module reference through: the
// chain of node names from the tree root down to the target.
//
// The first entry is the tree root and is skipped — JMeter's traversal starts matching at
// index 1 — but it must be present or the whole path shifts by one and the reference
// silently resolves to nothing. JMeter itself writes the Test Plan node twice there
// (root and Test Plan wrap the same element), and Apache's own regression plans confirm
// the shape: root, Test Plan name, then each node down to the target. A fragment
// container sits directly under the Test Plan, so the path is three deep.
func perfModuleNodePath(planName, fragment string) []string {
	plan := perfPlanNodeName(planName)
	return []string{plan, plan, fragment}
}

// perfFragmentRefs reports every include/link step in a VU tree, in emit order.
func perfFragmentRefs(steps []map[string]interface{}, planName string) []perfFragmentRef {
	frags := indexFragmentsByName(steps)
	counts := perfFragmentNameCounts(steps)
	var out []perfFragmentRef
	var walk func([]map[string]interface{})
	walk = func(list []map[string]interface{}) {
		for _, s := range list {
			switch perfStepType(s) {
			case "include", "link":
				out = append(out, perfFragmentRefDecision(s, planName, frags, counts))
			case "fragment":
				// A definition is not a call site; its own references are reported
				// when the fragment is reached through a module reference.
			default:
				walk(stepChildren(s))
			}
		}
	}
	walk(steps)
	return out
}

// perfFragmentRefsHonesty spells out, per reference, whether the emitted plan points at
// a shared fragment container or holds an inline copy. Empty when the scenario has no
// fragment references at all.
func perfFragmentRefsHonesty(refs []perfFragmentRef) string {
	if len(refs) == 0 {
		return ""
	}
	var module, inline, unresolved []string
	for _, r := range refs {
		switch r.Mode {
		case perfFragmentModeModule:
			module = append(module, fmt.Sprintf("%s → module reference to fragment %q", r.Step, r.Ref))
		case perfFragmentModeInline:
			inline = append(inline, fmt.Sprintf("%s → inlined copy (%s)", r.Step, r.Reason))
		default:
			unresolved = append(unresolved, fmt.Sprintf("%s → not emitted (%s)", r.Step, r.Reason))
		}
	}
	parts := []string{fmt.Sprintf("Fragment references (%d):", len(refs))}
	if len(module) > 0 {
		parts = append(parts, fmt.Sprintf("%d reuse a shared test fragment [%s]", len(module), strings.Join(module, "; ")))
	}
	if len(inline) > 0 {
		parts = append(parts, fmt.Sprintf("%d fell back to inline expansion, so the plan holds a copy rather than a reference [%s]",
			len(inline), strings.Join(inline, "; ")))
	}
	if len(unresolved) > 0 {
		parts = append(parts, fmt.Sprintf("%d could not be resolved and contribute no steps to the plan [%s]",
			len(unresolved), strings.Join(unresolved, "; ")))
	}
	return strings.Join(parts, " ") + "."
}

// --- JMX emission ---

// javaStringHashCode reproduces java.lang.String.hashCode over UTF-16 code units.
// JMeter names the entries of a saved collectionProp with the hash of their value; the
// values are what it resolves on load, but matching the naming keeps the file identical
// to one JMeter would write itself.
func javaStringHashCode(s string) int32 {
	var h int32
	for _, u := range utf16.Encode([]rune(s)) {
		h = 31*h + int32(u)
	}
	return h
}

// writeJMXTestFragments emits one disabled TestFragmentController per fragment
// definition at Test Plan level, so every thread group's module references share a
// single copy of the journey. Fragments stay disabled: a Test Fragment is a definition
// and must never execute on its own.
func writeJMXTestFragments(b *strings.Builder, steps []map[string]interface{}, indent string) {
	for i, frag := range perfFragmentDefs(steps) {
		name := nz(perfStepName(frag), fmt.Sprintf("fragment-%d", i+1))
		b.WriteString(fmt.Sprintf(`%s<TestFragmentController guiclass="TestFragmentControllerGui" testclass="TestFragmentController" testname=%q enabled="false">
%s  <stringProp name="opl.fragment">true</stringProp>
%s</TestFragmentController>
%s<hashTree>`+"\n", indent, xmlEscape(name), indent, indent, indent))
		for j, child := range stepChildren(frag) {
			appendStepJMXIndexed(b, child, j, indent+"  ", &jmxEmitCtx{})
		}
		b.WriteString(indent + "</hashTree>\n")
	}
}

// writeJMXModuleController emits a ModuleController pointing at a node path. The path
// is the chain of node names from the tree root, so a Test Plan level fragment is
// ["<plan name>", "<fragment name>"].
func writeJMXModuleController(b *strings.Builder, name string, nodePath []string, indent string) {
	b.WriteString(fmt.Sprintf(`%s<ModuleController guiclass="ModuleControllerGui" testclass="ModuleController" testname=%q enabled="true">
%s  <collectionProp name="ModuleController.node_path">
`, indent, xmlEscape(name), indent))
	for _, node := range nodePath {
		b.WriteString(fmt.Sprintf(`%s    <stringProp name="%d">%s</stringProp>`+"\n",
			indent, javaStringHashCode(node), xmlEscape(node)))
	}
	b.WriteString(fmt.Sprintf(`%s  </collectionProp>
%s</ModuleController>
%s<hashTree/>`+"\n", indent, indent, indent))
}

// writeJMXUserParameters emits the fragment inputs for one reference as a User
// Parameters element. Two details make one fragment reusable with different inputs:
//
//   - It is User Parameters, not a User Defined Variables config element. UDVs resolve
//     once at start, so every reference would end up sharing one set of values.
//   - per_iteration stays false, which is what makes JMeter treat it as a pre-processor
//     and re-apply the values before each sampler in this reference's scope. With
//     per_iteration true it becomes a loop-iteration listener instead, firing at the top
//     of the thread iteration — every reference would then fire before any of them ran
//     and the last one would silently win for the whole iteration.
func writeJMXUserParameters(b *strings.Builder, name string, names []string, vals map[string]string, indent string) {
	if len(names) == 0 {
		return
	}
	b.WriteString(fmt.Sprintf(`%s<UserParameters guiclass="UserParametersGui" testclass="UserParameters" testname=%q enabled="true">
%s  <collectionProp name="UserParameters.names">
`, indent, xmlEscape(name), indent))
	for _, n := range names {
		b.WriteString(fmt.Sprintf(`%s    <stringProp name="%d">%s</stringProp>`+"\n",
			indent, javaStringHashCode(n), xmlEscape(n)))
	}
	// One value row, named the way JMeter names collection rows (1-based index).
	b.WriteString(fmt.Sprintf(`%s  </collectionProp>
%s  <collectionProp name="UserParameters.thread_values">
%s    <collectionProp name="1">
`, indent, indent, indent))
	for _, n := range names {
		b.WriteString(fmt.Sprintf(`%s      <stringProp name="%d">%s</stringProp>`+"\n",
			indent, javaStringHashCode(vals[n]), xmlEscape(vals[n])))
	}
	b.WriteString(fmt.Sprintf(`%s    </collectionProp>
%s  </collectionProp>
%s  <boolProp name="UserParameters.per_iteration">false</boolProp>
%s</UserParameters>
%s<hashTree/>`+"\n", indent, indent, indent, indent, indent))
}

// perfModuleParamsProp marks the wrapper controller that scopes one reference's inputs,
// so import can fold it back into the include step it came from.
const perfModuleParamsProp = "opl.module_params"

// writeJMXModuleParamScope wraps one fragment reference in a Simple Controller holding
// its User Parameters pre-processor, which is what limits the inputs to this reference.
func writeJMXModuleParamScope(b *strings.Builder, ref perfFragmentRef, names []string, vals map[string]string,
	indent string, body func(innerIndent string)) {

	b.WriteString(fmt.Sprintf(`%s<GenericController guiclass="LogicControllerGui" testclass="GenericController" testname=%q enabled="true">
%s  <stringProp name="%s">true</stringProp>
%s</GenericController>
%s<hashTree>`+"\n", indent, xmlEscape(nz(ref.Step, ref.Ref)), indent, perfModuleParamsProp, indent, indent))
	writeJMXUserParameters(b, "Fragment inputs: "+nz(ref.Ref, ref.Step), names, vals, indent+"  ")
	body(indent + "  ")
	b.WriteString(indent + "</hashTree>\n")
}

// --- Synchronised burst (rendezvous) ---

// perfRendezvous is a synchronised burst. JMeter's SyncTimer parks each arriving thread
// until GroupSize of them are waiting and then releases them at once, which turns a
// designed spike into a simultaneous burst instead of a ramp.
//
// GroupSize 0 is JMeter's "every thread in the thread group". A GroupSize larger than
// the threads that will ever arrive never releases, so with TimeoutMS 0 (JMeter's
// no-timeout default) the plan would block forever — perfRendezvousTriage reports that
// rather than the engine quietly rewriting the group size.
type perfRendezvous struct {
	Name      string
	GroupSize int
	TimeoutMS int
}

// perfRendezvousFromStep reads a `rendezvous` step.
func perfRendezvousFromStep(step map[string]interface{}) *perfRendezvous {
	rv := &perfRendezvous{Name: nz(perfStepName(step), "Rendezvous")}
	for _, k := range []string{"group_size", "groupSize", "group"} {
		if n, ok := asFloat(step[k]); ok {
			rv.GroupSize = int(n)
			break
		}
	}
	for _, k := range []string{"timeout_ms", "timeoutMs", "timeout_milliseconds"} {
		if n, ok := asFloat(step[k]); ok {
			rv.TimeoutMS = int(n)
			break
		}
	}
	if rv.GroupSize < 0 {
		rv.GroupSize = 0
	}
	if rv.TimeoutMS < 0 {
		rv.TimeoutMS = 0
	}
	return rv
}

// perfRendezvousFromSchedule reads the plan-level option:
// schedule_json {"rendezvous": {"group_size": N, "timeout_ms": M}} or the
// {"rendezvous_group_size": N} shorthand. Returns nil when unset.
func perfRendezvousFromSchedule(sched map[string]interface{}) *perfRendezvous {
	if sched == nil {
		return nil
	}
	if blk, ok := sched["rendezvous"].(map[string]interface{}); ok && len(blk) > 0 {
		rv := perfRendezvousFromStep(blk)
		rv.Name = nz(strings.TrimSpace(fmt.Sprint(blk["name"])), "Rendezvous")
		if rv.Name == "<nil>" {
			rv.Name = "Rendezvous"
		}
		return rv
	}
	if n, ok := asFloat(sched["rendezvous_group_size"]); ok && int(n) >= 0 {
		rv := &perfRendezvous{Name: "Rendezvous", GroupSize: int(n)}
		if t, ok := asFloat(sched["rendezvous_timeout_ms"]); ok && int(t) > 0 {
			rv.TimeoutMS = int(t)
		}
		return rv
	}
	return nil
}

// writeJMXSyncTimer emits the synchronising timer for one burst. Group size 0 is left
// as JMeter's "every thread in the group" rather than being rewritten to a number.
func writeJMXSyncTimer(b *strings.Builder, rv *perfRendezvous, indent string) {
	if rv == nil {
		return
	}
	b.WriteString(fmt.Sprintf(`%s<SyncTimer guiclass="TestBeanGUI" testclass="SyncTimer" testname=%q enabled="true">
%s  <intProp name="groupSize">%d</intProp>
%s  <longProp name="timeoutInMs">%d</longProp>
%s</SyncTimer>
%s<hashTree/>`+"\n", indent, xmlEscape(nz(rv.Name, "Rendezvous")), indent, rv.GroupSize, indent, rv.TimeoutMS, indent, indent))
}

// perfRendezvousPlacement records where a plan-level burst ended up. A JMeter timer is
// scoped, not sequential: attached to one sampler it gates that request only, while at
// thread-group level it gates every sampler in the journey. Reporting which one was
// emitted keeps the two apart.
type perfRendezvousPlacement struct {
	Mode string `json:"mode"` // sampler | thread_group
	Step string `json:"step,omitempty"`
	Note string `json:"note,omitempty"`
}

// injectPlanRendezvous attaches a plan-level burst to the first request of the journey
// (depth-first, skipping fragment definitions), where the timer's scope is that one
// sampler and the whole VU population starts its journey together. When the journey has
// no request of its own to gate — a tree made only of module references, say — it
// reports thread_group so the caller can emit the timer at thread-group level and say
// that every sampler in the journey becomes a barrier.
func injectPlanRendezvous(steps []map[string]interface{}, rv *perfRendezvous) ([]map[string]interface{}, perfRendezvousPlacement) {
	if rv == nil {
		return steps, perfRendezvousPlacement{}
	}
	out, name, ok := attachRendezvousToFirstHTTP(steps, rv)
	if !ok {
		return steps, perfRendezvousPlacement{
			Mode: "thread_group",
			Note: "journey has no request of its own to gate, so the timer sits at thread-group level and every sampler in the journey becomes a barrier",
		}
	}
	place := perfRendezvousPlacement{Mode: "sampler", Step: name}
	// A reference emitted ahead of the gated request runs before the barrier: say so
	// rather than letting "synchronised burst" imply the whole journey starts together.
	if before := perfStepsBeforeFirstHTTP(steps); len(before) > 0 {
		place.Note = fmt.Sprintf("fragment references %s run before the gated request %q, so the burst begins at %s rather than at the first thing the journey does",
			strings.Join(before, ", "), name, name)
	}
	return out, place
}

// perfStepsBeforeFirstHTTP names the fragment references that execute before the
// journey's first request of its own.
func perfStepsBeforeFirstHTTP(list []map[string]interface{}) []string {
	var out []string
	var walk func([]map[string]interface{}) bool
	walk = func(list []map[string]interface{}) bool {
		for _, s := range list {
			switch perfStepType(s) {
			case "fragment":
				continue
			case "http":
				return true
			case "include", "link":
				out = append(out, nz(perfStepName(s), perfIncludeRef(s)))
			default:
				if walk(stepChildren(s)) {
					return true
				}
			}
		}
		return false
	}
	walk(list)
	return out
}

func attachRendezvousToFirstHTTP(list []map[string]interface{}, rv *perfRendezvous) ([]map[string]interface{}, string, bool) {
	out := make([]map[string]interface{}, 0, len(list))
	name, done := "", false
	for _, s := range list {
		if done {
			out = append(out, s)
			continue
		}
		typ := perfStepType(s)
		if typ == "fragment" {
			out = append(out, s)
			continue
		}
		if typ == "http" {
			clone := cloneStepWithout(s, "children")
			kids := stepChildren(s)
			next := make([]map[string]interface{}, 0, len(kids)+1)
			next = append(next, map[string]interface{}{
				"type": "rendezvous", "name": nz(rv.Name, "Rendezvous"),
				"group_size": rv.GroupSize, "timeout_ms": rv.TimeoutMS,
			})
			next = append(next, kids...)
			clone["children"] = next
			out = append(out, clone)
			name, done = nz(perfStepName(s), "request"), true
			continue
		}
		if kids := stepChildren(s); len(kids) > 0 {
			nkids, n, ok := attachRendezvousToFirstHTTP(kids, rv)
			if ok {
				clone := cloneStepWithout(s, "children")
				clone["children"] = nkids
				out = append(out, clone)
				name, done = n, true
				continue
			}
		}
		out = append(out, s)
	}
	return out, name, done
}

// cloneStepWithout shallow-copies a step, dropping the named keys.
func cloneStepWithout(step map[string]interface{}, drop ...string) map[string]interface{} {
	skip := map[string]bool{}
	for _, k := range drop {
		skip[k] = true
	}
	out := make(map[string]interface{}, len(step))
	for k, v := range step {
		if skip[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// perfRendezvousSteps collects every rendezvous in a VU tree, including inside
// fragment definitions (a fragment reached by a module reference runs its timer too).
func perfRendezvousSteps(steps []map[string]interface{}) []*perfRendezvous {
	var out []*perfRendezvous
	var walk func([]map[string]interface{})
	walk = func(list []map[string]interface{}) {
		for _, s := range list {
			if perfStepType(s) == "rendezvous" {
				out = append(out, perfRendezvousFromStep(s))
			}
			walk(stepChildren(s))
		}
	}
	walk(steps)
	return out
}

// perfRendezvousTriage reports bursts that cannot release. A group size above the
// threads that will ever arrive, with no timeout, parks those threads for the whole run.
func perfRendezvousTriage(steps []map[string]interface{}, vus int) []string {
	var out []string
	for _, rv := range perfRendezvousSteps(steps) {
		if rv.GroupSize == 0 {
			continue
		}
		if vus > 0 && rv.GroupSize > vus {
			if rv.TimeoutMS <= 0 {
				out = append(out, fmt.Sprintf(
					"rendezvous %q waits for %d threads but the scenario runs %d — with no timeout every thread parks for the whole run; lower the group size or set timeout_ms",
					rv.Name, rv.GroupSize, vus))
				continue
			}
			out = append(out, fmt.Sprintf(
				"rendezvous %q waits for %d threads but the scenario runs %d — the group never fills, so every thread waits out the %dms timeout instead of bursting",
				rv.Name, rv.GroupSize, vus, rv.TimeoutMS))
		}
	}
	return out
}
