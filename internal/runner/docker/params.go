package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/dymmer-code/dymploy/internal/manifest"
)

// Params is the runner's configuration (runner.params in the daemon's configuration).
type Params struct {
	// Depth and Submodules are for the clone that precedes the build. Submodules are refused: a build
	// could alter the git directories of its submodules and have the daemon run what it planted.
	Depth      int  `json:"depth"`
	Submodules bool `json:"submodules"`
	// Limits are the most a build may use and what it gets when its definition says nothing. A
	// definition that asks for more is refused. Unset values are 4g, 2 cpus and 1h.
	Limits struct {
		Memory  string  `json:"memory"`
		CPUs    float64 `json:"cpus"`
		Timeout string  `json:"timeout"`
	} `json:"limits"`
}

// Defaults of the limits.
const (
	DefaultMemory  = "4g"
	DefaultCPUs    = 2.0
	DefaultTimeout = time.Hour
)

// ParseParams reads and checks the runner's parameters; nil or empty means all defaults. Unknown keys
// are an error. Call it at start-up so that a typo is found then, not at the first job.
func ParseParams(raw json.RawMessage) (Params, error) {
	var p Params
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return Params{}, fmt.Errorf("the docker runner's params are invalid: %w", err)
		}
	}
	if p.Submodules {
		return Params{}, fmt.Errorf("the docker runner's params are invalid: submodules are not supported (a build could alter the git directories of its submodules)")
	}
	if p.Limits.Memory == "" {
		p.Limits.Memory = DefaultMemory
	}
	if _, err := memoryBytes(p.Limits.Memory); err != nil {
		return Params{}, fmt.Errorf("the docker runner's params are invalid: limits.memory: %w", err)
	}
	switch {
	case p.Limits.CPUs == 0:
		p.Limits.CPUs = DefaultCPUs
	case p.Limits.CPUs < 0 || p.Limits.CPUs > 64:
		return Params{}, fmt.Errorf("the docker runner's params are invalid: limits.cpus must be greater than 0 and at most 64")
	}
	if p.Limits.Timeout == "" {
		p.Limits.Timeout = DefaultTimeout.String()
	}
	if d, err := time.ParseDuration(p.Limits.Timeout); err != nil || d < time.Second {
		return Params{}, fmt.Errorf("the docker runner's params are invalid: limits.timeout %q is not a duration of at least 1s", p.Limits.Timeout)
	}
	return p, nil
}

// cloneParams are the parameters the clone step understands.
func (p Params) cloneParams() json.RawMessage {
	raw, _ := json.Marshal(struct {
		Depth int `json:"depth"`
	}{p.Depth})
	return raw
}

// memoryBytes converts a size written like the manifest does (512m, 2g, 64k).
func memoryBytes(s string) (int64, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("%q is not a size (use a number and k, m or g)", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a size (use a number and k, m or g)", s)
	}
	switch s[len(s)-1] {
	case 'k':
		return n << 10, nil
	case 'm':
		return n << 20, nil
	case 'g':
		return n << 30, nil
	}
	return 0, fmt.Errorf("%q is not a size (use a number and k, m or g)", s)
}

// effective is what one build is allowed to use.
type effective struct {
	memory  string
	cpus    float64
	timeout time.Duration
}

// resolve applies the server's limits to a definition: what it does not ask for is the server's
// default, and what it asks for beyond the server's maximum is an error that names both.
func (p Params) resolve(l manifest.Limits) (effective, error) {
	e := effective{memory: p.Limits.Memory, cpus: p.Limits.CPUs}
	e.timeout, _ = time.ParseDuration(p.Limits.Timeout)
	max, _ := memoryBytes(p.Limits.Memory)
	if l.Memory != "" {
		want, _ := memoryBytes(l.Memory) // already validated by the manifest
		if want > max {
			return effective{}, fmt.Errorf("build.limits.memory %s is more than this server allows (%s)", l.Memory, p.Limits.Memory)
		}
		e.memory = l.Memory
	}
	if l.CPUs != nil {
		if *l.CPUs > p.Limits.CPUs {
			return effective{}, fmt.Errorf("build.limits.cpus %g is more than this server allows (%g)", *l.CPUs, p.Limits.CPUs)
		}
		e.cpus = *l.CPUs
	}
	if t := l.Timeout.D(); t > e.timeout {
		return effective{}, fmt.Errorf("build.limits.timeout %s is more than this server allows (%s)", t, e.timeout)
	} else {
		e.timeout = t
	}
	return e, nil
}
