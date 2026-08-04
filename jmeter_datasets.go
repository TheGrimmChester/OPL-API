package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// perfCSVDataFile is the plan-relative CSV name the engine materializes next to plan.jmx.
// JMeter resolves a relative CSVDataSet path against the directory of the running .jmx,
// so the same name works for host runs and for containers mounted on a shared work root.
const perfCSVDataFile = "data.csv"

// CSVDataSet sharing modes (JMeter values).
const (
	perfCSVShareAll    = "shareMode.all"
	perfCSVShareGroup  = "shareMode.group"
	perfCSVShareThread = "shareMode.thread"
)

// perfCSVDataSetName is the testname of the emitted config element.
const perfCSVDataSetName = "OPL CSV Dataset"

// perfCSVDatasetHonesty is the caveat carried on imported/exported dataset blocks.
const perfCSVDatasetHonesty = "CSV path is local to the runner; upload via datasets_json.inline for Agent-hosted runs."

// perfCSVDataset is the normalized view of scenario `datasets_json` → `{"csv": {…}}`.
//
// Defaults are picked for load tests rather than single-shot functional runs:
//
//	recycle=true          a load run outlives the data file; wrapping keeps threads working
//	                      instead of dying mid-run.
//	stopThread=false      with recycle on, EOF never happens; stopping threads on EOF would
//	                      silently shrink the configured VU count and skew the result.
//	shareMode=all         one iterator shared by every thread, so rows are handed out
//	                      round-robin instead of every thread replaying row 1.
//	quotedData=true       the generated data.csv is written with RFC 4180 quoting, so values
//	                      containing the delimiter survive.
//	ignoreFirstLine=false the generated data.csv holds data rows only — column names always
//	                      travel in the plan as variableNames, never as a header row.
//
// Every default is overridable per scenario (`recycle`, `stop_thread`, `share_mode`,
// `quoted`, `ignore_first_line`, `delimiter`, `encoding`).
type perfCSVDataset struct {
	Filename        string
	Inline          string
	Delimiter       string // exactly one character; tab allowed
	VariableNames   []string
	Recycle         bool
	StopThread      bool
	ShareMode       string
	IgnoreFirstLine bool
	QuotedData      bool
	Encoding        string

	rows     [][]string // inline data rows, header removed
	rawWrite bool       // inline could not be parsed — write it verbatim
	Warnings []string
}

// perfCSVDatasetFromJSON parses a scenario `datasets_json` blob. Returns nil when the
// scenario defines no CSV dataset (the live default is `{}`).
func perfCSVDatasetFromJSON(raw string) *perfCSVDataset {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	blk, ok := m["csv"].(map[string]interface{})
	if !ok || len(blk) == 0 {
		return nil
	}
	return perfCSVDatasetFromMap(blk)
}

// perfCSVDatasetFromMap builds a dataset from the decoded `csv` block.
func perfCSVDatasetFromMap(blk map[string]interface{}) *perfCSVDataset {
	d := &perfCSVDataset{
		Delimiter: ",", Recycle: true, StopThread: false,
		ShareMode: perfCSVShareAll, QuotedData: true, Encoding: "UTF-8",
	}
	d.Filename = strings.TrimSpace(csvCfgString(blk, "filename"))
	d.Inline = csvCfgString(blk, "inline")
	if raw := csvCfgString(blk, "delimiter"); raw != "" || hasAnyKey(blk, "delimiter") {
		delim, ok := normalizePerfCSVDelimiter(raw)
		d.Delimiter = delim
		if !ok {
			d.Warnings = append(d.Warnings,
				fmt.Sprintf("delimiter %q is not a single character — falling back to \",\"", raw))
		}
	}
	d.VariableNames = perfCSVNamesFromAny(blk["variableNames"], blk["variable_names"], blk["columns"])
	if v, ok := csvCfgBool(blk, "recycle"); ok {
		d.Recycle = v
	}
	if v, ok := csvCfgBool(blk, "stop_thread", "stopThread"); ok {
		d.StopThread = v
	}
	if v, ok := csvCfgBool(blk, "quoted", "quotedData", "quoted_data"); ok {
		d.QuotedData = v
	}
	if v, ok := csvCfgBool(blk, "ignore_first_line", "ignoreFirstLine"); ok {
		d.IgnoreFirstLine = v
	}
	if v := csvCfgString(blk, "share_mode", "shareMode"); v != "" {
		d.ShareMode = normalizePerfCSVShareMode(v)
	}
	if v := csvCfgString(blk, "encoding", "fileEncoding"); v != "" {
		d.Encoding = v
	}
	if d.Recycle && d.StopThread {
		d.StopThread = false
		d.Warnings = append(d.Warnings, "recycle and stop_thread are mutually exclusive — stop_thread ignored")
	}
	d.parseInline()
	return d
}

// parseInline splits the pasted CSV with the configured delimiter and resolves column names.
func (d *perfCSVDataset) parseInline() {
	text := strings.ReplaceAll(strings.ReplaceAll(d.Inline, "\r\n", "\n"), "\r", "\n")
	text = strings.Trim(text, "\n")
	if text == "" {
		return
	}
	comma, ok := perfCSVComma(d.Delimiter)
	if !ok {
		d.rawWrite = true
		d.Warnings = append(d.Warnings,
			fmt.Sprintf("delimiter %q cannot be parsed — inline rows are written verbatim", d.Delimiter))
		return
	}
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = comma
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	recs, err := r.ReadAll()
	if err != nil || len(recs) == 0 {
		d.rawWrite = true
		d.Warnings = append(d.Warnings, "inline rows could not be parsed as CSV — written verbatim")
		return
	}
	if len(d.VariableNames) == 0 {
		d.VariableNames = trimAllFields(recs[0])
		recs = recs[1:]
		d.Warnings = append(d.Warnings, "column names taken from the first inline row (no variableNames configured)")
	} else if perfCSVRowIsHeader(recs[0], d.VariableNames) {
		recs = recs[1:]
	}
	d.rows = recs
	for i, rec := range d.rows {
		if len(rec) != len(d.VariableNames) {
			d.Warnings = append(d.Warnings, fmt.Sprintf(
				"inline row %d has %d field(s) for %d column name(s) — check the delimiter",
				i+1, len(rec), len(d.VariableNames)))
			break
		}
	}
	if len(d.rows) == 0 {
		d.Warnings = append(d.Warnings, "inline CSV has column names but no data rows — no CSVDataSet emitted")
	}
}

func (d *perfCSVDataset) hasInline() bool {
	return d != nil && strings.TrimSpace(d.Inline) != ""
}

// planFilename is the path written into the emitted element.
func (d *perfCSVDataset) planFilename() string {
	if d == nil {
		return ""
	}
	if d.hasInline() {
		return perfCSVDataFile
	}
	return d.Filename
}

// emits reports whether the scenario has enough to wire a real CSVDataSet.
func (d *perfCSVDataset) emits() bool {
	if d == nil || d.planFilename() == "" {
		return false
	}
	if d.hasInline() {
		if len(d.VariableNames) == 0 {
			return false
		}
		return d.rawWrite || len(d.rows) > 0
	}
	// External file: JMeter reads its own header when variableNames is empty.
	return d.Filename != ""
}

// columns returns the resolved variable names backing `${…}` tokens.
func (d *perfCSVDataset) columns() []string {
	if d == nil {
		return nil
	}
	return d.VariableNames
}

// rowCount is the number of inline data rows (0 for external files / verbatim writes).
func (d *perfCSVDataset) rowCount() int {
	if d == nil {
		return 0
	}
	return len(d.rows)
}

// columnsUnknown is true for an external CSV whose column names live only in the file —
// unbound-variable checks cannot be resolved in that case.
func (d *perfCSVDataset) columnsUnknown() bool {
	return d != nil && !d.hasInline() && d.Filename != "" && len(d.VariableNames) == 0
}

// firstRow maps the first inline data row onto the column names (validate seeding).
func (d *perfCSVDataset) firstRow() map[string]string {
	if d == nil || len(d.rows) == 0 {
		return nil
	}
	out := map[string]string{}
	for i, name := range d.VariableNames {
		if name == "" || i >= len(d.rows[0]) {
			continue
		}
		out[name] = d.rows[0][i]
	}
	return out
}

// delimiterProp renders the delimiter the way JMeter expects it in the plan
// (a literal tab is written as the two-character escape `\t`).
func (d *perfCSVDataset) delimiterProp() string {
	if d == nil || d.Delimiter == "" {
		return ","
	}
	if d.Delimiter == "\t" {
		return `\t`
	}
	return d.Delimiter
}

// workerCSV renders the data.csv bytes for one worker. Rows are sharded round-robin when
// several containers run the same plan and there are at least as many rows as workers, so
// "unique" data stays unique across the fleet instead of every worker replaying every row.
func (d *perfCSVDataset) workerCSV(worker, workers int) (content string, sharded bool) {
	if !d.hasInline() {
		return "", false
	}
	if d.rawWrite {
		return strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(d.Inline, "\r\n", "\n"), "\r", "\n"), "\n") + "\n", false
	}
	rows := d.rows
	if workers > 1 && worker >= 0 && len(rows) >= workers {
		shard := make([][]string, 0, len(rows)/workers+1)
		for i, rec := range rows {
			if i%workers == worker {
				shard = append(shard, rec)
			}
		}
		rows, sharded = shard, true
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	if comma, ok := perfCSVComma(d.Delimiter); ok {
		w.Comma = comma
	}
	_ = w.WriteAll(rows)
	w.Flush()
	return b.String(), sharded
}

// summary is the operator-facing description attached to validate / run responses.
func (d *perfCSVDataset) summary() map[string]interface{} {
	if d == nil {
		return nil
	}
	out := map[string]interface{}{
		"filename":          d.planFilename(),
		"variable_names":    d.columns(),
		"delimiter":         d.delimiterProp(),
		"recycle":           d.Recycle,
		"stop_thread":       d.StopThread,
		"share_mode":        d.ShareMode,
		"quoted_data":       d.QuotedData,
		"ignore_first_line": d.ignoreFirstLineProp(),
		"encoding":          d.Encoding,
		"rows":              d.rowCount(),
		"emitted":           d.emits(),
		"honesty":           perfCSVDatasetHonesty,
	}
	if len(d.Warnings) > 0 {
		out["warnings"] = d.Warnings
	}
	return out
}

// ignoreFirstLineProp is always false for the data.csv this engine writes (data rows only).
func (d *perfCSVDataset) ignoreFirstLineProp() bool {
	if d == nil {
		return false
	}
	if d.hasInline() && !d.rawWrite {
		return false
	}
	return d.IgnoreFirstLine
}

// writeJMXCSVDataSet emits the config element at Test Plan level so every thread group
// (classic VUs and arrivals segments alike) sees the same variables.
func writeJMXCSVDataSet(b *strings.Builder, d *perfCSVDataset, indent string) {
	if !d.emits() {
		return
	}
	b.WriteString(fmt.Sprintf(`%s<CSVDataSet guiclass="TestBeanGUI" testclass="CSVDataSet" testname=%q enabled="true">
%s  <stringProp name="filename">%s</stringProp>
%s  <stringProp name="fileEncoding">%s</stringProp>
%s  <stringProp name="variableNames">%s</stringProp>
%s  <stringProp name="delimiter">%s</stringProp>
%s  <boolProp name="ignoreFirstLine">%t</boolProp>
%s  <boolProp name="quotedData">%t</boolProp>
%s  <boolProp name="recycle">%t</boolProp>
%s  <boolProp name="stopThread">%t</boolProp>
%s  <stringProp name="shareMode">%s</stringProp>
%s</CSVDataSet>
%s<hashTree/>`+"\n",
		indent, xmlEscape(perfCSVDataSetName),
		indent, xmlEscape(d.planFilename()),
		indent, xmlEscape(nz(d.Encoding, "UTF-8")),
		indent, xmlEscape(strings.Join(d.columns(), ",")),
		indent, xmlEscape(d.delimiterProp()),
		indent, d.ignoreFirstLineProp(),
		indent, d.QuotedData,
		indent, d.Recycle,
		indent, d.StopThread,
		indent, xmlEscape(normalizePerfCSVShareMode(d.ShareMode)),
		indent, indent))
}

// injectJMXCSVDataSet wires a dataset into a plan that was stored before this engine emitted
// CSVDataSet (or into raw imported JMX). It inserts the element as a sibling of the first
// ThreadGroup, i.e. inside the Test Plan hashTree, and never touches a plan that already
// declares one.
func injectJMXCSVDataSet(jmx string, d *perfCSVDataset) (string, bool) {
	if !d.emits() || strings.TrimSpace(jmx) == "" {
		return jmx, false
	}
	if strings.Contains(jmx, "<CSVDataSet") {
		return jmx, false
	}
	idx := strings.Index(jmx, "<ThreadGroup")
	if idx < 0 {
		return jmx, false
	}
	lineStart := strings.LastIndex(jmx[:idx], "\n") + 1
	indent := jmx[lineStart:idx]
	if strings.TrimSpace(indent) != "" {
		lineStart, indent = idx, "      "
	}
	var b strings.Builder
	writeJMXCSVDataSet(&b, d, indent)
	if b.Len() == 0 {
		return jmx, false
	}
	return jmx[:lineStart] + b.String() + jmx[lineStart:], true
}

// syncJMXCSVDataSet makes a stored plan agree with datasets_json: any existing CSVDataSet is
// dropped and re-emitted from the scenario. Without this, editing the delimiter or the columns
// would leave the executed plan on the previous wiring — the same silent-mismatch class of bug
// as having no element at all. Plans whose scenario defines no dataset are left untouched.
func syncJMXCSVDataSet(jmx string, d *perfCSVDataset) (string, bool) {
	if !d.emits() || strings.TrimSpace(jmx) == "" {
		return jmx, false
	}
	stripped, removed := stripJMXCSVDataSets(jmx)
	out, added := injectJMXCSVDataSet(stripped, d)
	if !added {
		// Nothing to anchor to — keep the original plan rather than dropping its wiring.
		return jmx, false
	}
	return out, added || removed
}

// stripJMXCSVDataSets removes every CSVDataSet element plus the sibling <hashTree/> that
// follows it. CSVDataSet is a leaf config element, so that sibling is always self-closed.
func stripJMXCSVDataSets(jmx string) (string, bool) {
	removed := false
	for {
		start := strings.Index(jmx, "<CSVDataSet")
		if start < 0 {
			return jmx, removed
		}
		closeIdx := strings.Index(jmx[start:], "</CSVDataSet>")
		if closeIdx < 0 {
			return jmx, removed
		}
		end := start + closeIdx + len("</CSVDataSet>")
		// Swallow the sibling hashTree and the surrounding whitespace/newlines.
		rest := jmx[end:]
		trimmed := strings.TrimLeft(rest, " \t\r\n")
		if strings.HasPrefix(trimmed, "<hashTree/>") {
			consumed := len(rest) - len(trimmed) + len("<hashTree/>")
			end += consumed
		}
		if nl := strings.IndexAny(jmx[end:], "\n"); nl >= 0 && strings.TrimSpace(jmx[end:end+nl]) == "" {
			end += nl + 1
		}
		lineStart := strings.LastIndex(jmx[:start], "\n") + 1
		if strings.TrimSpace(jmx[lineStart:start]) != "" {
			lineStart = start
		}
		jmx = jmx[:lineStart] + jmx[end:]
		removed = true
	}
}

// perfDatasetFromJMXCSVDataSet maps an imported CSVDataSet back onto datasets_json.
func perfDatasetFromJMXCSVDataSet(c jmxCSVDataSet) map[string]interface{} {
	pm := jmxPropMap(c.Props)
	bm := jmxBoolPropMap(c.Bools)
	delim, _ := normalizePerfCSVDelimiter(nz(pm["delimiter"], ","))
	out := map[string]interface{}{
		"filename":          pm["filename"],
		"variableNames":     pm["variableNames"],
		"delimiter":         delim,
		"recycle":           jmxFlag(bm, pm, "recycle", true),
		"stop_thread":       jmxFlag(bm, pm, "stopThread", false),
		"quoted":            jmxFlag(bm, pm, "quotedData", true),
		"ignore_first_line": jmxFlag(bm, pm, "ignoreFirstLine", false),
		"share_mode":        normalizePerfCSVShareMode(nz(pm["shareMode"], perfCSVShareAll)),
		"honesty":           perfCSVDatasetHonesty,
	}
	if enc := pm["fileEncoding"]; enc != "" {
		out["encoding"] = enc
	}
	return out
}

func jmxBoolPropMap(props []jmxBoolProp) map[string]string {
	m := map[string]string{}
	for _, p := range props {
		m[p.Name] = strings.TrimSpace(p.Value)
	}
	return m
}

func jmxFlag(bools, strs map[string]string, key string, def bool) bool {
	for _, m := range []map[string]string{bools, strs} {
		if v, ok := m[key]; ok && v != "" {
			return strings.EqualFold(v, "true")
		}
	}
	return def
}

// --- unbound `${…}` detection -------------------------------------------------------

// perfVarTokenRe matches plain variable references only. JMeter function calls
// (`${__P(x)}`, `${__jexl3(…)}`) never match, so they are not reported as unbound.
var perfVarTokenRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.\-]*)\}`)

// perfJMXArgumentNameRe finds User Defined Variables / Arguments declared inside raw JMX,
// so imported plans that bind their own variables are not falsely flagged.
var perfJMXArgumentNameRe = regexp.MustCompile(`<stringProp name="Argument\.name">([^<]*)</stringProp>`)

// perfJMXVariableNamesRe finds CSVDataSet columns declared directly in raw JMX (pasted plans
// whose datasets_json was never filled in).
var perfJMXVariableNamesRe = regexp.MustCompile(`<stringProp name="variableNames">([^<]*)</stringProp>`)

// perfBuiltinVars are bound by the generated plan itself.
var perfBuiltinVars = []string{"LOAD_RUN_ID"}

func perfVarTokensIn(s string) []string {
	if s == "" || !strings.Contains(s, "${") {
		return nil
	}
	var out []string
	for _, m := range perfVarTokenRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if strings.HasPrefix(name, "__") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// perfScenarioVarTokens collects every `${…}` reference a run would try to expand.
func perfScenarioVarTokens(sc map[string]interface{}) []string {
	var tokens []string
	add := func(v interface{}) {
		s := fmt.Sprint(v)
		if s == "<nil>" {
			return
		}
		tokens = append(tokens, perfVarTokensIn(s)...)
	}
	add(getString(sc, "target_url"))
	add(getString(sc, "body"))
	add(getString(sc, "headers_json"))
	var walk func([]map[string]interface{})
	walk = func(list []map[string]interface{}) {
		for _, st := range list {
			for _, k := range []string{"url", "body", "condition", "expression", "page_url", "selector", "body_contains"} {
				if v, ok := st[k]; ok {
					add(v)
				}
			}
			if h, ok := st["headers"].(map[string]interface{}); ok {
				for hk, hv := range h {
					add(hk)
					add(hv)
				}
			}
			walk(stepChildren(st))
		}
	}
	walk(scenarioSteps(sc))
	return tokens
}

// perfScenarioKnownVars lists variables a run can actually bind: dataset columns, extractor
// refnames, ForEach loop variables, plan built-ins, and Arguments declared in raw JMX.
func perfScenarioKnownVars(sc map[string]interface{}, d *perfCSVDataset) map[string]bool {
	known := map[string]bool{}
	for _, v := range perfBuiltinVars {
		known[v] = true
	}
	for _, c := range d.columns() {
		if c != "" {
			known[c] = true
		}
	}
	var walk func([]map[string]interface{})
	walk = func(list []map[string]interface{}) {
		for _, st := range list {
			typ := strings.TrimSpace(fmt.Sprint(st["type"]))
			if v := strings.TrimSpace(fmt.Sprint(st["var"])); v != "" && v != "<nil>" {
				known[v] = true
			}
			if typ == "foreach" || typ == "foreach_controller" || typ == "for_each" {
				for _, k := range []string{"return_var", "input_var"} {
					if v := strings.TrimSpace(fmt.Sprint(st[k])); v != "" && v != "<nil>" {
						known[v] = true
					}
				}
			}
			// Fragment inputs bind the tokens the reused journey reads.
			if names, _ := perfStepParams(st); len(names) > 0 {
				for _, n := range names {
					known[n] = true
				}
			}
			walk(stepChildren(st))
		}
	}
	walk(scenarioSteps(sc))
	jmx := getString(sc, "jmx_xml")
	for _, m := range perfJMXArgumentNameRe.FindAllStringSubmatch(jmx, -1) {
		if name := strings.TrimSpace(m[1]); name != "" {
			known[name] = true
		}
	}
	for _, m := range perfJMXVariableNamesRe.FindAllStringSubmatch(jmx, -1) {
		for _, name := range splitPerfCSVNames(m[1]) {
			known[name] = true
		}
	}
	return known
}

// scenarioUnboundVariables lists `${…}` tokens with no backing dataset column, extracted
// variable, or built-in. resolvable is false when the dataset points at an external CSV
// whose column names are only known to the runner — nothing can be asserted then.
func scenarioUnboundVariables(sc map[string]interface{}) (unbound []string, resolvable bool) {
	d := perfCSVDatasetFromJSON(getString(sc, "datasets_json"))
	if d.columnsUnknown() {
		return []string{}, false
	}
	known := perfScenarioKnownVars(sc, d)
	seen := map[string]bool{}
	unbound = []string{}
	for _, tok := range perfScenarioVarTokens(sc) {
		if known[tok] || seen[tok] {
			continue
		}
		seen[tok] = true
		unbound = append(unbound, tok)
	}
	sort.Strings(unbound)
	return unbound, true
}

// unboundVariableTriage is the pre-load triage card for unbound tokens. Without it a plan
// fires requests containing literal `${…}` text and nothing warns the operator.
func unboundVariableTriage(unbound []string) map[string]interface{} {
	return map[string]interface{}{
		"type": "dataset", "name": "unbound variables", "ok": false,
		"severity":  "unbound_variable",
		"variables": unbound,
		"error":     "unbound ${…} tokens: " + strings.Join(unbound, ", "),
		"hint": "No dataset column, extractor, or built-in backs these tokens — requests would fire with literal ${…} text. " +
			"Add the columns to datasets.csv (variableNames + rows) or an extractor before dispatching workers.",
	}
}

// --- small helpers -----------------------------------------------------------------

func hasAnyKey(m map[string]interface{}, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func csvCfgString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

func csvCfgBool(m map[string]interface{}, keys ...string) (bool, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t, true
		case string:
			s := strings.ToLower(strings.TrimSpace(t))
			return s == "1" || s == "true" || s == "yes", true
		case float64:
			return t != 0, true
		}
	}
	return false, false
}

func perfCSVNamesFromAny(vals ...interface{}) []string {
	for _, v := range vals {
		switch t := v.(type) {
		case nil:
			continue
		case string:
			if names := splitPerfCSVNames(t); len(names) > 0 {
				return names
			}
		case []interface{}:
			out := make([]string, 0, len(t))
			for _, x := range t {
				if s := strings.TrimSpace(fmt.Sprint(x)); s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		case []string:
			if names := splitPerfCSVNames(strings.Join(t, ",")); len(names) > 0 {
				return names
			}
		}
	}
	return nil
}

func splitPerfCSVNames(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// trimAllFields trims each header cell but keeps positions — an empty column name is a
// valid JMeter instruction to skip that column.
func trimAllFields(rec []string) []string {
	out := make([]string, 0, len(rec))
	for _, f := range rec {
		out = append(out, strings.TrimSpace(f))
	}
	return out
}

func perfCSVRowIsHeader(rec, names []string) bool {
	if len(rec) != len(names) {
		return false
	}
	for i := range rec {
		if strings.TrimSpace(rec[i]) != strings.TrimSpace(names[i]) {
			return false
		}
	}
	return true
}

// normalizePerfCSVDelimiter accepts a single character or a friendly name and reports
// whether the input was usable (false → caller warns and the comma default applies).
func normalizePerfCSVDelimiter(in string) (string, bool) {
	if in == "" {
		return ",", true
	}
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "tab", `\t`:
		return "\t", true
	case "comma":
		return ",", true
	case "semicolon":
		return ";", true
	case "pipe":
		return "|", true
	}
	r := []rune(in)
	if len(r) != 1 {
		return ",", false
	}
	switch r[0] {
	case '"', '\r', '\n':
		return ",", false
	}
	return string(r[0]), true
}

// perfCSVComma converts the delimiter to an encoding/csv comma rune.
func perfCSVComma(delim string) (rune, bool) {
	r := []rune(delim)
	if len(r) != 1 {
		return ',', false
	}
	switch r[0] {
	case '"', '\r', '\n':
		return ',', false
	}
	return r[0], true
}

func normalizePerfCSVShareMode(in string) string {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "", "all", "sharemode.all":
		return perfCSVShareAll
	case "group", "threadgroup", "thread_group", "sharemode.group":
		return perfCSVShareGroup
	case "thread", "sharemode.thread":
		return perfCSVShareThread
	}
	if strings.HasPrefix(strings.TrimSpace(in), "shareMode.") {
		return strings.TrimSpace(in)
	}
	return perfCSVShareAll
}
