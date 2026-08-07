package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PerfContainerRunner launches ephemeral load-engine containers (JMeter today).
// DockerRunner is the production default. Future Kubernetes / other container APIs
// should implement this interface — do not fake a K8s path here.
//
//	type FutureK8sRunner struct{} // extension point: Job/Pod per worker, same RunSpec contract
type PerfContainerRunner interface {
	// Name identifies the runner implementation (e.g. "docker").
	Name() string
	// Available reports whether this runner can execute (CLI/socket present).
	Available() bool
	// RunJMeter starts one ephemeral JMeter container for the given work dir.
	// workDirAgent is the agent-local path containing plan.jmx (and optional data.csv).
	// workDirRel is the subdirectory name under the shared volume root (usually runID or runID-wN).
	// Returns container name/id and a wait function that blocks until exit.
	RunJMeter(spec PerfJMeterRunSpec) (PerfContainerHandle, error)
}

// PerfJMeterRunSpec describes one JMeter worker container.
type PerfJMeterRunSpec struct {
	RunID       string
	WorkerIndex int
	Workers     int
	VUs         int
	WorkDir     string // absolute path inside the Agent filesystem
	WorkRel     string // relative key under shared mount (e.g. runID or runID-w0)
	Image       string
	Network     string
	CPUs        string
	Memory      string
	ExtraArgs   []string
}

// PerfContainerHandle is a started container.
type PerfContainerHandle struct {
	ID   string
	Name string
	Wait func() error
}

// defaultPerfContainerRunner is the process-wide runner (Docker by default).
var defaultPerfContainerRunner PerfContainerRunner = DockerRunner{}

// In-memory registry of dispatched containers for live status / cancel stop.
type runRunnerState struct {
	Containers []string
	Mode       string
	Image      string
	Workers    int
	StartedAt  time.Time
}

var (
	runRunnersMu sync.RWMutex
	runRunners   = map[string]*runRunnerState{}
)

func registerRunContainers(runID string, names []string, mode, image string, workers int) {
	if runID == "" || len(names) == 0 {
		return
	}
	cp := append([]string(nil), names...)
	runRunnersMu.Lock()
	runRunners[runID] = &runRunnerState{
		Containers: cp, Mode: mode, Image: image, Workers: workers, StartedAt: time.Now().UTC(),
	}
	runRunnersMu.Unlock()
}

func lookupRunContainers(runID string) *runRunnerState {
	runRunnersMu.RLock()
	defer runRunnersMu.RUnlock()
	st := runRunners[runID]
	if st == nil {
		return nil
	}
	cp := *st
	cp.Containers = append([]string(nil), st.Containers...)
	return &cp
}

func clearRunContainers(runID string) {
	runRunnersMu.Lock()
	delete(runRunners, runID)
	runRunnersMu.Unlock()
}

// dockerContainerSnapshot inspects one container by name (best-effort).
func dockerContainerSnapshot(name string) map[string]interface{} {
	out := map[string]interface{}{
		"name": name, "engine": "docker", "found": false, "status": "unknown",
	}
	if strings.TrimSpace(name) == "" {
		out["error"] = "empty name"
		return out
	}
	if _, err := exec.LookPath("docker"); err != nil {
		out["error"] = "docker CLI not found"
		out["honesty"] = "Live container inspect needs docker on the opl-api host (compose mounts docker.sock)."
		return out
	}
	cmd := exec.Command("docker", "inspect", "--format",
		"{{.Id}}|{{.State.Status}}|{{.State.Running}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.ExitCode}}|{{.Config.Image}}",
		name)
	b, err := cmd.CombinedOutput()
	if err != nil {
		out["status"] = "not_found"
		out["error"] = strings.TrimSpace(string(b))
		if out["error"] == "" {
			out["error"] = err.Error()
		}
		out["honesty"] = "Container gone (docker run --rm) or never started — check run status/samples."
		return out
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) < 7 {
		out["error"] = "unexpected inspect format"
		out["raw"] = strings.TrimSpace(string(b))
		return out
	}
	running := strings.EqualFold(parts[2], "true")
	out["found"] = true
	out["id"] = truncateStr(parts[0], 12)
	out["status"] = parts[1]
	out["running"] = running
	out["started_at"] = parts[3]
	out["finished_at"] = parts[4]
	out["exit_code"] = atoiDefault(parts[5], 0)
	out["image"] = parts[6]
	return out
}

// parseDockerInspectLine is a pure helper for tests (same format as docker inspect --format above).
func parseDockerInspectLine(name, line string) map[string]interface{} {
	out := map[string]interface{}{
		"name": name, "engine": "docker", "found": false, "status": "unknown",
	}
	parts := strings.Split(strings.TrimSpace(line), "|")
	if len(parts) < 7 {
		out["error"] = "unexpected inspect format"
		out["raw"] = strings.TrimSpace(line)
		return out
	}
	running := strings.EqualFold(parts[2], "true")
	out["found"] = true
	out["id"] = truncateStr(parts[0], 12)
	out["status"] = parts[1]
	out["running"] = running
	out["started_at"] = parts[3]
	out["finished_at"] = parts[4]
	out["exit_code"] = atoiDefault(parts[5], 0)
	out["image"] = parts[6]
	return out
}

// stopRunContainers best-effort docker stop for cancel.
func stopRunContainers(runID string) []map[string]interface{} {
	st := lookupRunContainers(runID)
	if st == nil || len(st.Containers) == 0 {
		return nil
	}
	results := make([]map[string]interface{}, 0, len(st.Containers))
	for _, name := range st.Containers {
		entry := map[string]interface{}{"name": name, "stopped": false}
		if _, err := exec.LookPath("docker"); err != nil {
			entry["error"] = "docker CLI not found"
			results = append(results, entry)
			continue
		}
		cmd := exec.Command("docker", "stop", "--time", "5", name)
		b, err := cmd.CombinedOutput()
		if err != nil {
			entry["error"] = strings.TrimSpace(string(b))
			if entry["error"] == "" {
				entry["error"] = err.Error()
			}
		} else {
			entry["stopped"] = true
			entry["output"] = strings.TrimSpace(string(b))
		}
		results = append(results, entry)
	}
	clearRunContainers(runID)
	return results
}

// containerNamesFromAny normalizes dispatchInfo["containers"] (typed or JSON-decoded).
func containerNamesFromAny(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// DockerRunner launches JMeter via the docker CLI (compose stack mounts docker.sock).
type DockerRunner struct{}

func (DockerRunner) Name() string { return "docker" }

func (DockerRunner) Available() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	// Soft probe: docker info may be slow; LookPath is enough for "available".
	// Actual failures surface at RunJMeter.
	return true
}

func (d DockerRunner) RunJMeter(spec PerfJMeterRunSpec) (PerfContainerHandle, error) {
	if !d.Available() {
		return PerfContainerHandle{}, fmt.Errorf("docker CLI not found")
	}
	image := strings.TrimSpace(spec.Image)
	if image == "" {
		image = envOr("OPA_JMETER_IMAGE", "justb4/jmeter:5.5")
	}
	name := fmt.Sprintf("opa-jmeter-%s-w%d", sanitizeDockerName(spec.RunID), spec.WorkerIndex)
	mount := dockerJMeterMountArg(spec.WorkDir, spec.WorkRel)
	jmxIn := "/jmeter/plan.jmx"
	jtlIn := "/jmeter/results.jtl"
	logIn := "/jmeter/jmeter.log"
	if mount.usesSharedRoot {
		jmxIn = "/jmeter/" + spec.WorkRel + "/plan.jmx"
		jtlIn = "/jmeter/" + spec.WorkRel + "/results.jtl"
		logIn = "/jmeter/" + spec.WorkRel + "/jmeter.log"
	}

	args := []string{"run", "--rm", "--name", name,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=256m",
	}
	if net := strings.TrimSpace(firstNonEmpty(spec.Network, envOr("OPA_JMETER_NETWORK", ""))); net != "" {
		args = append(args, "--network", net)
	} else {
		args = append(args, "--network", "none")
	}
	if cpus := strings.TrimSpace(firstNonEmpty(spec.CPUs, envOr("OPA_JMETER_CPUS", ""))); cpus != "" {
		args = append(args, "--cpus", cpus)
	}
	if mem := strings.TrimSpace(firstNonEmpty(spec.Memory, envOr("OPA_JMETER_MEMORY", ""))); mem != "" {
		args = append(args, "--memory", mem)
	}
	args = append(args,
		"-v", mount.arg,
		"-e", "JVM_ARGS=-Djava.awt.headless=true",
		image,
		"-n", "-t", jmxIn, "-l", jtlIn, "-j", logIn,
		"-JLOAD_RUN_ID="+spec.RunID,
		fmt.Sprintf("-JOPA_THREADS=%d", spec.VUs),
		fmt.Sprintf("-JOPA_WORKER=%d", spec.WorkerIndex),
		fmt.Sprintf("-JOPA_WORKERS=%d", maxInt(1, spec.Workers)),
	)
	args = append(args, spec.ExtraArgs...)

	cmd := exec.Command("docker", args...)
	cmd.Dir = spec.WorkDir
	if err := cmd.Start(); err != nil {
		return PerfContainerHandle{}, err
	}
	return PerfContainerHandle{
		ID:   fmt.Sprintf("%d", cmd.Process.Pid),
		Name: name,
		Wait: func() error {
			_, err := cmd.Process.Wait()
			return err
		},
	}, nil
}

type dockerMount struct {
	arg            string
	usesSharedRoot bool
}

// dockerJMeterMountArg builds the -v argument.
// Prefer named volume (OPA_JMETER_VOLUME) or host bind (OPA_JMETER_HOST_WORK) so
// Agent-in-container can share files with sibling JMeter containers.
func dockerJMeterMountArg(workDir, workRel string) dockerMount {
	if vol := strings.TrimSpace(envOr("OPA_JMETER_VOLUME", "")); vol != "" {
		return dockerMount{arg: vol + ":/jmeter", usesSharedRoot: true}
	}
	if host := strings.TrimSpace(envOr("OPA_JMETER_HOST_WORK", "")); host != "" {
		return dockerMount{arg: host + ":/jmeter", usesSharedRoot: true}
	}
	// Agent on host (or bind-compatible path): mount the specific work dir.
	_ = workRel
	return dockerMount{arg: workDir + ":/jmeter", usesSharedRoot: false}
}

func sanitizeDockerName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		out = "run"
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func envFlagOn(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// perfJMeterWorkers returns how many containers to spawn for VU scale.
func perfJMeterWorkers(requested int) int {
	n := requested
	if n <= 0 {
		n, _ = strconv.Atoi(strings.TrimSpace(envOr("OPA_JMETER_WORKERS", "1")))
	}
	if n <= 0 {
		n = 1
	}
	maxW, _ := strconv.Atoi(strings.TrimSpace(envOr("OPA_JMETER_MAX_WORKERS", "16")))
	if maxW <= 0 {
		maxW = 16
	}
	if n > maxW {
		n = maxW
	}
	return n
}

// perfJMeterWorkRoot is the Agent-local directory for JMX/JTL artifacts.
func perfJMeterWorkRoot() string {
	return envOr("OPA_JMETER_WORK", filepath.Join(os.TempDir(), "opa-jmeter"))
}

// hostJMeterAllowed is the explicit dev escape hatch for OPA_JMETER_BIN / PATH jmeter.
func hostJMeterAllowed() bool { return envFlagOn("OPA_PERF_ALLOW_HOST_JMETER") }

// nodePerfFallbackAllowed is the explicit dev escape hatch for load-runner.mjs.
func nodePerfFallbackAllowed() bool { return envFlagOn("OPA_PERF_ALLOW_NODE_FALLBACK") }

// resolveJMeterEngine picks docker (default) or host bin (dev-only).
func resolveJMeterEngine() (mode string, ok bool) {
	runner := defaultPerfContainerRunner
	if runner != nil && runner.Available() {
		return runner.Name(), true
	}
	if hostJMeterAllowed() {
		if bin := strings.TrimSpace(envOr("OPA_JMETER_BIN", "")); bin != "" {
			if _, err := os.Stat(bin); err == nil {
				return "bin", true
			}
			if p, err := exec.LookPath(bin); err == nil && p != "" {
				return "bin", true
			}
		}
		if p, err := exec.LookPath("jmeter"); err == nil && p != "" {
			return "path", true
		}
	}
	return "", false
}

// waitWithTimeout waits on a handle, ignoring hang past timeout (container --rm still reapable).
func waitWithTimeout(h PerfContainerHandle, timeout time.Duration) error {
	if h.Wait == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- h.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("jmeter container wait timeout after %s", timeout)
	}
}
