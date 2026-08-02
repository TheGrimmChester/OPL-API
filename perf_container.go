package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

	args := []string{"run", "--rm", "--name", name}
	if net := strings.TrimSpace(firstNonEmpty(spec.Network, envOr("OPA_JMETER_NETWORK", ""))); net != "" {
		args = append(args, "--network", net)
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
