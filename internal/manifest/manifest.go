// Package manifest reads and validates dymploy.yaml, the file in the root of a repository that tells
// dymploy how to build it and where to deploy the result.
//
// The file is untrusted input: it comes from a repository. The parser therefore refuses anything it
// does not know (unknown keys, anchors and aliases), bounds every size, and never accepts machines,
// users or credentials: a deployment names a TARGET, and what that name means (host, user, key, who may
// use it) is decided by the daemon's own configuration.
package manifest

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MaxSize bounds the file.
const MaxSize = 64 << 10

// Manifest is a parsed and validated dymploy.yaml.
type Manifest struct {
	Version  int      `yaml:"version"`
	Build    Build    `yaml:"build"`
	Artifact Artifact `yaml:"artifact"`
	Deploy   *Deploy  `yaml:"deploy"`
}

// Build says how the artifact is produced, inside containers.
type Build struct {
	Image   Image             `yaml:"image"`
	Env     map[string]string `yaml:"env"`
	Steps   []Step            `yaml:"steps"`
	Cache   []Cache           `yaml:"cache"`
	Limits  Limits            `yaml:"limits"`
	Network string            `yaml:"network"` // "default" or "none"
}

// Image is the build environment: an image to pull, or a Dockerfile in the repository to build.
type Image struct {
	From       string            `yaml:"from"`
	Dockerfile string            `yaml:"dockerfile"`
	Context    string            `yaml:"context"`
	Args       map[string]string `yaml:"args"`
}

// Step is one command of the build, run with `sh -ec` in a container with the repository in /work.
type Step struct {
	Name    string            `yaml:"name"`
	Run     string            `yaml:"run"`
	Image   string            `yaml:"image"` // overrides build.image for this step
	Workdir string            `yaml:"workdir"`
	Env     map[string]string `yaml:"env"`
}

// Cache is a directory inside the build containers kept between builds of the same repository.
type Cache struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Limits bound a build.
type Limits struct {
	CPUs    *float64 `yaml:"cpus"`
	Memory  string   `yaml:"memory"`
	Timeout Duration `yaml:"timeout"`
	// NoFile is the limit of open files of the build's containers (RLIMIT_NOFILE). Unset, they keep the one of
	// the container engine, which is very high on some hosts and makes some runtimes reserve memory from it.
	NoFile *int `yaml:"nofile"`
}

// Artifact says which files of the build result are packed into the transportable tar.gz.
type Artifact struct {
	Name    string   `yaml:"name"`
	Paths   []string `yaml:"paths"`
	Exclude []string `yaml:"exclude"`
}

// Deploy delivers the artifact to a target and runs the commands that install it.
type Deploy struct {
	Target  string   `yaml:"target"`
	Timeout Duration `yaml:"timeout"`
	// Secrets asks for the target's secrets: the server fetches them from the source configured for the
	// target and puts them on it for the steps (path in $DYMPLOY_SECRETS_FILE). Which source is the
	// server's decision; the definition cannot name one.
	Secrets bool `yaml:"secrets"`
	// Path is where, inside the bucket of an s3 target, the files of the artifact are put (a prefix, no
	// leading slash). It is only for s3 targets; steps, check, after and secrets are only for ssh ones.
	Path      string       `yaml:"path"`
	Steps     []DeployStep `yaml:"steps"`
	Check     *Check       `yaml:"check"`
	After     *StepList    `yaml:"after"`
	StepsUndo []DeployStep `yaml:"steps_undo"`
	Finally   *StepList    `yaml:"finally"`
	// SecretsSteps are what `dymploy secrets` runs on the target to put new secrets in place without building
	// or delivering anything (the file is in $DYMPLOY_SECRETS_FILE, as in steps): typically install it and
	// restart the service. The check runs afterwards. They need `secrets: true`. SecretsUndo runs, in order,
	// when one of them or the check fails, so a definition can put back what it replaced.
	SecretsSteps []DeployStep `yaml:"secrets_steps"`
	SecretsUndo  []DeployStep `yaml:"secrets_undo"`
}

// DeployStep is a command run on the target.
type DeployStep struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
}

// StepList is a list of commands run on the target.
type StepList struct {
	Steps []DeployStep `yaml:"steps"`
}

// Check is the health check run after the deploy steps.
type Check struct {
	Run      string   `yaml:"run"`
	Attempts int      `yaml:"attempts"`
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
}

//go:embed dymploy.schema.json
var schema []byte

// Schema is the JSON Schema of dymploy.yaml, for editors and tools. A test keeps it identical in
// structure to what the parser accepts.
func Schema() []byte { return schema }

// HasArtifact reports whether the build produces an artifact. A job that only checks the code (tests,
// linters) has none; a job that deploys always does.
func (m *Manifest) HasArtifact() bool {
	return m.Artifact.Name != "" || len(m.Artifact.Paths) > 0 || len(m.Artifact.Exclude) > 0
}

// HasDeploy reports whether the manifest delivers the artifact anywhere.
func (m *Manifest) HasDeploy() bool { return m.Deploy != nil }

// Summary is a one-line description for humans.
func (m *Manifest) Summary() string {
	steps := fmt.Sprintf("%d steps", len(m.Build.Steps))
	if len(m.Build.Steps) == 1 {
		steps = "1 step"
	}
	parts := []string{"build: " + steps}
	if m.HasArtifact() {
		parts = append(parts, "artifact: "+m.Artifact.Name)
	} else {
		parts = append(parts, "no artifact")
	}
	if m.Deploy != nil {
		parts = append(parts, "deploy: "+m.Deploy.Target)
	}
	return strings.Join(parts, ", ")
}

// Errors is the list of problems found in a manifest.
type Errors []string

func (e Errors) Error() string {
	if len(e) == 1 {
		return e[0]
	}
	return fmt.Sprintf("%d problems:\n  - %s", len(e), strings.Join(e, "\n  - "))
}

// ErrNotFound is returned by Find when the repository has no manifest.
var ErrNotFound = errors.New("no dymploy.yaml in the repository")

// Find returns the name of the manifest in the root of fsys: dymploy.yaml or dymploy.yml, not both.
func Find(fsys fs.FS) (string, error) {
	_, errA := fs.Stat(fsys, "dymploy.yaml")
	_, errB := fs.Stat(fsys, "dymploy.yml")
	switch {
	case errA == nil && errB == nil:
		return "", errors.New("the repository has both dymploy.yaml and dymploy.yml: keep only one")
	case errA == nil:
		return "dymploy.yaml", nil
	case errB == nil:
		return "dymploy.yml", nil
	}
	return "", ErrNotFound
}

// CheckFiles verifies that the files the manifest refers to exist in the repository.
func (m *Manifest) CheckFiles(fsys fs.FS) error {
	var errs Errors
	if df := m.Build.Image.Dockerfile; df != "" {
		if _, err := fs.Stat(fsys, df); err != nil {
			errs = append(errs, fmt.Sprintf("build.image.dockerfile: %s does not exist in the repository", df))
		}
	}
	if ctx := m.Build.Image.Context; ctx != "" && ctx != "." {
		if fi, err := fs.Stat(fsys, ctx); err != nil || !fi.IsDir() {
			errs = append(errs, fmt.Sprintf("build.image.context: %s is not a directory of the repository", ctx))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// Parse reads, checks and completes a manifest. All the problems found are reported together.
func Parse(data []byte) (*Manifest, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("the file is too large (%d bytes, the limit is %d)", len(data), MaxSize)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, errors.New(cleanYAMLError(err))
	}
	if root.Kind == 0 {
		return nil, errors.New("the file is empty")
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, errors.New("the file must be a mapping of settings (version, build, artifact, deploy)")
	}
	if err := noAnchors(top); err != nil {
		return nil, err
	}
	if err := checkVersion(top); err != nil {
		return nil, err
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, errors.New(cleanYAMLError(err))
	}
	m.applyDefaults()
	if errs := m.validate(); len(errs) > 0 {
		return nil, errs
	}
	return &m, nil
}

func cleanYAMLError(err error) string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		out := make([]string, len(te.Errors))
		for i, e := range te.Errors {
			out[i] = friendly(e)
		}
		return strings.Join(out, "; ")
	}
	return strings.TrimPrefix(err.Error(), "yaml: ")
}

func noAnchors(n *yaml.Node) error {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return fmt.Errorf("line %d: anchors and aliases are not supported", n.Line)
	}
	for _, c := range n.Content {
		if err := noAnchors(c); err != nil {
			return err
		}
	}
	return nil
}

func checkVersion(top *yaml.Node) error {
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "version" {
			continue
		}
		v := top.Content[i+1]
		if v.Kind != yaml.ScalarNode || v.Tag != "!!int" {
			return fmt.Errorf("line %d: version must be a number (this dymploy understands version 1)", v.Line)
		}
		if v.Value != "1" {
			return fmt.Errorf("line %d: unsupported version %s: this dymploy understands version 1", v.Line, v.Value)
		}
		return nil
	}
	return errors.New("version is required: start the file with `version: 1`")
}

// Duration is a time written with a unit ("30s", "20m"). A bare number is refused: it would be a
// silent guess about the unit. Reading never fails; validation reports the problem with the field's
// name, together with everything else that is wrong.
type Duration struct {
	d   time.Duration
	bad string
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return d.d }

func dur(d time.Duration) Duration { return Duration{d: d} }

func (d Duration) unset() bool { return d.d == 0 && d.bad == "" }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag == "!!int" || n.Tag == "!!float" {
		d.bad = fmt.Sprintf("needs a unit, for example 30s or 20m (got %q)", n.Value)
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		d.bad = fmt.Sprintf("%q is not a valid duration (examples: 30s, 20m, 1h)", n.Value)
		return nil
	}
	d.d = v
	return nil
}

func (m *Manifest) applyDefaults() {
	if m.Build.Network == "" {
		m.Build.Network = "default"
	}
	if m.Build.Limits.Timeout.unset() {
		m.Build.Limits.Timeout = dur(30 * time.Minute)
	}
	if m.Artifact.Name == "" && len(m.Artifact.Paths) > 0 {
		base := path.Base(path.Clean(m.Artifact.Paths[0]))
		if nameRE.MatchString(base) && base != "." && base != ".." {
			m.Artifact.Name = base
		} else {
			m.Artifact.Name = "artifact"
		}
	}
	if d := m.Deploy; d != nil {
		if d.Timeout.unset() {
			d.Timeout = dur(10 * time.Minute)
		}
		if c := d.Check; c != nil {
			if c.Attempts == 0 {
				c.Attempts = 5
			}
			if c.Interval.unset() {
				c.Interval = dur(3 * time.Second)
			}
			if c.Timeout.unset() {
				c.Timeout = dur(10 * time.Second)
			}
		}
	}
}
