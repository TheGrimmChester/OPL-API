package main

import (
	"encoding/json"
	"fmt"
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

// flattenScenarioSteps walks a VU tree depth-first into validate/runtime order
// (containers unwrap; HTTP then its nested extract/assert children).
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
