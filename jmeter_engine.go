package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// generateJMXFromUpsert builds a Classic ThreadGroup JMX from steps or a single URL.
// Used when the visual builder saves without raw jmx_xml — users never need to write JMX.
func generateJMXFromUpsert(name, targetURL, method, body string, vus, dur int, stepsJSON json.RawMessage) string {
	return generateJMXFromUpsertEx(name, targetURL, method, body, vus, dur, stepsJSON, nil)
}

// generateJMXFromUpsertEx builds JMX; when sched has curve_mode=arrivals, emits open-model segments.
func generateJMXFromUpsertEx(name, targetURL, method, body string, vus, dur int, stepsJSON json.RawMessage, sched map[string]interface{}) string {
	return generateJMXFromUpsertData(name, targetURL, method, body, vus, dur, stepsJSON, sched, nil)
}

// generateJMXFromUpsertData is the full entry point: it also wires the scenario CSV dataset
// into the plan so `${column}` tokens are actually bound at run time.
func generateJMXFromUpsertData(name, targetURL, method, body string, vus, dur int, stepsJSON json.RawMessage, sched map[string]interface{}, ds *perfCSVDataset) string {
	rv := perfRendezvousFromSchedule(sched)
	if curveModeFromSchedule(sched) == "arrivals" {
		curve := parseCurveFromSchedule(sched)
		if len(curve) > 0 {
			_, _, _ = applyArrivalsCurveToSchedule(curve, sched)
		}
		segs := arrivalSegmentsFromSched(sched)
		if len(segs) > 0 {
			return generateJMXArrivalsFromUpsert(name, targetURL, method, body, stepsJSON, segs, dur, ds, rv)
		}
	}
	return generateJMXConcurrentFromUpsert(name, targetURL, method, body, vus, dur, stepsJSON, ds, rv, sched)
}

// classicThreadGroupRampSeconds reads schedule.ramp_seconds for Classic ThreadGroup.
// When omitted or ≤0, keeps the historical default of 10 (not a silent 0-ramp).
func classicThreadGroupRampSeconds(sched map[string]interface{}) int {
	if sched != nil {
		if n, ok := asFloat(sched["ramp_seconds"]); ok && int(n) > 0 {
			return int(n)
		}
	}
	return 10
}

func arrivalSegmentsFromSched(sched map[string]interface{}) []arrivalSegment {
	if sched == nil {
		return nil
	}
	raw, ok := sched["arrival_segments"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var segs []arrivalSegment
	if json.Unmarshal(b, &segs) != nil {
		return nil
	}
	out := segs[:0]
	for _, s := range segs {
		if s.Arrivals > 0 && s.RampSec > 0 {
			out = append(out, s)
		}
	}
	return out
}

func generateJMXConcurrentFromUpsert(name, targetURL, method, body string, vus, dur int, stepsJSON json.RawMessage, ds *perfCSVDataset, rv *perfRendezvous, sched map[string]interface{}) string {
	var steps []map[string]interface{}
	_ = json.Unmarshal(stepsJSON, &steps)
	if len(steps) == 0 {
		steps = []map[string]interface{}{{
			"type": "http", "name": "Request", "method": method, "url": targetURL, "body": body,
		}}
	}
	if vus <= 0 {
		vus = 10
	}
	if dur <= 0 {
		dur = 60
	}
	ramp := classicThreadGroupRampSeconds(sched)
	steps, placement := injectPlanRendezvous(steps, rv)
	var b strings.Builder
	writeJMXPlanOpen(&b, name)
	writeJMXCSVDataSet(&b, ds, "      ")
	writeJMXTestFragments(&b, steps, "      ")
	b.WriteString(fmt.Sprintf(`      <ThreadGroup guiclass="ThreadGroupGui" testclass="ThreadGroup" testname="VUs" enabled="true">
        <stringProp name="ThreadGroup.num_threads">%d</stringProp>
        <stringProp name="ThreadGroup.ramp_time">%d</stringProp>
        <boolProp name="ThreadGroup.scheduler">true</boolProp>
        <stringProp name="ThreadGroup.duration">%d</stringProp>
        <elementProp name="ThreadGroup.main_controller" elementType="LoopController">
          <boolProp name="LoopController.continue_forever">true</boolProp>
          <stringProp name="LoopController.loops">-1</stringProp>
        </elementProp>
      </ThreadGroup>`+"\n", vus, ramp, dur))
	b.WriteString("      <hashTree>\n")
	writeJMXCorrelationHeaders(&b, "        ")
	if placement.Mode == "thread_group" {
		writeJMXSyncTimer(&b, rv, "        ")
	}
	ctx := newJMXEmitCtx(name, steps)
	for i, step := range steps {
		appendStepJMXIndexed(&b, step, i, "        ", ctx)
	}
	b.WriteString("      </hashTree>\n")
	writeJMXPlanClose(&b)
	return b.String()
}

// generateJMXArrivalsFromUpsert emits one stock ThreadGroup per arrival segment (open model, loops=1).
func generateJMXArrivalsFromUpsert(name, targetURL, method, body string, stepsJSON json.RawMessage, segs []arrivalSegment, curveDur int, ds *perfCSVDataset, rv *perfRendezvous) string {
	var steps []map[string]interface{}
	_ = json.Unmarshal(stepsJSON, &steps)
	if len(steps) == 0 {
		steps = []map[string]interface{}{{
			"type": "http", "name": "Request", "method": method, "url": targetURL, "body": body,
		}}
	}
	journeyBudget := 120
	if curveDur > 0 {
		journeyBudget = clampPerfDuration(curveDur + 60)
	}
	steps, placement := injectPlanRendezvous(steps, rv)
	var b strings.Builder
	writeJMXPlanOpen(&b, name)
	// Test Plan level: one shared iterator for every arrival segment's threads, and one
	// copy of each reusable journey that every segment's module references point at.
	writeJMXCSVDataSet(&b, ds, "      ")
	writeJMXTestFragments(&b, steps, "      ")
	ctx := newJMXEmitCtx(name, steps)
	for si, seg := range segs {
		if seg.Arrivals <= 0 {
			continue
		}
		ramp := seg.RampSec
		if ramp < 1 {
			ramp = 1
		}
		delay := seg.DelaySec
		if delay < 0 {
			delay = 0
		}
		// Duration is relative to TG start after delay: cover ramp + journey budget.
		tgDur := ramp + journeyBudget
		tgName := fmt.Sprintf("Arrivals %d-%ds", delay, delay+ramp)
		b.WriteString(fmt.Sprintf(`      <ThreadGroup guiclass="ThreadGroupGui" testclass="ThreadGroup" testname=%q enabled="true">
        <stringProp name="ThreadGroup.num_threads">%d</stringProp>
        <stringProp name="ThreadGroup.ramp_time">%d</stringProp>
        <boolProp name="ThreadGroup.scheduler">true</boolProp>
        <stringProp name="ThreadGroup.duration">%d</stringProp>
        <stringProp name="ThreadGroup.delay">%d</stringProp>
        <elementProp name="ThreadGroup.main_controller" elementType="LoopController">
          <boolProp name="LoopController.continue_forever">false</boolProp>
          <stringProp name="LoopController.loops">1</stringProp>
        </elementProp>
      </ThreadGroup>`+"\n", xmlEscape(tgName), seg.Arrivals, ramp, tgDur, delay))
		b.WriteString("      <hashTree>\n")
		if si == 0 {
			writeJMXCorrelationHeaders(&b, "        ")
		}
		if placement.Mode == "thread_group" {
			writeJMXSyncTimer(&b, rv, "        ")
		}
		for i, step := range steps {
			appendStepJMXIndexed(&b, step, i, "        ", ctx)
		}
		b.WriteString("      </hashTree>\n")
	}
	writeJMXPlanClose(&b)
	return b.String()
}

func writeJMXPlanOpen(b *strings.Builder, name string) {
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<jmeterTestPlan version="1.2" properties="5.0" jmeter="5.5">` + "\n")
	b.WriteString("  <hashTree>\n")
	b.WriteString(`    <TestPlan guiclass="TestPlanGui" testclass="TestPlan" testname="` + xmlEscape(perfPlanNodeName(name)) + `" enabled="true"/>` + "\n")
	b.WriteString("    <hashTree>\n")
	b.WriteString(`      <Arguments guiclass="ArgumentsPanel" testclass="Arguments" testname="OPA Vars" enabled="true">
        <collectionProp name="Arguments.arguments">
          <elementProp name="LOAD_RUN_ID" elementType="Argument">
            <stringProp name="Argument.name">LOAD_RUN_ID</stringProp>
            <stringProp name="Argument.value">${__P(LOAD_RUN_ID,)}</stringProp>
          </elementProp>
        </collectionProp>
      </Arguments>
      <hashTree/>` + "\n")
}

func writeJMXPlanClose(b *strings.Builder) {
	b.WriteString("    </hashTree>\n")
	b.WriteString("  </hashTree>\n")
	b.WriteString("</jmeterTestPlan>\n")
}

func writeJMXCorrelationHeaders(b *strings.Builder, indent string) {
	b.WriteString(indent + `<HeaderManager guiclass="HeaderPanel" testclass="HeaderManager" testname="OPA Correlation Headers" enabled="true">
` + indent + `  <collectionProp name="HeaderManager.headers">
` + indent + `    <elementProp name="" elementType="Header">
` + indent + `      <stringProp name="Header.name">X-OPA-Load-Run-Id</stringProp>
` + indent + `      <stringProp name="Header.value">${LOAD_RUN_ID}</stringProp>
` + indent + `    </elementProp>
` + indent + `    <elementProp name="" elementType="Header">
` + indent + `      <stringProp name="Header.name">baggage</stringProp>
` + indent + `      <stringProp name="Header.value">load_run_id=${LOAD_RUN_ID}</stringProp>
` + indent + `    </elementProp>
` + indent + `  </collectionProp>
` + indent + `</HeaderManager>
` + indent + `<hashTree/>` + "\n")
}

// jmxEmitCtx carries the plan-level facts step emission needs: the Test Plan node name
// (first element of every ModuleController node path) and the fragment index used to
// decide whether a reference becomes a module reference or an inline copy.
type jmxEmitCtx struct {
	planName string
	frags    map[string]map[string]interface{}
	counts   map[string]int
}

// newJMXEmitCtx indexes a VU tree's fragment definitions for one plan.
func newJMXEmitCtx(planName string, steps []map[string]interface{}) *jmxEmitCtx {
	return &jmxEmitCtx{
		planName: planName,
		frags:    indexFragmentsByName(steps),
		counts:   perfFragmentNameCounts(steps),
	}
}

func appendStepJMXIndexed(b *strings.Builder, step map[string]interface{}, i int, indent string, ctx *jmxEmitCtx) {
	if ctx == nil {
		ctx = &jmxEmitCtx{}
	}
	typ := fmt.Sprint(step["type"])
	if typ == "" || typ == "<nil>" {
		typ = "http"
	}
	name := nz(fmt.Sprint(step["name"]), fmt.Sprintf("step-%d", i+1))
	en := jmxEnabledAttr(step)
	kids := stepChildren(step)
	emitKids := func(list []map[string]interface{}, childIndent string) {
		for j, child := range list {
			appendStepJMXIndexed(b, child, j, childIndent, ctx)
		}
	}
	switch typ {
	case "extract":
		expr := fmt.Sprint(step["expression"])
		vname := fmt.Sprint(step["var"])
		engine := fmt.Sprint(step["engine"])
		matchNum := 1
		if n, ok := asFloat(step["match_number"]); ok {
			matchNum = int(n)
		}
		template := "$1$"
		if t := strings.TrimSpace(fmt.Sprint(step["template"])); t != "" && t != "<nil>" {
			template = t
		}
		defaultVal := ""
		if v, ok := step["default_value"]; ok {
			defaultVal = fmt.Sprint(v)
			if defaultVal == "<nil>" {
				defaultVal = ""
			}
		}
		if engine == "jsonpath" || strings.HasPrefix(expr, "$.") {
			b.WriteString(fmt.Sprintf(`%s<JSONPostProcessor guiclass="JSONPostProcessorGui" testclass="JSONPostProcessor" testname=%q enabled=%q>
%s  <stringProp name="JSONPostProcessor.referenceNames">%s</stringProp>
%s  <stringProp name="JSONPostProcessor.jsonPathExprs">%s</stringProp>
%s  <stringProp name="JSONPostProcessor.match_numbers">%d</stringProp>
%s  <stringProp name="JSONPostProcessor.defaultValues">%s</stringProp>
%s</JSONPostProcessor>
%s<hashTree/>`+"\n", indent, xmlEscape(name), en, indent, xmlEscape(vname), indent, xmlEscape(expr), indent, matchNum, indent, xmlEscape(defaultVal), indent, indent))
		} else {
			b.WriteString(fmt.Sprintf(`%s<RegexExtractor guiclass="RegexExtractorGui" testclass="RegexExtractor" testname=%q enabled=%q>
%s  <stringProp name="RegexExtractor.refname">%s</stringProp>
%s  <stringProp name="RegexExtractor.regex">%s</stringProp>
%s  <stringProp name="RegexExtractor.template">%s</stringProp>
%s  <stringProp name="RegexExtractor.match_number">%d</stringProp>
%s  <stringProp name="RegexExtractor.default">%s</stringProp>
%s</RegexExtractor>
%s<hashTree/>`+"\n", indent, xmlEscape(name), en, indent, xmlEscape(vname), indent, xmlEscape(expr), indent, xmlEscape(template), indent, matchNum, indent, xmlEscape(defaultVal), indent, indent))
		}
	case "assert":
		if st, ok := step["status"]; ok {
			field := jmeterAssertField(step, "Assertion.response_code")
			testType := jmeterAssertTestType(step, 8)
			assume := jmeterAssumeSuccess(step, false)
			b.WriteString(fmt.Sprintf(`%s<ResponseAssertion guiclass="AssertionGui" testclass="ResponseAssertion" testname=%q enabled=%q>
%s  <collectionProp name="Asserion.test_strings">
%s    <stringProp name="0">%v</stringProp>
%s  </collectionProp>
%s  <stringProp name="Assertion.test_field">%s</stringProp>
%s  <boolProp name="Assertion.assume_success">%t</boolProp>
%s  <intProp name="Assertion.test_type">%d</intProp>
%s</ResponseAssertion>
%s<hashTree/>`+"\n", indent, xmlEscape(name), en, indent, indent, st, indent, indent, field, indent, assume, indent, testType, indent, indent))
		}
		if contains, ok := step["body_contains"].(string); ok && contains != "" {
			field := jmeterAssertField(step, "Assertion.response_data")
			testType := jmeterAssertTestType(step, 2)
			assume := jmeterAssumeSuccess(step, false)
			b.WriteString(fmt.Sprintf(`%s<ResponseAssertion guiclass="AssertionGui" testclass="ResponseAssertion" testname=%q enabled=%q>
%s  <collectionProp name="Asserion.test_strings">
%s    <stringProp name="0">%s</stringProp>
%s  </collectionProp>
%s  <stringProp name="Assertion.test_field">%s</stringProp>
%s  <boolProp name="Assertion.assume_success">%t</boolProp>
%s  <intProp name="Assertion.test_type">%d</intProp>
%s</ResponseAssertion>
%s<hashTree/>`+"\n", indent, xmlEscape(name+" body"), en, indent, indent, xmlEscape(contains), indent, indent, field, indent, assume, indent, testType, indent, indent))
		}
	case "container", "transaction":
		includeTimers := false
		if v, ok := step["include_timers"].(bool); ok {
			includeTimers = v
		}
		genParent := false
		if v, ok := step["generate_parent_sample"].(bool); ok {
			genParent = v
		}
		b.WriteString(fmt.Sprintf(`%s<TransactionController guiclass="TransactionControllerGui" testclass="TransactionController" testname=%q enabled=%q>
%s  <boolProp name="TransactionController.includeTimers">%t</boolProp>
%s  <boolProp name="TransactionController.parent">%t</boolProp>
%s</TransactionController>
%s<hashTree>`+"\n", indent, xmlEscape(name), en, indent, includeTimers, indent, genParent, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "if", "if_controller":
		cond := nz(fmt.Sprint(step["condition"]), `${__jexl3(true)}`)
		if cond == "<nil>" {
			cond = `${__jexl3(true)}`
		}
		evalAll := false
		if v, ok := step["evaluate_all"].(bool); ok {
			evalAll = v
		}
		useExpr := true
		if v, ok := step["use_expression"].(bool); ok {
			useExpr = v
		}
		b.WriteString(fmt.Sprintf(`%s<IfController guiclass="IfControllerPanel" testclass="IfController" testname=%q enabled=%q>
%s  <stringProp name="IfController.condition">%s</stringProp>
%s  <boolProp name="IfController.evaluateAll">%t</boolProp>
%s  <boolProp name="IfController.useExpression">%t</boolProp>
%s</IfController>
%s<hashTree>`+"\n", indent, xmlEscape(name), en, indent, xmlEscape(cond), indent, evalAll, indent, useExpr, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "while", "while_controller":
		cond := nz(fmt.Sprint(step["condition"]), `${__jexl3(false)}`)
		if cond == "<nil>" {
			cond = `${__jexl3(false)}`
		}
		b.WriteString(fmt.Sprintf(`%s<WhileController guiclass="WhileControllerGui" testclass="WhileController" testname=%q enabled=%q>
%s  <stringProp name="WhileController.condition">%s</stringProp>
%s</WhileController>
%s<hashTree>`+"\n", indent, xmlEscape(name), en, indent, xmlEscape(cond), indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "loop", "loop_controller":
		loops := 1
		if n, ok := asFloat(step["loops"]); ok && int(n) > 0 {
			loops = int(n)
		}
		forever := false
		if v, ok := step["forever"].(bool); ok {
			forever = v
		}
		b.WriteString(fmt.Sprintf(`%s<LoopController guiclass="LoopControlPanel" testclass="LoopController" testname=%q enabled=%q>
%s  <boolProp name="LoopController.continue_forever">%t</boolProp>
%s  <stringProp name="LoopController.loops">%d</stringProp>
%s</LoopController>
%s<hashTree>`+"\n", indent, xmlEscape(name), en, indent, forever, indent, loops, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "foreach", "foreach_controller", "for_each":
		inputVar := nz(fmt.Sprint(step["input_var"]), "items")
		if inputVar == "<nil>" {
			inputVar = "items"
		}
		returnVar := nz(fmt.Sprint(step["return_var"]), "item")
		if returnVar == "<nil>" {
			returnVar = "item"
		}
		useSep := true
		if v, ok := step["use_separator"].(bool); ok {
			useSep = v
		}
		b.WriteString(fmt.Sprintf(`%s<ForeachController guiclass="ForeachControlPanel" testclass="ForeachController" testname=%q enabled=%q>
%s  <stringProp name="ForeachController.inputVal">%s</stringProp>
%s  <stringProp name="ForeachController.returnVal">%s</stringProp>
%s  <boolProp name="ForeachController.useSeparator">%t</boolProp>
%s</ForeachController>
%s<hashTree>`+"\n", indent, xmlEscape(name), en, indent, xmlEscape(inputVar), indent, xmlEscape(returnVar), indent, useSep, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "fragment":
		// A definition, not part of the flow: writeJMXTestFragments emits it once at
		// Test Plan level as a disabled TestFragmentController that module references
		// point at, so it is not duplicated into every thread group.
		return
	case "rendezvous":
		writeJMXSyncTimer(b, perfRendezvousFromStep(step), indent)
	case "params":
		pnames, pvals := perfStepParams(step)
		writeJMXUserParameters(b, name, pnames, pvals, indent)
	case "include", "link":
		decision := perfFragmentRefDecision(step, ctx.planName, ctx.frags, ctx.counts)
		pnames, pvals := perfStepParams(step)
		if decision.Mode == perfFragmentModeModule {
			if len(pnames) == 0 {
				writeJMXModuleController(b, nz(perfStepName(step), decision.Ref), decision.NodePath, indent)
				return
			}
			// Inputs are scoped to this reference by wrapping it: the pre-processor
			// then applies only to the samplers this module reference contributes.
			writeJMXModuleParamScope(b, decision, pnames, pvals, indent, func(inner string) {
				writeJMXModuleController(b, nz(perfStepName(step), decision.Ref), decision.NodePath, inner)
			})
			return
		}
		if decision.Mode == perfFragmentModeUnresolved {
			b.WriteString(fmt.Sprintf("%s<!-- opl-include ref=%s mode=%s reason=%s -->\n",
				indent, xmlCommentSafe(decision.Ref), perfFragmentModeUnresolved, xmlCommentSafe(decision.Reason)))
			return
		}
		expanded := kids
		if len(expanded) == 0 {
			expanded = resolveIncludeSteps(step, ctx.frags)
		}
		b.WriteString(fmt.Sprintf("%s<!-- opl-include ref=%s mode=%s reason=%s -->\n",
			indent, xmlCommentSafe(decision.Ref), perfFragmentModeInline, xmlCommentSafe(decision.Reason)))
		if len(pnames) == 0 {
			emitKids(expanded, indent)
			return
		}
		writeJMXModuleParamScope(b, decision, pnames, pvals, indent, func(inner string) {
			emitKids(expanded, inner)
		})
	default: // http
		method := nz(fmt.Sprint(step["method"]), "GET")
		urlStr := fmt.Sprint(step["url"])
		if selRaw, ok := step["selector"]; ok {
			sel := strings.TrimSpace(fmt.Sprint(selRaw))
			if sel != "" && sel != "<nil>" {
				stype := "css"
				if t, ok := step["selector_type"]; ok {
					if ts := strings.TrimSpace(fmt.Sprint(t)); ts != "" && ts != "<nil>" {
						stype = ts
					}
				}
				page := ""
				if p, ok := step["page_url"]; ok {
					if ps := strings.TrimSpace(fmt.Sprint(p)); ps != "" && ps != "<nil>" {
						page = ps
					}
				}
				action := ""
				if a, ok := step["ui_action"]; ok {
					if as := strings.TrimSpace(fmt.Sprint(a)); as != "" && as != "<nil>" {
						action = as
					}
				}
				b.WriteString(fmt.Sprintf("%s<!-- opa-ui type=%s selector=%s page=%s action=%s -->\n",
					indent, xmlCommentSafe(stype), xmlCommentSafe(sel), xmlCommentSafe(page), xmlCommentSafe(action)))
			}
		}
		domain, path, proto, port := "127.0.0.1", "/", "http", ""
		if strings.HasPrefix(urlStr, "http") {
			proto = "http"
			if strings.HasPrefix(urlStr, "https") {
				proto = "https"
			}
			rest := strings.TrimPrefix(strings.TrimPrefix(urlStr, "https://"), "http://")
			slash := strings.Index(rest, "/")
			hostport := rest
			if slash >= 0 {
				hostport = rest[:slash]
				path = rest[slash:]
			} else {
				path = "/"
			}
			if colon := strings.Index(hostport, ":"); colon >= 0 {
				domain = hostport[:colon]
				port = hostport[colon+1:]
			} else {
				domain = hostport
			}
		} else if urlStr != "" && urlStr != "<nil>" {
			path = urlStr
		}
		body := fmt.Sprint(step["body"])
		if body == "<nil>" {
			body = ""
		}
		alwaysEncode := false
		if v, ok := step["always_encode"].(bool); ok {
			alwaysEncode = v
		}
		bodyXML := ""
		if body != "" {
			bodyXML = fmt.Sprintf(`
%s  <boolProp name="HTTPSampler.postBodyRaw">true</boolProp>
%s  <elementProp name="HTTPsampler.Arguments" elementType="Arguments">
%s    <collectionProp name="Arguments.arguments">
%s      <elementProp name="" elementType="HTTPArgument">
%s        <boolProp name="HTTPArgument.always_encode">%t</boolProp>
%s        <stringProp name="Argument.value">%s</stringProp>
%s        <stringProp name="Argument.metadata">=</stringProp>
%s      </elementProp>
%s    </collectionProp>
%s  </elementProp>`, indent, indent, indent, indent, indent, alwaysEncode, indent, xmlEscape(body), indent, indent, indent, indent)
		}
		follow := true
		if v, ok := step["follow_redirects"].(bool); ok {
			follow = v
		}
		extraProps := ""
		if n, ok := asFloat(step["connect_timeout_ms"]); ok && int(n) > 0 {
			extraProps += fmt.Sprintf("\n%s  <stringProp name=\"HTTPSampler.connect_timeout\">%d</stringProp>", indent, int(n))
		}
		if n, ok := asFloat(step["response_timeout_ms"]); ok && int(n) > 0 {
			extraProps += fmt.Sprintf("\n%s  <stringProp name=\"HTTPSampler.response_timeout\">%d</stringProp>", indent, int(n))
		}
		b.WriteString(fmt.Sprintf(`%s<HTTPSamplerProxy guiclass="HttpTestSampleGui" testclass="HTTPSamplerProxy" testname=%q enabled=%q>
%s  <stringProp name="HTTPSampler.domain">%s</stringProp>
%s  <stringProp name="HTTPSampler.port">%s</stringProp>
%s  <stringProp name="HTTPSampler.protocol">%s</stringProp>
%s  <stringProp name="HTTPSampler.path">%s</stringProp>
%s  <stringProp name="HTTPSampler.method">%s</stringProp>
%s  <boolProp name="HTTPSampler.follow_redirects">%t</boolProp>%s%s
%s</HTTPSamplerProxy>
%s<hashTree>`+"\n", indent, xmlEscape(name), en, indent, xmlEscape(domain), indent, xmlEscape(port), indent, xmlEscape(proto), indent, xmlEscape(path), indent, xmlEscape(method), indent, follow, bodyXML, extraProps, indent, indent))
		// Per-step headers → HeaderManager under this sampler (before extract/assert).
		// Plan-level OPA correlation HeaderManager stays separate at ThreadGroup scope.
		writeJMXStepHeaderManager(b, step, indent+"  ")
		emitKids(kids, indent+"  ")
		think := 0
		if t, ok := asFloat(step["think_ms"]); ok {
			think = int(t)
		}
		thinkRand := 0
		if t, ok := asFloat(step["think_ms_rand"]); ok {
			thinkRand = int(t)
		}
		if thinkRand > think {
			rng := thinkRand - think
			b.WriteString(fmt.Sprintf(`%s  <UniformRandomTimer guiclass="UniformRandomTimerGui" testclass="UniformRandomTimer" testname="Think" enabled=%q>
%s    <stringProp name="ConstantTimer.delay">%d</stringProp>
%s    <stringProp name="RandomTimer.range">%d</stringProp>
%s  </UniformRandomTimer>
%s  <hashTree/>`+"\n", indent, en, indent, think, indent, rng, indent, indent))
		} else if think > 0 {
			b.WriteString(fmt.Sprintf(`%s  <ConstantTimer guiclass="ConstantTimerGui" testclass="ConstantTimer" testname="Think" enabled=%q>
%s    <stringProp name="ConstantTimer.delay">%d</stringProp>
%s  </ConstantTimer>
%s  <hashTree/>`+"\n", indent, en, indent, think, indent, indent))
		}
		b.WriteString(indent + "</hashTree>\n")
	}
}

// stepEnabled reports whether a step should run (default true when omitted).
func stepEnabled(step map[string]interface{}) bool {
	if step == nil {
		return true
	}
	v, ok := step["enabled"]
	if !ok || v == nil {
		return true
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s != "false" && s != "0" && s != "no"
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return true
	}
}

func jmxEnabledAttr(step map[string]interface{}) string {
	if stepEnabled(step) {
		return "true"
	}
	return "false"
}

// stepHTTPHeaderPairs normalizes headers from a map or [{name,value}] array.
func stepHTTPHeaderPairs(step map[string]interface{}) [][2]string {
	raw, ok := step["headers"]
	if !ok || raw == nil {
		return nil
	}
	var out [][2]string
	switch t := raw.(type) {
	case map[string]interface{}:
		for k, v := range t {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			out = append(out, [2]string{k, fmt.Sprint(v)})
		}
	case map[string]string:
		for k, v := range t {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			out = append(out, [2]string{k, v})
		}
	case []interface{}:
		for _, item := range t {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			name := strings.TrimSpace(fmt.Sprint(m["name"]))
			if name == "" || name == "<nil>" {
				continue
			}
			out = append(out, [2]string{name, fmt.Sprint(m["value"])})
		}
	case []map[string]interface{}:
		for _, m := range t {
			name := strings.TrimSpace(fmt.Sprint(m["name"]))
			if name == "" || name == "<nil>" {
				continue
			}
			out = append(out, [2]string{name, fmt.Sprint(m["value"])})
		}
	}
	return out
}

func writeJMXStepHeaderManager(b *strings.Builder, step map[string]interface{}, indent string) {
	hdrs := stepHTTPHeaderPairs(step)
	if len(hdrs) == 0 {
		return
	}
	b.WriteString(indent + `<HeaderManager guiclass="HeaderPanel" testclass="HeaderManager" testname="HTTP Headers" enabled="true">` + "\n")
	b.WriteString(indent + `  <collectionProp name="HeaderManager.headers">` + "\n")
	for _, hv := range hdrs {
		b.WriteString(indent + `    <elementProp name="" elementType="Header">` + "\n")
		b.WriteString(fmt.Sprintf("%s      <stringProp name=\"Header.name\">%s</stringProp>\n", indent, xmlEscape(hv[0])))
		b.WriteString(fmt.Sprintf("%s      <stringProp name=\"Header.value\">%s</stringProp>\n", indent, xmlEscape(hv[1])))
		b.WriteString(indent + `    </elementProp>` + "\n")
	}
	b.WriteString(indent + `  </collectionProp>` + "\n")
	b.WriteString(indent + `</HeaderManager>` + "\n")
	b.WriteString(indent + `<hashTree/>` + "\n")
}

func jmeterAssertTestType(step map[string]interface{}, def int) int {
	at := strings.ToLower(strings.TrimSpace(fmt.Sprint(step["assert_type"])))
	switch at {
	case "contains":
		return 1
	case "equals":
		return 8
	case "regex", "matches":
		return 2
	case "", "<nil>":
		return def
	default:
		return def
	}
}

func jmeterAssertField(step map[string]interface{}, def string) string {
	af := strings.ToLower(strings.TrimSpace(fmt.Sprint(step["assert_field"])))
	switch af {
	case "response_code", "assertion.response_code":
		return "Assertion.response_code"
	case "response_data", "assertion.response_data":
		return "Assertion.response_data"
	case "response_headers", "assertion.response_headers":
		return "Assertion.response_headers"
	case "", "<nil>":
		return def
	default:
		if strings.HasPrefix(af, "assertion.") {
			return "Assertion." + strings.TrimPrefix(af, "assertion.")
		}
		return def
	}
}

func jmeterAssumeSuccess(step map[string]interface{}, def bool) bool {
	if v, ok := step["assume_success"].(bool); ok {
		return v
	}
	return def
}

// jmeterAvailable reports whether Docker (default) or host JMeter (dev-only) can run.
func jmeterAvailable() (mode string, ok bool) {
	return resolveJMeterEngine()
}

// dispatchJMeterRun writes JMX and runs Apache JMeter via ephemeral Docker container(s)
// (production path). Host bin requires OPA_PERF_ALLOW_HOST_JMETER=1.
func dispatchJMeterRun(scenarioID, runID string, vus int, org, proj string) map[string]interface{} {
	return dispatchJMeterRunScaled(scenarioID, runID, vus, 0, org, proj)
}

// dispatchJMeterRunScaled fans VUs across N ephemeral JMeter containers when workers>1.
func dispatchJMeterRunScaled(scenarioID, runID string, vus, workers int, org, proj string) map[string]interface{} {
	mode, ok := jmeterAvailable()
	if !ok {
		err := "Apache JMeter Docker runner unavailable — install docker (OPA_JMETER_IMAGE) or set OPA_PERF_ALLOW_HOST_JMETER=1 with OPA_JMETER_BIN."
		if nodePerfFallbackAllowed() {
			return map[string]interface{}{
				"dispatched": false, "engine": "jmeter", "error": err,
				"fallback": "node", "tip": "OPA_PERF_ALLOW_NODE_FALLBACK=1 enables Node load-runner (dev-only).",
			}
		}
		return map[string]interface{}{
			"dispatched": false, "engine": "jmeter", "error": err,
			"honesty": "Production path is Docker; host bin and Node fallback are opt-in dev escapes.",
		}
	}
	sc := loadScenarioMapForTenant(scenarioID, org, proj)
	if sc == nil {
		return map[string]interface{}{"dispatched": false, "error": "scenario not found"}
	}
	jmx := getString(sc, "jmx_xml")
	stepsRaw := getString(sc, "steps_json")
	// Generated/simple scenarios: enforce URL policy on target + http steps before dispatch.
	// Raw JMX may still reach arbitrary hosts via HTTPSampler unless unsafe elements are blocked.
	if strings.TrimSpace(stepsRaw) != "" || strings.TrimSpace(getString(sc, "target_url")) != "" {
		if err := perfScenarioHTTPURLsBlocked(sc); err != nil {
			return map[string]interface{}{
				"dispatched": false,
				"error":      "url policy: " + err.Error(),
				"honesty":    "Raw JMX HTTPSampler hosts are not fully pinned; unsafe script/OS samplers are blocked.",
			}
		}
	}
	schedMap := map[string]interface{}{}
	if s := getString(sc, "schedule_json"); s != "" && s != "{}" {
		_ = json.Unmarshal([]byte(s), &schedMap)
	}
	arrivalsMode := curveModeFromSchedule(schedMap) == "arrivals"
	if arrivalsMode {
		vus = clampPerfArrivals(vus)
		if vus <= 0 {
			vus = 1
		}
	} else {
		vus = clampPerfVUs(vus)
	}
	nWorkers := perfJMeterWorkers(workers)
	if nWorkers > vus {
		nWorkers = vus
	}
	if nWorkers < 1 {
		nWorkers = 1
	}
	dur := clampPerfDuration(int(getFloat64(sc, "duration_seconds")))
	if curve := parseCurveFromSchedule(schedMap); len(curve) > 0 {
		peak, cDur, _ := applyLoadCurveToSchedule(curve, schedMap)
		if cDur > 0 {
			dur = clampPerfDuration(cDur)
		}
		if peak > 0 && arrivalsMode {
			vus = peak
			if nWorkers > vus {
				nWorkers = vus
			}
			if nWorkers < 1 {
				nWorkers = 1
			}
		}
	}
	steps := json.RawMessage(stepsRaw)
	if len(steps) == 0 {
		steps = json.RawMessage(`[]`)
	}
	dataset := perfCSVDatasetFromJSON(getString(sc, "datasets_json"))
	// Arrivals mode always regenerates open-model JMX from steps (ignore stale classic jmx_xml).
	if arrivalsMode {
		jmx = generateJMXFromUpsertData(getString(sc, "name"), getString(sc, "target_url"), getString(sc, "method"), getString(sc, "body"),
			vus, dur, steps, schedMap, dataset)
	} else if strings.TrimSpace(jmx) == "" {
		jmx = generateJMXFromUpsertData(getString(sc, "name"), getString(sc, "target_url"), getString(sc, "method"), getString(sc, "body"),
			vus, dur, steps, schedMap, dataset)
	}
	// Plans stored before the engine emitted CSVDataSet (and raw imported JMX) get the element
	// wired in here — otherwise the data file is written but never read. An element that is
	// already there is re-emitted from datasets_json so the run matches the saved dataset.
	jmx, datasetInjected := syncJMXCSVDataSet(jmx, dataset)
	if jmxContainsUnsafeElements(jmx) {
		return map[string]interface{}{"dispatched": false, "error": "jmx contains unsafe elements"}
	}

	root := perfJMeterWorkRoot()
	workerDirs := make([]string, 0, nWorkers)
	workerVUs := splitVUsAcrossWorkers(vus, nWorkers)
	datasetSharded := false

	for i := 0; i < nWorkers; i++ {
		rel := runID
		if nWorkers > 1 {
			rel = fmt.Sprintf("%s-w%d", runID, i)
		}
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return map[string]interface{}{"dispatched": false, "error": err.Error()}
		}
		workerJMX := jmx
		if arrivalsMode && nWorkers > 1 {
			workerSegs := scaleArrivalSegments(arrivalSegmentsFromSched(schedMap), workerVUs[i], vus)
			workerJMX = generateJMXArrivalsFromUpsert(getString(sc, "name"), getString(sc, "target_url"), getString(sc, "method"), getString(sc, "body"),
				steps, workerSegs, dur, dataset, perfRendezvousFromSchedule(schedMap))
		} else if !arrivalsMode {
			workerJMX = rewriteJMXThreadCount(jmx, workerVUs[i])
		}
		if err := os.WriteFile(filepath.Join(dir, "plan.jmx"), []byte(workerJMX), 0o600); err != nil {
			return map[string]interface{}{"dispatched": false, "error": err.Error()}
		}
		// data.csv is written with the scenario delimiter and matches the emitted CSVDataSet.
		if content, sharded := dataset.workerCSV(i, nWorkers); content != "" {
			if err := os.WriteFile(filepath.Join(dir, perfCSVDataFile), []byte(content), 0o600); err != nil {
				return map[string]interface{}{"dispatched": false, "error": err.Error()}
			}
			datasetSharded = datasetSharded || sharded
		}
		workerDirs = append(workerDirs, dir)
	}

	handles := make([]PerfContainerHandle, 0, nWorkers)
	containerNames := make([]string, 0, nWorkers)
	image := envOr("OPA_JMETER_IMAGE", "justb4/jmeter:5.5")

	if mode == "docker" {
		runner := defaultPerfContainerRunner
		if runner == nil {
			runner = DockerRunner{}
		}
		for i := 0; i < nWorkers; i++ {
			rel := filepath.Base(workerDirs[i])
			h, err := runner.RunJMeter(PerfJMeterRunSpec{
				RunID: runID, WorkerIndex: i, Workers: nWorkers, VUs: workerVUs[i],
				WorkDir: workerDirs[i], WorkRel: rel, Image: image,
			})
			if err != nil {
				return map[string]interface{}{
					"dispatched": false, "error": err.Error(), "mode": mode,
					"workers_started": i, "image": image,
				}
			}
			handles = append(handles, h)
			containerNames = append(containerNames, h.Name)
		}
	} else {
		// Dev-only host JMeter (OPA_PERF_ALLOW_HOST_JMETER=1).
		bin := envOr("OPA_JMETER_BIN", "jmeter")
		for i := 0; i < nWorkers; i++ {
			dir := workerDirs[i]
			jmxPath := filepath.Join(dir, "plan.jmx")
			jtlPath := filepath.Join(dir, "results.jtl")
			logPath := filepath.Join(dir, "jmeter.log")
			cmd := exec.Command(bin, "-n", "-t", jmxPath, "-l", jtlPath, "-j", logPath, "-JLOAD_RUN_ID="+runID)
			cmd.Dir = dir
			if err := cmd.Start(); err != nil {
				return map[string]interface{}{"dispatched": false, "error": err.Error(), "mode": mode}
			}
			p := cmd.Process
			handles = append(handles, PerfContainerHandle{
				ID: fmt.Sprintf("%d", p.Pid), Name: "host-jmeter",
				Wait: func() error { _, err := p.Wait(); return err },
			})
		}
	}

	go func() {
		timeout := time.Duration(dur+120) * time.Second
		if timeout < 3*time.Minute {
			timeout = 3 * time.Minute
		}
		for _, h := range handles {
			_ = waitWithTimeout(h, timeout)
		}
		var mergedSummary map[string]interface{}
		var mergedSamples []map[string]interface{}
		for _, dir := range workerDirs {
			sum, samples := parseJTLFile(filepath.Join(dir, "results.jtl"))
			mergedSummary = mergeJMeterSummaries(mergedSummary, sum)
			mergedSamples = append(mergedSamples, samples...)
		}
		if mergedSummary == nil {
			mergedSummary = map[string]interface{}{"requests": 0, "error_rate": 0.0, "engine": "jmeter"}
		}
		mergedSummary["engine"] = "jmeter"
		mergedSummary["mode"] = mode
		mergedSummary["workers"] = nWorkers
		mergedSummary["image"] = image
		if len(containerNames) > 0 {
			mergedSummary["containers"] = containerNames
		}
		if len(mergedSamples) > 500 {
			mergedSamples = mergedSamples[:500]
		}
		stampRunIDOnSamples(runID, org, proj, mergedSamples)
		sla := map[string]interface{}{}
		if s := getString(sc, "sla_json"); s != "" && s != "{}" {
			_ = json.Unmarshal([]byte(s), &sla)
		} else if s := getString(sc, "thresholds_json"); s != "" {
			_ = json.Unmarshal([]byte(s), &sla)
		}
		pass, reasons := evaluateSLAFailClosed(mergedSummary, sla)
		status := "completed"
		if !pass {
			status = "failed"
			mergedSummary["sla_reasons"] = reasons
		} else if len(sla) > 0 {
			status = "passed"
		}
		now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
		sum, _ := json.Marshal(mergedSummary)
		payload, _ := json.Marshal(map[string]interface{}{
			"id": runID, "organization_id": org, "project_id": proj,
			"scenario_id": scenarioID, "status": status, "vus": vus,
			"started_at": now, "finished_at": now, "summary_json": string(sum),
			"error": strings.Join(reasons, "; "),
		})
		if writer != nil {
			writer.insertAsync("load_runs", append(payload, '\n'))
			for _, s := range mergedSamples {
				samp, _ := json.Marshal(s)
				writer.insertAsync("load_run_samples", append(samp, '\n'))
			}
		}
		notifyRunTerminal(runNotifyEvent{
			RunID: runID, ScenarioID: scenarioID, OrganizationID: org, ProjectID: proj,
			Status: status, VUs: vus, Error: strings.Join(reasons, "; "), Summary: mergedSummary,
			FinishedAt: now, Source: "jmeter",
		})
		clearRunContainers(runID)
	}()

	out := map[string]interface{}{
		"dispatched": true, "engine": "jmeter", "mode": mode,
		"workers": nWorkers, "worker_vus": workerVUs, "image": image,
		"containers": containerNames, "work_dirs": workerDirs,
		"honesty": "Ephemeral Apache JMeter container(s); JTL merged into load_run_samples. Host bin is OPA_PERF_ALLOW_HOST_JMETER=1 only.",
	}
	if dataset != nil {
		out["dataset"] = dataset.summary()
		out["dataset_injected"] = datasetInjected
		out["dataset_sharded"] = datasetSharded
		if datasetSharded {
			out["dataset_honesty"] = fmt.Sprintf("data.csv rows sharded round-robin across %d worker(s) so each row is used once per pass.", nWorkers)
		} else if nWorkers > 1 && dataset.rowCount() > 0 {
			out["dataset_honesty"] = "Fewer rows than workers — every worker got the full data.csv, so rows repeat across containers."
		}
	}
	if unbound, resolvable := scenarioUnboundVariables(sc); len(unbound) > 0 {
		out["unbound_variables"] = unbound
		out["warning"] = "Unbound ${…} tokens will be sent as literal text: " + strings.Join(unbound, ", ")
	} else if !resolvable {
		out["unbound_variables"] = []string{}
		out["dataset_columns_unknown"] = true
	}
	return out
}

// splitVUsAcrossWorkers distributes VUs as evenly as possible across N workers.
func splitVUsAcrossWorkers(vus, workers int) []int {
	if workers < 1 {
		workers = 1
	}
	out := make([]int, workers)
	base := vus / workers
	rem := vus % workers
	for i := 0; i < workers; i++ {
		out[i] = base
		if i < rem {
			out[i]++
		}
		if out[i] < 1 {
			out[i] = 1
		}
	}
	return out
}

// scaleArrivalSegments proportionally assigns segment arrivals to one worker's share of total.
func scaleArrivalSegments(segs []arrivalSegment, workerShare, total int) []arrivalSegment {
	if len(segs) == 0 || total <= 0 || workerShare <= 0 {
		return nil
	}
	out := make([]arrivalSegment, 0, len(segs))
	assigned := 0
	for i, s := range segs {
		n := int(math.Round(float64(s.Arrivals) * float64(workerShare) / float64(total)))
		if i == len(segs)-1 {
			n = workerShare - assigned
		}
		if n < 0 {
			n = 0
		}
		assigned += n
		if n == 0 {
			continue
		}
		s.Arrivals = n
		out = append(out, s)
	}
	if assigned < workerShare && len(out) > 0 {
		out[len(out)-1].Arrivals += workerShare - assigned
	}
	return out
}

// rewriteJMXThreadCount replaces ThreadGroup.num_threads when present.
func rewriteJMXThreadCount(jmx string, threads int) string {
	if threads <= 0 {
		return jmx
	}
	re := regexp.MustCompile(`(<stringProp name="ThreadGroup\.num_threads">)[^<]*(</stringProp>)`)
	if re.MatchString(jmx) {
		return re.ReplaceAllString(jmx, fmt.Sprintf(`${1}%d${2}`, threads))
	}
	return jmx
}

func mergeJMeterSummaries(a, b map[string]interface{}) map[string]interface{} {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	ra, _ := a["requests"].(int)
	rb, _ := b["requests"].(int)
	// JSON numbers may be float64 from some paths; normalize.
	if ra == 0 {
		ra = int(getFloat64(a, "requests"))
	}
	if rb == 0 {
		rb = int(getFloat64(b, "requests"))
	}
	ea := getFloat64(a, "error_rate")
	eb := getFloat64(b, "error_rate")
	total := ra + rb
	errRate := 0.0
	if total > 0 {
		errRate = (ea*float64(ra) + eb*float64(rb)) / float64(total)
	}
	// Conservative latency merge: take max of percentiles (worst worker).
	maxP := func(key string) float64 {
		va, vb := getFloat64(a, key), getFloat64(b, key)
		if vb > va {
			return vb
		}
		return va
	}
	out := map[string]interface{}{
		"requests": total, "error_rate": errRate,
		"p50_ms": maxP("p50_ms"), "p95_ms": maxP("p95_ms"), "p99_ms": maxP("p99_ms"),
		"engine": "jmeter",
	}
	if t, ok := a["truncated"]; ok {
		out["truncated"] = t
	}
	if t, ok := b["truncated"]; ok {
		out["truncated"] = t
	}
	return out
}

func parseJTLFile(path string) (map[string]interface{}, []map[string]interface{}) {
	summary := map[string]interface{}{"requests": 0, "error_rate": 0.0, "p50_ms": 0.0, "p95_ms": 0.0, "p99_ms": 0.0, "engine": "jmeter"}
	samples := []map[string]interface{}{}
	f, err := os.Open(path)
	if err != nil {
		summary["error"] = "jtl missing: " + err.Error()
		return summary, samples
	}
	defer f.Close()
	r := csv.NewReader(bufio.NewReader(f))
	r.ReuseRecord = true
	r.FieldsPerRecord = -1
	idx := map[string]int{"elapsed": 1, "label": 2, "responseCode": 3, "success": 7, "timeStamp": 0}
	headerDone := false
	var lats []float64
	errors := 0
	n := 0
	const maxRows = 100000
	const maxLats = 50000
	org := defaultOrgID
	proj := defaultProjectID
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if !headerDone {
			headerDone = true
			if len(row) > 0 && (strings.Contains(strings.ToLower(row[0]), "time") || row[0] == "timeStamp") {
				idx = map[string]int{}
				for i, h := range row {
					idx[h] = i
				}
				continue
			}
		}
		n++
		if n > maxRows {
			summary["truncated"] = true
			break
		}
		get := func(k string, def int) string {
			if j, ok := idx[k]; ok && j < len(row) {
				return row[j]
			}
			if def < len(row) {
				return row[def]
			}
			return ""
		}
		elat, _ := strconv.ParseFloat(get("elapsed", 1), 64)
		code, _ := strconv.Atoi(get("responseCode", 3))
		ok := strings.EqualFold(get("success", 7), "true") || get("success", 7) == "1"
		if !ok {
			errors++
		}
		if len(lats) < maxLats {
			lats = append(lats, elat)
		}
		label := get("label", 2)
		ts := time.Now().UTC()
		if rawTS := get("timeStamp", 0); rawTS != "" {
			if ms, err := strconv.ParseInt(rawTS, 10, 64); err == nil && ms > 0 {
				if ms > 1e12 { // epoch millis
					ts = time.UnixMilli(ms).UTC()
				} else if ms > 1e9 { // epoch seconds
					ts = time.Unix(ms, 0).UTC()
				}
			}
		}
		if len(samples) < 500 {
			samples = append(samples, map[string]interface{}{
				"run_id": "", "organization_id": org, "project_id": proj,
				"ts":         ts.Format("2006-01-02 15:04:05.000"),
				"latency_ms": elat, "status_code": code, "ok": jmeterBoolToU8(ok),
				"url": label, "step_name": label,
			})
		}
	}
	sortFloats(lats)
	summary["requests"] = n
	if n > 0 {
		summary["error_rate"] = float64(errors) / float64(n)
		summary["p50_ms"] = percentileFloat(lats, 50)
		summary["p95_ms"] = percentileFloat(lats, 95)
		summary["p99_ms"] = percentileFloat(lats, 99)
	}
	return summary, samples
}

func jmeterBoolToU8(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

func sortFloats(a []float64) {
	// simple insertion for typical JTL sizes in smoke
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

func percentileFloat(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int((p/100)*float64(len(sorted))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func stampRunIDOnSamples(runID, org, proj string, samples []map[string]interface{}) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	for _, s := range samples {
		s["run_id"] = runID
		s["organization_id"] = org
		s["project_id"] = proj
		if s["ts"] == "" {
			s["ts"] = now
		}
	}
}
