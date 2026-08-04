package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
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
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<jmeterTestPlan version="1.2" properties="5.0" jmeter="5.5">` + "\n")
	b.WriteString("  <hashTree>\n")
	b.WriteString(`    <TestPlan guiclass="TestPlanGui" testclass="TestPlan" testname="` + xmlEscape(nz(name, "OPA Plan")) + `" enabled="true"/>` + "\n")
	b.WriteString("    <hashTree>\n")
	// User Defined Variables for load_run_id correlation
	b.WriteString(`      <Arguments guiclass="ArgumentsPanel" testclass="Arguments" testname="OPA Vars" enabled="true">
        <collectionProp name="Arguments.arguments">
          <elementProp name="LOAD_RUN_ID" elementType="Argument">
            <stringProp name="Argument.name">LOAD_RUN_ID</stringProp>
            <stringProp name="Argument.value">${__P(LOAD_RUN_ID,)}</stringProp>
          </elementProp>
        </collectionProp>
      </Arguments>
      <hashTree/>` + "\n")
	b.WriteString(fmt.Sprintf(`      <ThreadGroup guiclass="ThreadGroupGui" testclass="ThreadGroup" testname="VUs" enabled="true">
        <stringProp name="ThreadGroup.num_threads">%d</stringProp>
        <stringProp name="ThreadGroup.ramp_time">10</stringProp>
        <boolProp name="ThreadGroup.scheduler">true</boolProp>
        <stringProp name="ThreadGroup.duration">%d</stringProp>
        <elementProp name="ThreadGroup.main_controller" elementType="LoopController">
          <boolProp name="LoopController.continue_forever">true</boolProp>
          <stringProp name="LoopController.loops">-1</stringProp>
        </elementProp>
      </ThreadGroup>`+"\n", vus, dur))
	b.WriteString("      <hashTree>\n")
	// Header manager for load_run_id → APM
	b.WriteString(`        <HeaderManager guiclass="HeaderPanel" testclass="HeaderManager" testname="OPA Correlation Headers" enabled="true">
          <collectionProp name="HeaderManager.headers">
            <elementProp name="" elementType="Header">
              <stringProp name="Header.name">X-OPA-Load-Run-Id</stringProp>
              <stringProp name="Header.value">${LOAD_RUN_ID}</stringProp>
            </elementProp>
            <elementProp name="" elementType="Header">
              <stringProp name="Header.name">baggage</stringProp>
              <stringProp name="Header.value">load_run_id=${LOAD_RUN_ID}</stringProp>
            </elementProp>
          </collectionProp>
        </HeaderManager>
        <hashTree/>` + "\n")
	frags := indexFragmentsByName(steps)
	for i, step := range steps {
		appendStepJMXIndexed(&b, step, i, "        ", frags)
	}
	b.WriteString("      </hashTree>\n")
	b.WriteString("    </hashTree>\n")
	b.WriteString("  </hashTree>\n")
	b.WriteString("</jmeterTestPlan>\n")
	return b.String()
}

func appendStepJMX(b *strings.Builder, step map[string]interface{}, i int) {
	appendStepJMXIndexed(b, step, i, "        ", nil)
}

func appendStepJMXIndent(b *strings.Builder, step map[string]interface{}, i int, indent string) {
	appendStepJMXIndexed(b, step, i, indent, nil)
}

func appendStepJMXIndexed(b *strings.Builder, step map[string]interface{}, i int, indent string, frags map[string]map[string]interface{}) {
	typ := fmt.Sprint(step["type"])
	if typ == "" || typ == "<nil>" {
		typ = "http"
	}
	name := nz(fmt.Sprint(step["name"]), fmt.Sprintf("step-%d", i+1))
	kids := stepChildren(step)
	emitKids := func(list []map[string]interface{}, childIndent string) {
		for j, child := range list {
			appendStepJMXIndexed(b, child, j, childIndent, frags)
		}
	}
	switch typ {
	case "extract":
		expr := fmt.Sprint(step["expression"])
		vname := fmt.Sprint(step["var"])
		engine := fmt.Sprint(step["engine"])
		if engine == "jsonpath" || strings.HasPrefix(expr, "$.") {
			b.WriteString(fmt.Sprintf(`%s<JSONPostProcessor guiclass="JSONPostProcessorGui" testclass="JSONPostProcessor" testname=%q enabled="true">
%s  <stringProp name="JSONPostProcessor.referenceNames">%s</stringProp>
%s  <stringProp name="JSONPostProcessor.jsonPathExprs">%s</stringProp>
%s  <stringProp name="JSONPostProcessor.match_numbers">1</stringProp>
%s</JSONPostProcessor>
%s<hashTree/>`+"\n", indent, xmlEscape(name), indent, xmlEscape(vname), indent, xmlEscape(expr), indent, indent, indent))
		} else {
			b.WriteString(fmt.Sprintf(`%s<RegexExtractor guiclass="RegexExtractorGui" testclass="RegexExtractor" testname=%q enabled="true">
%s  <stringProp name="RegexExtractor.refname">%s</stringProp>
%s  <stringProp name="RegexExtractor.regex">%s</stringProp>
%s  <stringProp name="RegexExtractor.template">$1$</stringProp>
%s  <stringProp name="RegexExtractor.match_number">1</stringProp>
%s</RegexExtractor>
%s<hashTree/>`+"\n", indent, xmlEscape(name), indent, xmlEscape(vname), indent, xmlEscape(expr), indent, indent, indent, indent))
		}
	case "assert":
		if st, ok := step["status"]; ok {
			b.WriteString(fmt.Sprintf(`%s<ResponseAssertion guiclass="AssertionGui" testclass="ResponseAssertion" testname=%q enabled="true">
%s  <collectionProp name="Asserion.test_strings">
%s    <stringProp name="0">%v</stringProp>
%s  </collectionProp>
%s  <stringProp name="Assertion.test_field">Assertion.response_code</stringProp>
%s  <boolProp name="Assertion.assume_success">false</boolProp>
%s  <intProp name="Assertion.test_type">8</intProp>
%s</ResponseAssertion>
%s<hashTree/>`+"\n", indent, xmlEscape(name), indent, indent, st, indent, indent, indent, indent, indent, indent))
		}
		if contains, ok := step["body_contains"].(string); ok && contains != "" {
			b.WriteString(fmt.Sprintf(`%s<ResponseAssertion guiclass="AssertionGui" testclass="ResponseAssertion" testname=%q enabled="true">
%s  <collectionProp name="Asserion.test_strings">
%s    <stringProp name="0">%s</stringProp>
%s  </collectionProp>
%s  <stringProp name="Assertion.test_field">Assertion.response_data</stringProp>
%s  <intProp name="Assertion.test_type">2</intProp>
%s</ResponseAssertion>
%s<hashTree/>`+"\n", indent, xmlEscape(name+" body"), indent, indent, xmlEscape(contains), indent, indent, indent, indent, indent))
		}
	case "container", "transaction":
		b.WriteString(fmt.Sprintf(`%s<TransactionController guiclass="TransactionControllerGui" testclass="TransactionController" testname=%q enabled="true">
%s  <boolProp name="TransactionController.includeTimers">false</boolProp>
%s</TransactionController>
%s<hashTree>`+"\n", indent, xmlEscape(name), indent, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "if", "if_controller":
		cond := nz(fmt.Sprint(step["condition"]), `${__jexl3(true)}`)
		if cond == "<nil>" {
			cond = `${__jexl3(true)}`
		}
		b.WriteString(fmt.Sprintf(`%s<IfController guiclass="IfControllerPanel" testclass="IfController" testname=%q enabled="true">
%s  <stringProp name="IfController.condition">%s</stringProp>
%s  <boolProp name="IfController.evaluateAll">false</boolProp>
%s  <boolProp name="IfController.useExpression">true</boolProp>
%s</IfController>
%s<hashTree>`+"\n", indent, xmlEscape(name), indent, xmlEscape(cond), indent, indent, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "while", "while_controller":
		cond := nz(fmt.Sprint(step["condition"]), `${__jexl3(false)}`)
		if cond == "<nil>" {
			cond = `${__jexl3(false)}`
		}
		b.WriteString(fmt.Sprintf(`%s<WhileController guiclass="WhileControllerGui" testclass="WhileController" testname=%q enabled="true">
%s  <stringProp name="WhileController.condition">%s</stringProp>
%s</WhileController>
%s<hashTree>`+"\n", indent, xmlEscape(name), indent, xmlEscape(cond), indent, indent))
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
		b.WriteString(fmt.Sprintf(`%s<LoopController guiclass="LoopControlPanel" testclass="LoopController" testname=%q enabled="true">
%s  <boolProp name="LoopController.continue_forever">%t</boolProp>
%s  <stringProp name="LoopController.loops">%d</stringProp>
%s</LoopController>
%s<hashTree>`+"\n", indent, xmlEscape(name), indent, forever, indent, loops, indent, indent))
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
		b.WriteString(fmt.Sprintf(`%s<ForeachController guiclass="ForeachControlPanel" testclass="ForeachController" testname=%q enabled="true">
%s  <stringProp name="ForeachController.inputVal">%s</stringProp>
%s  <stringProp name="ForeachController.returnVal">%s</stringProp>
%s  <boolProp name="ForeachController.useSeparator">%t</boolProp>
%s</ForeachController>
%s<hashTree>`+"\n", indent, xmlEscape(name), indent, xmlEscape(inputVar), indent, xmlEscape(returnVar), indent, useSep, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "fragment":
		// Disabled so definition is preserved for round-trip / include expand, not executed twice.
		fragName := "Fragment:" + name
		b.WriteString(fmt.Sprintf(`%s<GenericController guiclass="LogicControllerGui" testclass="GenericController" testname=%q enabled="false">
%s  <stringProp name="opl.fragment">true</stringProp>
%s</GenericController>
%s<hashTree>`+"\n", indent, xmlEscape(fragName), indent, indent, indent))
		emitKids(kids, indent+"  ")
		b.WriteString(indent + "</hashTree>\n")
	case "include", "link":
		ref := strings.TrimSpace(fmt.Sprint(step["ref"]))
		if ref == "" || ref == "<nil>" {
			ref = strings.TrimSpace(fmt.Sprint(step["fragment"]))
		}
		if ref == "" || ref == "<nil>" {
			ref = name
		}
		expanded := kids
		if len(expanded) == 0 && frags != nil {
			expanded = resolveIncludeSteps(step, frags)
			if len(expanded) == 1 {
				errMsg := strings.TrimSpace(fmt.Sprint(expanded[0]["error"]))
				if errMsg != "" && errMsg != "<nil>" {
					b.WriteString(fmt.Sprintf("%s<!-- opl-include missing fragment ref=%s -->\n", indent, xmlCommentSafe(ref)))
					return
				}
			}
		}
		b.WriteString(fmt.Sprintf("%s<!-- opl-include ref=%s -->\n", indent, xmlCommentSafe(ref)))
		emitKids(expanded, indent)
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
		bodyXML := ""
		if body != "" {
			bodyXML = fmt.Sprintf(`
%s  <boolProp name="HTTPSampler.postBodyRaw">true</boolProp>
%s  <elementProp name="HTTPsampler.Arguments" elementType="Arguments">
%s    <collectionProp name="Arguments.arguments">
%s      <elementProp name="" elementType="HTTPArgument">
%s        <boolProp name="HTTPArgument.always_encode">false</boolProp>
%s        <stringProp name="Argument.value">%s</stringProp>
%s        <stringProp name="Argument.metadata">=</stringProp>
%s      </elementProp>
%s    </collectionProp>
%s  </elementProp>`, indent, indent, indent, indent, indent, indent, xmlEscape(body), indent, indent, indent, indent)
		}
		b.WriteString(fmt.Sprintf(`%s<HTTPSamplerProxy guiclass="HttpTestSampleGui" testclass="HTTPSamplerProxy" testname=%q enabled="true">
%s  <stringProp name="HTTPSampler.domain">%s</stringProp>
%s  <stringProp name="HTTPSampler.port">%s</stringProp>
%s  <stringProp name="HTTPSampler.protocol">%s</stringProp>
%s  <stringProp name="HTTPSampler.path">%s</stringProp>
%s  <stringProp name="HTTPSampler.method">%s</stringProp>
%s  <boolProp name="HTTPSampler.follow_redirects">true</boolProp>%s
%s</HTTPSamplerProxy>
%s<hashTree>`+"\n", indent, xmlEscape(name), indent, xmlEscape(domain), indent, xmlEscape(port), indent, xmlEscape(proto), indent, xmlEscape(path), indent, xmlEscape(method), indent, bodyXML, indent, indent))
		emitKids(kids, indent+"  ")
		think := 0
		if t, ok := asFloat(step["think_ms"]); ok {
			think = int(t)
		}
		if t, ok := asFloat(step["think_ms_rand"]); ok && int(t) > think {
			think = int(t)
		}
		if think > 0 {
			b.WriteString(fmt.Sprintf(`%s  <ConstantTimer guiclass="ConstantTimerGui" testclass="ConstantTimer" testname="Think" enabled="true">
%s    <stringProp name="ConstantTimer.delay">%d</stringProp>
%s  </ConstantTimer>
%s  <hashTree/>`+"\n", indent, indent, think, indent, indent))
		}
		b.WriteString(indent + "</hashTree>\n")
	}
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
	vus = clampPerfVUs(vus)
	nWorkers := perfJMeterWorkers(workers)
	if nWorkers > vus {
		nWorkers = vus
	}
	dur := clampPerfDuration(int(getFloat64(sc, "duration_seconds")))
	if strings.TrimSpace(jmx) == "" {
		steps := json.RawMessage(stepsRaw)
		if len(steps) == 0 {
			steps = json.RawMessage(`[]`)
		}
		jmx = generateJMXFromUpsert(getString(sc, "name"), getString(sc, "target_url"), getString(sc, "method"), getString(sc, "body"),
			vus, dur, steps)
	}
	if jmxContainsUnsafeElements(jmx) {
		return map[string]interface{}{"dispatched": false, "error": "jmx contains unsafe elements"}
	}

	root := perfJMeterWorkRoot()
	workerDirs := make([]string, 0, nWorkers)
	workerVUs := splitVUsAcrossWorkers(vus, nWorkers)
	csvInline := ""
	if ds := getString(sc, "datasets_json"); ds != "" {
		var m map[string]interface{}
		if json.Unmarshal([]byte(ds), &m) == nil {
			if csvBlock, ok := m["csv"].(map[string]interface{}); ok {
				if inline, ok := csvBlock["inline"].(string); ok {
					csvInline = inline
				}
			}
		}
	}

	for i := 0; i < nWorkers; i++ {
		rel := runID
		if nWorkers > 1 {
			rel = fmt.Sprintf("%s-w%d", runID, i)
		}
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return map[string]interface{}{"dispatched": false, "error": err.Error()}
		}
		workerJMX := rewriteJMXThreadCount(jmx, workerVUs[i])
		if err := os.WriteFile(filepath.Join(dir, "plan.jmx"), []byte(workerJMX), 0o600); err != nil {
			return map[string]interface{}{"dispatched": false, "error": err.Error()}
		}
		if csvInline != "" {
			_ = os.WriteFile(filepath.Join(dir, "data.csv"), []byte(csvInline), 0o600)
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

	return map[string]interface{}{
		"dispatched": true, "engine": "jmeter", "mode": mode,
		"workers": nWorkers, "worker_vus": workerVUs, "image": image,
		"containers": containerNames, "work_dirs": workerDirs,
		"honesty": "Ephemeral Apache JMeter container(s); JTL merged into load_run_samples. Host bin is OPA_PERF_ALLOW_HOST_JMETER=1 only.",
	}
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
