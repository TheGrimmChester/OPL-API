package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

func stepChildren(step map[string]interface{}) []map[string]interface{} {
	if step == nil {
		return nil
	}
	raw, ok := step["children"]
	if !ok || raw == nil {
		return nil
	}
	switch t := raw.(type) {
	case []map[string]interface{}:
		return t
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(t))
		for _, x := range t {
			if m, ok := x.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		blob, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		var out []map[string]interface{}
		if json.Unmarshal(blob, &out) != nil {
			return nil
		}
		return out
	}
}

// isPerfControllerMarkerType reports whether a step type is a logic controller that
// flattenScenarioSteps keeps in the flat list as a structural marker. Both the flatten
// walk and the validate dry run read this one list, so a controller can never be
// mistaken for a sampler and fired as a request.
func isPerfControllerMarkerType(typ string) bool {
	switch typ {
	case "if", "if_controller", "while", "while_controller", "loop", "loop_controller",
		"foreach", "foreach_controller", "for_each":
		return true
	default:
		return false
	}
}

func isLogicControllerType(typ string) bool {
	if isPerfControllerMarkerType(typ) {
		return true
	}
	switch typ {
	case "fragment", "include", "link", "container", "transaction":
		return true
	default:
		return false
	}
}

// cloneStepDroppingChildren copies a step without its children[], so a flattened entry
// cannot be read as still owning the subtree that now follows it in flat order.
func cloneStepDroppingChildren(s map[string]interface{}) map[string]interface{} {
	clone := map[string]interface{}{}
	for k, v := range s {
		if k == "children" {
			continue
		}
		clone[k] = v
	}
	return clone
}

// indexFragmentsByName walks a VU tree and maps fragment name → node.
func indexFragmentsByName(steps []map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	var walk func([]map[string]interface{})
	walk = func(list []map[string]interface{}) {
		for _, s := range list {
			typ := fmt.Sprint(s["type"])
			if typ == "fragment" {
				name := strings.TrimSpace(fmt.Sprint(s["name"]))
				if name != "" && name != "<nil>" {
					out[name] = s
				}
			}
			walk(stepChildren(s))
		}
	}
	walk(steps)
	return out
}

// resolveIncludeSteps expands include/link nodes to the referenced fragment's children.
// Unknown refs become a single failed placeholder step for validate honesty.
func resolveIncludeSteps(step map[string]interface{}, frags map[string]map[string]interface{}) []map[string]interface{} {
	ref := strings.TrimSpace(fmt.Sprint(step["ref"]))
	if ref == "" || ref == "<nil>" {
		ref = strings.TrimSpace(fmt.Sprint(step["fragment"]))
	}
	if ref == "" || ref == "<nil>" {
		ref = strings.TrimSpace(fmt.Sprint(step["name"]))
	}
	frag, ok := frags[ref]
	if !ok || frag == nil {
		return []map[string]interface{}{{
			"type": "transaction", "name": "include:" + ref, "ok": false,
			"error": "fragment not found: " + ref,
		}}
	}
	return stepChildren(frag)
}

// foldModuleParamScope collapses an emitted parameter scope — a marked Simple
// Controller holding a User Parameters pre-processor and one module reference — back
// into the single include step it was written from, so params survive a round trip.
// Reports false for any other shape rather than inventing a reference.
func foldModuleParamScope(kids []map[string]interface{}) (map[string]interface{}, bool) {
	var params map[string]interface{}
	var body []map[string]interface{}
	for _, k := range kids {
		if fmt.Sprint(k["type"]) == "params" {
			if p, ok := k["params"].(map[string]interface{}); ok && len(p) > 0 {
				params = p
			}
			continue
		}
		body = append(body, k)
	}
	if params == nil || len(body) != 1 {
		return nil, false
	}
	if fmt.Sprint(body[0]["type"]) != "include" {
		return nil, false
	}
	step := map[string]interface{}{}
	for k, v := range body[0] {
		step[k] = v
	}
	step["params"] = params
	return step, true
}

// seekTestPlanHashTree advances dec to just inside the Test Plan's hashTree.
func seekTestPlanHashTree(dec *xml.Decoder) bool {
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "TestPlan" {
			continue
		}
		if _, err := readXMLStringProps(dec, se); err != nil {
			return false
		}
		for {
			ntok, nerr := dec.Token()
			if nerr != nil {
				return false
			}
			switch nt := ntok.(type) {
			case xml.CharData, xml.Comment:
				continue
			case xml.StartElement:
				if nt.Name.Local == "hashTree" {
					return true
				}
				_ = skipXMLElement(dec, nt)
			case xml.EndElement:
				return false
			}
		}
	}
}

// extractFragmentStepsFromJMX returns the reusable journeys defined at Test Plan level,
// where the emitter puts them so every thread group's module references share one copy.
// Thread group contents are not returned here — ThreadGroup itself maps to no step, so
// its subtree is discarded and cannot double up with extractStepsFromJMXTree.
func extractFragmentStepsFromJMX(raw []byte) []map[string]interface{} {
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	if !seekTestPlanHashTree(dec) {
		return nil
	}
	steps, err := parseJMXHashTreeSteps(dec)
	if err != nil && len(steps) == 0 {
		return nil
	}
	var out []map[string]interface{}
	for _, s := range steps {
		if fmt.Sprint(s["type"]) == "fragment" {
			out = append(out, s)
		}
	}
	return out
}

// canNestChildren reports whether a step type may own a children[] list in the VU tree.
func canNestChildren(typ string) bool {
	if typ == "" || typ == "http" {
		return true
	}
	return isLogicControllerType(typ)
}

// flattenScenarioSteps walks a VU tree depth-first into validate/runtime order
// (containers/logic controllers unwrap; HTTP then its nested extract/assert children).
// Fragments are definitions only (skipped); include/link expands referenced children.
// Disabled steps (enabled=false) are skipped for validate — they remain in steps_json and
// are still emitted to JMX with enabled="false".
func flattenScenarioSteps(steps []map[string]interface{}) []map[string]interface{} {
	frags := indexFragmentsByName(steps)
	var out []map[string]interface{}
	var walk func([]map[string]interface{}, []interface{})
	walk = func(list []map[string]interface{}, base []interface{}) {
		for i, s := range list {
			p := append(append([]interface{}{}, base...), i)
			if !stepEnabled(s) {
				continue
			}
			typ := fmt.Sprint(s["type"])
			if typ == "" || typ == "<nil>" {
				typ = "http"
			}
			kids := stepChildren(s)
			childBase := append(append([]interface{}{}, p...), "children")
			attachPath := func(m map[string]interface{}) map[string]interface{} {
				m["path"] = append([]interface{}{}, p...)
				return m
			}
			if isPerfControllerMarkerType(typ) {
				// A logic controller stays in the flat list as a structural marker so
				// validate can report the journey shape; the steps it wraps follow it.
				out = append(out, attachPath(cloneStepDroppingChildren(s)))
				walk(kids, childBase)
				continue
			}
			switch typ {
			case "fragment":
				// Definition only — reached through include/link, not executed inline.
				continue
			case "include", "link":
				// The dry run walks into the referenced journey whichever way the plan
				// emits it. A reference that resolves to nothing contributes a failing
				// step rather than disappearing from the run it was designed into.
				if pnames, _ := perfStepParams(s); len(pnames) > 0 {
					out = append(out, attachPath(map[string]interface{}{
						"type": "params", "name": "Fragment inputs: " + perfIncludeRef(s),
						"params": s["params"],
					}))
				}
				if len(kids) > 0 {
					walk(kids, childBase)
					continue
				}
				ref := perfIncludeRef(s)
				frag, found := frags[ref]
				if !found || frag == nil {
					out = append(out, attachPath(map[string]interface{}{
						"type": "include", "name": nz(perfStepName(s), ref), "ref": ref,
						"ok": false, "error": "no fragment named " + ref + " in this scenario",
					}))
					continue
				}
				walk(stepChildren(frag), nil)
			case "container", "transaction":
				out = append(out, attachPath(map[string]interface{}{
					"type": "transaction", "name": s["name"], "ok": true,
				}))
				walk(kids, childBase)
			case "http":
				out = append(out, attachPath(cloneStepDroppingChildren(s)))
				walk(kids, childBase)
			default:
				out = append(out, attachPath(cloneStepDroppingChildren(s)))
			}
		}
	}
	walk(steps, nil)
	return out
}

// --- Nested JMX → steps (sibling element + hashTree pairs) ---

func xmlAttr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func skipXMLElement(dec *xml.Decoder, start xml.StartElement) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	_ = start
	return nil
}

func readXMLStringProps(dec *xml.Decoder, start xml.StartElement) (map[string]string, error) {
	props, _, err := readXMLProps(dec, start)
	return props, err
}

// isXMLScalarProp reports the JMX property elements that carry a single value.
func isXMLScalarProp(local string) bool {
	switch local {
	case "stringProp", "boolProp", "intProp", "longProp", "doubleProp", "floatProp":
		return true
	}
	return false
}

// readXMLProps reads a JMX element's properties: the flat name→value map plus, for each
// collectionProp, the ordered values it holds. Nested collections flatten into their
// outermost collection's list, which is what makes an ordered ModuleController node path
// and a UserParameters name/value pair recoverable on import.
func readXMLProps(dec *xml.Decoder, start xml.StartElement) (map[string]string, map[string][]string, error) {
	props := map[string]string{}
	colls := map[string][]string{}
	depth := 1
	var curName string
	var buf strings.Builder
	var collStack []string
	inString := false
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return props, colls, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "collectionProp" {
				name := xmlAttr(t, "name")
				if len(collStack) == 0 {
					if _, seen := colls[name]; !seen {
						colls[name] = []string{}
					}
					collStack = append(collStack, name)
				} else {
					collStack = append(collStack, collStack[0])
				}
				continue
			}
			if isXMLScalarProp(t.Name.Local) {
				curName = xmlAttr(t, "name")
				buf.Reset()
				inString = true
			}
		case xml.CharData:
			if inString {
				buf.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "collectionProp" {
				if n := len(collStack); n > 0 {
					collStack = collStack[:n-1]
				}
				depth--
				continue
			}
			if inString && isXMLScalarProp(t.Name.Local) {
				val := strings.TrimSpace(buf.String())
				props[curName] = val
				if len(collStack) > 0 {
					colls[collStack[0]] = append(colls[collStack[0]], val)
				}
				inString = false
			}
			depth--
		}
	}
	_ = start
	return props, colls, nil
}

func parseJMXHashTreeSteps(dec *xml.Decoder) ([]map[string]interface{}, error) {
	var steps []map[string]interface{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return steps, nil
		}
		if err != nil {
			return steps, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if t.Name.Local == "hashTree" {
				return steps, nil
			}
		case xml.StartElement:
			if t.Name.Local == "hashTree" {
				// Empty nested hashTree under leaf — skip.
				if _, err := parseJMXHashTreeSteps(dec); err != nil {
					return steps, err
				}
				continue
			}
			testname := xmlAttr(t, "testname")
			props, colls, err := readXMLProps(dec, t)
			if err != nil {
				return steps, err
			}
			// Expect following sibling hashTree for children.
			kids := []map[string]interface{}{}
			for {
				ntok, nerr := dec.Token()
				if nerr != nil {
					return steps, nerr
				}
				switch nt := ntok.(type) {
				case xml.CharData:
					continue
				case xml.Comment:
					continue
				case xml.StartElement:
					if nt.Name.Local == "hashTree" {
						kids, err = parseJMXHashTreeSteps(dec)
						if err != nil {
							return steps, err
						}
					} else {
						// Unexpected — put back by treating as sibling: shouldn't happen in well-formed JMX.
						_ = skipXMLElement(dec, nt)
					}
					goto nextElem
				case xml.EndElement:
					goto nextElem
				}
			}
		nextElem:
			enabledAttr := xmlAttr(t, "enabled")
			step := jmxElementToStep(t.Name.Local, testname, props, colls, kids)
			if step != nil {
				if enabledAttr == "false" {
					step["enabled"] = false
				}
				steps = append(steps, step)
			}
		}
	}
}

func jmxElementToStep(local, testname string, props map[string]string, colls map[string][]string, kids []map[string]interface{}) map[string]interface{} {
	switch local {
	case "TestFragmentController":
		return map[string]interface{}{
			"type": "fragment", "name": nz(strings.TrimPrefix(testname, "Fragment:"), "Fragment"),
			"children": kids,
		}
	case "ModuleController":
		path := colls["ModuleController.node_path"]
		ref := ""
		if len(path) > 0 {
			ref = path[len(path)-1]
		}
		return map[string]interface{}{
			"type": "include", "name": nz(testname, nz(ref, "Include")), "ref": ref,
		}
	case "SyncTimer":
		group, timeout := 0, 0
		fmt.Sscanf(props["groupSize"], "%d", &group)
		fmt.Sscanf(props["timeoutInMs"], "%d", &timeout)
		return map[string]interface{}{
			"type": "rendezvous", "name": nz(testname, "Rendezvous"),
			"group_size": group, "timeout_ms": timeout,
		}
	case "UserParameters":
		names := colls["UserParameters.names"]
		values := colls["UserParameters.thread_values"]
		params := map[string]interface{}{}
		for i, n := range names {
			if i < len(values) {
				params[n] = values[i]
			} else {
				params[n] = ""
			}
		}
		if len(params) == 0 {
			return nil
		}
		return map[string]interface{}{
			"type": "params", "name": nz(testname, "Fragment inputs"), "params": params,
		}
	case "HTTPSamplerProxy":
		domain := props["HTTPSampler.domain"]
		path := props["HTTPSampler.path"]
		port := props["HTTPSampler.port"]
		proto := props["HTTPSampler.protocol"]
		if proto == "" {
			proto = "http"
		}
		method := props["HTTPSampler.method"]
		if method == "" {
			method = "GET"
		}
		url := path
		if domain != "" {
			host := domain
			if port != "" && port != "80" && port != "443" {
				host = domain + ":" + port
			}
			if path != "" && !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			url = fmt.Sprintf("%s://%s%s", proto, host, path)
		} else if url != "" && !strings.HasPrefix(url, "http") {
			if !strings.HasPrefix(url, "/") {
				url = "/" + url
			}
			url = "http://127.0.0.1" + url
		}
		step := map[string]interface{}{
			"type": "http", "name": nz(testname, "Request"),
			"method": method, "url": url, "body": props["Argument.value"],
		}
		if props["HTTPSampler.follow_redirects"] == "false" {
			step["follow_redirects"] = false
		}
		if n := props["HTTPSampler.connect_timeout"]; n != "" && n != "0" {
			var ms int
			if _, err := fmt.Sscanf(n, "%d", &ms); err == nil && ms > 0 {
				step["connect_timeout_ms"] = ms
			}
		}
		if n := props["HTTPSampler.response_timeout"]; n != "" && n != "0" {
			var ms int
			if _, err := fmt.Sscanf(n, "%d", &ms); err == nil && ms > 0 {
				step["response_timeout_ms"] = ms
			}
		}
		var cleaned []map[string]interface{}
		for _, k := range kids {
			kt := fmt.Sprint(k["type"])
			if kt == "think" {
				if ms, ok := k["think_ms"]; ok {
					step["think_ms"] = ms
				}
				if ms, ok := k["think_ms_rand"]; ok {
					step["think_ms_rand"] = ms
				}
				continue
			}
			if kt == "_headers" {
				if hdrs, ok := k["headers"]; ok {
					step["headers"] = hdrs
				}
				continue
			}
			cleaned = append(cleaned, k)
		}
		if len(cleaned) > 0 {
			step["children"] = cleaned
		}
		return step
	case "TransactionController":
		if props["opl.fragment"] == "true" || strings.HasPrefix(testname, "Fragment:") {
			name := strings.TrimPrefix(testname, "Fragment:")
			return map[string]interface{}{
				"type": "fragment", "name": nz(name, "Fragment"), "children": kids,
			}
		}
		tx := map[string]interface{}{
			"type": "transaction", "name": nz(testname, "Transaction"), "children": kids,
		}
		if props["TransactionController.includeTimers"] == "true" {
			tx["include_timers"] = true
		}
		if props["TransactionController.parent"] == "true" {
			tx["generate_parent_sample"] = true
		}
		return tx
	case "IfController":
		ifc := map[string]interface{}{
			"type": "if", "name": nz(testname, "If"),
			"condition": props["IfController.condition"], "children": kids,
		}
		if props["IfController.evaluateAll"] == "true" {
			ifc["evaluate_all"] = true
		}
		if props["IfController.useExpression"] == "false" {
			ifc["use_expression"] = false
		}
		return ifc
	case "WhileController":
		return map[string]interface{}{
			"type": "while", "name": nz(testname, "While"),
			"condition": props["WhileController.condition"], "children": kids,
		}
	case "LoopController":
		loops := 1
		fmt.Sscanf(props["LoopController.loops"], "%d", &loops)
		forever := props["LoopController.continue_forever"] == "true"
		return map[string]interface{}{
			"type": "loop", "name": nz(testname, "Loop"),
			"loops": loops, "forever": forever, "children": kids,
		}
	case "ForeachController":
		return map[string]interface{}{
			"type": "foreach", "name": nz(testname, "ForEach"),
			"input_var":     props["ForeachController.inputVal"],
			"return_var":    props["ForeachController.returnVal"],
			"use_separator": props["ForeachController.useSeparator"] != "false",
			"children":      kids,
		}
	case "GenericController":
		if props["opl.fragment"] == "true" || strings.HasPrefix(testname, "Fragment:") {
			name := strings.TrimPrefix(testname, "Fragment:")
			return map[string]interface{}{
				"type": "fragment", "name": nz(name, "Fragment"), "children": kids,
			}
		}
		if props[perfModuleParamsProp] == "true" {
			if step, ok := foldModuleParamScope(kids); ok {
				return step
			}
		}
		return map[string]interface{}{
			"type": "transaction", "name": nz(testname, "Controller"), "children": kids,
		}
	case "RegexExtractor":
		ex := map[string]interface{}{
			"type": "extract", "name": nz(testname, props["RegexExtractor.refname"]),
			"engine": "regex", "expression": props["RegexExtractor.regex"],
			"var": props["RegexExtractor.refname"],
		}
		if t := props["RegexExtractor.template"]; t != "" {
			ex["template"] = t
		}
		if n := props["RegexExtractor.match_number"]; n != "" {
			var mn int
			if _, err := fmt.Sscanf(n, "%d", &mn); err == nil {
				ex["match_number"] = mn
			}
		}
		if d := props["RegexExtractor.default"]; d != "" {
			ex["default_value"] = d
		}
		return ex
	case "JSONPostProcessor":
		ex := map[string]interface{}{
			"type": "extract", "name": nz(testname, props["JSONPostProcessor.referenceNames"]),
			"engine": "jsonpath", "expression": props["JSONPostProcessor.jsonPathExprs"],
			"var": props["JSONPostProcessor.referenceNames"],
		}
		if n := props["JSONPostProcessor.match_numbers"]; n != "" {
			var mn int
			if _, err := fmt.Sscanf(n, "%d", &mn); err == nil {
				ex["match_number"] = mn
			}
		}
		if d := props["JSONPostProcessor.defaultValues"]; d != "" {
			ex["default_value"] = d
		}
		return ex
	case "ResponseAssertion":
		st := props["0"]
		step := map[string]interface{}{"type": "assert", "name": nz(testname, "Assert")}
		if props["Assertion.test_field"] == "Assertion.response_code" || st != "" {
			var code int
			if _, err := fmt.Sscanf(st, "%d", &code); err == nil {
				step["status"] = code
			}
		}
		if props["Assertion.test_field"] == "Assertion.response_data" {
			step["body_contains"] = st
		}
		if f := props["Assertion.test_field"]; f != "" {
			step["assert_field"] = strings.TrimPrefix(f, "Assertion.")
		}
		if props["Assertion.assume_success"] == "true" {
			step["assume_success"] = true
		}
		return step
	case "HeaderManager":
		// Plan-level OPA correlation headers stay out of the step tree.
		if strings.Contains(strings.ToLower(testname), "correlation") {
			return nil
		}
		// Best-effort: alternating Header.name / Header.value values in the collection.
		vals := colls["HeaderManager.headers"]
		if len(vals) == 0 {
			return nil
		}
		hdrs := []map[string]interface{}{}
		for i := 0; i+1 < len(vals); i += 2 {
			hdrs = append(hdrs, map[string]interface{}{"name": vals[i], "value": vals[i+1]})
		}
		if len(hdrs) == 0 {
			return nil
		}
		return map[string]interface{}{"type": "_headers", "headers": hdrs}
	case "ConstantTimer":
		var think int
		fmt.Sscanf(props["ConstantTimer.delay"], "%d", &think)
		if think > 0 {
			return map[string]interface{}{"type": "think", "name": nz(testname, "Think"), "think_ms": think}
		}
		return nil
	case "UniformRandomTimer":
		var delay, rng int
		fmt.Sscanf(props["ConstantTimer.delay"], "%d", &delay)
		fmt.Sscanf(props["RandomTimer.range"], "%d", &rng)
		if delay > 0 || rng > 0 {
			step := map[string]interface{}{"type": "think", "name": nz(testname, "Think"), "think_ms": delay}
			if rng > 0 {
				step["think_ms_rand"] = delay + rng
			}
			return step
		}
		return nil
	default:
		// Skip Arguments, ThreadGroup scaffolding, etc.
		return nil
	}
}

// extractStepsFromJMXTree finds the ThreadGroup hashTree and returns nested steps
// (HTTP, transaction, if/while/loop, extractors). Falls back to nil when structure
// is unfamiliar so callers can use the flat/loose parser.
func extractStepsFromJMXTree(raw []byte) []map[string]interface{} {
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local != "ThreadGroup" {
			continue
		}
		// Consume ThreadGroup body, then expect its sibling hashTree.
		if _, err := readXMLStringProps(dec, se); err != nil {
			return nil
		}
		for {
			ntok, nerr := dec.Token()
			if nerr != nil {
				return nil
			}
			switch nt := ntok.(type) {
			case xml.CharData, xml.Comment:
				continue
			case xml.StartElement:
				if nt.Name.Local == "hashTree" {
					steps, err := parseJMXHashTreeSteps(dec)
					if err != nil || len(steps) == 0 {
						return nil
					}
					// Drop orphan HeaderManager rows not folded under an HTTP sampler.
					out := steps[:0]
					for _, s := range steps {
						if fmt.Sprint(s["type"]) == "_headers" {
							continue
						}
						out = append(out, s)
					}
					if len(out) == 0 {
						return nil
					}
					return out
				}
				_ = skipXMLElement(dec, nt)
			case xml.EndElement:
				return nil
			}
		}
	}
}
