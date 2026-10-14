package docker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/manifest"
)

func TestParamsDefaults(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`  `), json.RawMessage(`{}`)} {
		p, err := ParseParams(raw)
		if err != nil || p.Limits.Memory != "4g" || p.Limits.CPUs != 2 || p.Limits.Timeout != "1h0m0s" || p.Depth != 0 {
			t.Fatalf("%q: %v %+v", raw, err, p)
		}
	}
	p, err := ParseParams(json.RawMessage(`{"depth":1,"limits":{"memory":"8g","cpus":4,"timeout":"2h"}}`))
	if err != nil || p.Depth != 1 || p.Limits.Memory != "8g" || p.Limits.CPUs != 4 || p.Limits.Timeout != "2h" {
		t.Fatalf("%v %+v", err, p)
	}
	if got := string(p.cloneParams()); got != `{"depth":1}` {
		t.Fatalf("the clone only gets its own settings: %s", got)
	}
}

func TestParamsNoFile(t *testing.T) {
	// not set by default: the containers keep the engine's own limit
	p, err := ParseParams(nil)
	if err != nil || p.Limits.NoFile != 0 {
		t.Fatalf("%v %+v", err, p)
	}
	p, err = ParseParams(json.RawMessage(`{"limits":{"nofile":65536}}`))
	if err != nil || p.Limits.NoFile != 65536 {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestParamsRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown key":      `{"memry":"1g"}`,
		"unknown in limit": `{"limits":{"disk":"1g"}}`,
		"submodules":       `{"submodules":true}`,
		"bad memory":       `{"limits":{"memory":"lots"}}`,
		"zero memory":      `{"limits":{"memory":"0m"}}`,
		"no unit":          `{"limits":{"memory":"512"}}`,
		"negative cpus":    `{"limits":{"cpus":-1}}`,
		"negative nofile":  `{"limits":{"nofile":-1}}`,
		"too many files":   `{"limits":{"nofile":2000000}}`,
		"too many cpus":    `{"limits":{"cpus":100}}`,
		"bad timeout":      `{"limits":{"timeout":"soon"}}`,
		"tiny timeout":     `{"limits":{"timeout":"1ms"}}`,
		"not an object":    `[1]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseParams(json.RawMessage(raw)); err == nil || !strings.Contains(err.Error(), "docker runner's params") {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestMemoryBytes(t *testing.T) {
	for in, want := range map[string]int64{"64k": 64 << 10, "512m": 512 << 20, "2g": 2 << 30} {
		if got, err := memoryBytes(in); err != nil || got != want {
			t.Errorf("%s = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "g", "12", "1t", "-1g", "x1g"} {
		if _, err := memoryBytes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func limitsOf(t *testing.T, yaml string) manifest.Limits {
	t.Helper()
	m, err := manifest.Parse([]byte("version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n" + yaml + "  steps: [{run: x}]\nartifact: {paths: [a]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m.Build.Limits
}

func TestResolveAppliesTheServersLimits(t *testing.T) {
	p, _ := ParseParams(json.RawMessage(`{"limits":{"memory":"4g","cpus":2,"timeout":"1h"}}`))
	// nothing asked: the server's defaults, with the manifest's own default timeout (30m)
	e, err := p.resolve(limitsOf(t, ""))
	if err != nil || e.memory != "4g" || e.cpus != 2 || e.timeout != 30*time.Minute {
		t.Fatalf("%v %+v", err, e)
	}
	// less than the maximum is honoured
	e, err = p.resolve(limitsOf(t, "  limits: {memory: 512m, cpus: 0.5, timeout: 5m}\n"))
	if err != nil || e.memory != "512m" || e.cpus != 0.5 || e.timeout != 5*time.Minute {
		t.Fatalf("%v %+v", err, e)
	}
	// exactly the maximum is fine; beyond it is refused, naming both
	if _, err := p.resolve(limitsOf(t, "  limits: {memory: 4g, cpus: 2, timeout: 1h}\n")); err != nil {
		t.Fatalf("the maximum itself: %v", err)
	}
	for yaml, want := range map[string]string{
		"  limits: {memory: 8g}\n":  "build.limits.memory 8g is more than this server allows (4g)",
		"  limits: {cpus: 8}\n":     "build.limits.cpus 8 is more than this server allows (2)",
		"  limits: {timeout: 3h}\n": "build.limits.timeout 3h0m0s is more than this server allows (1h0m0s)",
	} {
		if _, err := p.resolve(limitsOf(t, yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", yaml, want, err)
		}
	}
}

func TestResolveNoFile(t *testing.T) {
	// the server says nothing: a build gets the engine's limit unless it asks for one, whatever it asks
	p, _ := ParseParams(nil)
	e, err := p.resolve(limitsOf(t, ""))
	if err != nil || e.nofile != 0 {
		t.Fatalf("%v %+v", err, e)
	}
	e, err = p.resolve(limitsOf(t, "  limits: {nofile: 65536}\n"))
	if err != nil || e.nofile != 65536 {
		t.Fatalf("%v %+v", err, e)
	}
	// the server sets it: that is what a build gets by default and the most it may ask for
	p, _ = ParseParams(json.RawMessage(`{"limits":{"nofile":65536}}`))
	e, err = p.resolve(limitsOf(t, ""))
	if err != nil || e.nofile != 65536 {
		t.Fatalf("%v %+v", err, e)
	}
	e, err = p.resolve(limitsOf(t, "  limits: {nofile: 4096}\n"))
	if err != nil || e.nofile != 4096 {
		t.Fatalf("%v %+v", err, e)
	}
	if _, err := p.resolve(limitsOf(t, "  limits: {nofile: 65536}\n")); err != nil {
		t.Fatalf("the maximum itself: %v", err)
	}
	if _, err := p.resolve(limitsOf(t, "  limits: {nofile: 100000}\n")); err == nil ||
		!strings.Contains(err.Error(), "build.limits.nofile 100000 is more than this server allows (65536)") {
		t.Fatalf("%v", err)
	}
}
