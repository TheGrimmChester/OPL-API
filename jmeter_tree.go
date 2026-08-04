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

func isLogicControllerType(typ string) bool {
	switch typ {
	case "if", "if_controller", "while", "while_controller", "loop", "loop_controller",
		"container", "transaction":
		return true
	default:
		return false
	}
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
func flattenScenarioSteps(steps []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	var walk func([]map[string]interface{})
	walk = func(list []map[string]interface{}) {
		for _, s := range list {
			typ := fmt.Sprint(s["type"])
			if typ == "" || typ == "<nil>" {
				typ = "http"
			}
			kids := stepChildren(s)
			switch typ {
			case "container", "transaction":
				out = append(out, map[string]interface{}{
					"type": "transaction", "name": s["name"], "ok": true,
				})
				walk(kids)
			case "if", "if_controller", "while", "while_controller", "loop", "loop_controller":
				clone := map[string]interface{}{}
				for k, v := range s {
					if k == "children" {
						continue
					}
					clone[k] = v
				}
				out = append(out, clone)
				walk(kids)
			case "http":
				clone := map[string]interface{}{}
				for k, v := range s {
					if k == "children" {
						continue
					}
					clone[k] = v
				}
				out = append(out, clone)
				walk(kids)
			default:
				out = append(out, s)
			}
		}
	}
	walk(steps)
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
	props := map[string]string{}
	depth := 1
	var curName string
	var buf strings.Builder
	inString := false
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return props, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "stringProp" {
				curName = xmlAttr(t, "name")
				buf.Reset()
				inString = true
			} else if t.Name.Local == "boolProp" || t.Name.Local == "intProp" {
				curName = xmlAttr(t, "name")
				buf.Reset()
				inString = true
			}
		case xml.CharData:
			if inString {
				buf.Write(t)
			}
		case xml.EndElement:
			if inString && (t.Name.Local == "stringProp" || t.Name.Local == "boolProp" || t.Name.Local == "intProp") {
				props[curName] = strings.TrimSpace(buf.String())
				inString = false
			}
			depth--
		}
	}
	_ = start
	return props, nil
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
			props, err := readXMLStringProps(dec, t)
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
			step := jmxElementToStep(t.Name.Local, testname, props, kids)
			if step != nil {
				steps = append(steps, step)
			}
		}
	}
}

func jmxElementToStep(local, testname string, props map[string]string, kids []map[string]interface{}) map[string]interface{} {
	switch local {
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
		var cleaned []map[string]interface{}
		for _, k := range kids {
			if fmt.Sprint(k["type"]) == "think" {
				if ms, ok := k["think_ms"]; ok {
					step["think_ms"] = ms
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
		return map[string]interface{}{
			"type": "transaction", "name": nz(testname, "Transaction"), "children": kids,
		}
	case "IfController":
		return map[string]interface{}{
			"type": "if", "name": nz(testname, "If"),
			"condition": props["IfController.condition"], "children": kids,
		}
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
	case "RegexExtractor":
		return map[string]interface{}{
			"type": "extract", "name": nz(testname, props["RegexExtractor.refname"]),
			"engine": "regex", "expression": props["RegexExtractor.regex"],
			"var": props["RegexExtractor.refname"],
		}
	case "JSONPostProcessor":
		return map[string]interface{}{
			"type": "extract", "name": nz(testname, props["JSONPostProcessor.referenceNames"]),
			"engine": "jsonpath", "expression": props["JSONPostProcessor.jsonPathExprs"],
			"var": props["JSONPostProcessor.referenceNames"],
		}
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
		return step
	case "ConstantTimer":
		var think int
		fmt.Sscanf(props["ConstantTimer.delay"], "%d", &think)
		if think > 0 {
			return map[string]interface{}{"type": "think", "name": nz(testname, "Think"), "think_ms": think}
		}
		return nil
	default:
		// Skip HeaderManager, Arguments, ThreadGroup scaffolding, etc.
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
					return steps
				}
				_ = skipXMLElement(dec, nt)
			case xml.EndElement:
				return nil
			}
		}
	}
}
