package manifest

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

var (
	nameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	labelRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/-]{0,63}$`)
	targetRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
	imageRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,200}(:[A-Za-z0-9._-]{1,128})?(@sha256:[0-9a-f]{64})?$|^[a-z0-9][a-z0-9.-]*:[0-9]{1,5}/[a-z0-9._/-]{1,200}(:[A-Za-z0-9._-]{1,128})?$`)
	memoryRE    = regexp.MustCompile(`^[1-9][0-9]{0,5}[kmg]$`)
	absPathRE   = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)
	cacheNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// MaxNoFile is the most open files a definition (or the server) may ask the containers for.
const MaxNoFile = 1 << 20

const (
	maxSteps     = 100
	maxRunLength = 8 << 10
	maxEnv       = 100
	maxEnvValue  = 4 << 10
)

type validator struct{ errs Errors }

func (v *validator) add(p, format string, a ...any) {
	v.errs = append(v.errs, p+": "+fmt.Sprintf(format, a...))
}

func (m *Manifest) validate() Errors {
	v := &validator{}
	v.build(&m.Build)
	switch {
	case m.HasArtifact():
		v.artifact(&m.Artifact)
	case m.Deploy != nil:
		v.add("artifact", "a deploy needs an artifact: say which files of the build to pack (artifact.paths)")
	}
	if m.Deploy != nil {
		v.deploy(m.Deploy)
	}
	return v.errs
}

func (v *validator) build(b *Build) {
	v.image("build.image", b.Image)
	v.env("build.env", b.Env)
	switch b.Network {
	case "default", "none":
	default:
		v.add("build.network", "%q is not valid: use default or none", b.Network)
	}
	if c := b.Limits.CPUs; c != nil && (*c <= 0 || *c > 64) {
		v.add("build.limits.cpus", "must be greater than 0 and at most 64")
	}
	if b.Limits.Memory != "" && !memoryRE.MatchString(b.Limits.Memory) {
		v.add("build.limits.memory", "%q is not valid: use a number and k, m or g (for example 512m or 2g)", b.Limits.Memory)
	}
	if n := b.Limits.NoFile; n != nil && (*n < 1 || *n > MaxNoFile) {
		v.add("build.limits.nofile", "must be between 1 and %d", MaxNoFile)
	}
	v.duration("build.limits.timeout", b.Limits.Timeout, time.Second, 4*time.Hour)
	seen := map[string]bool{}
	for i, c := range b.Cache {
		p := fmt.Sprintf("build.cache[%d]", i)
		if !cacheNameRE.MatchString(c.Name) {
			v.add(p+".name", "%q is not a valid name (lowercase letters, digits, . _ -)", c.Name)
		} else if seen[c.Name] {
			v.add(p+".name", "duplicate cache name %q", c.Name)
		}
		seen[c.Name] = true
		if !absPathRE.MatchString(c.Path) || hasDotDot(c.Path) {
			v.add(p+".path", "%q must be an absolute path inside the container, without ..", c.Path)
		}
	}
	if len(b.Steps) == 0 {
		v.add("build.steps", "at least one step is required")
	}
	if len(b.Steps) > maxSteps {
		v.add("build.steps", "too many steps (%d, the limit is %d)", len(b.Steps), maxSteps)
		return
	}
	names := map[string]bool{}
	for i, s := range b.Steps {
		p := fmt.Sprintf("build.steps[%d]", i)
		v.label(p, s.Name, names)
		v.run(p+".run", s.Run)
		if s.Image != "" && !imageRE.MatchString(s.Image) {
			v.add(p+".image", "%q is not a valid image reference", s.Image)
		}
		if s.Workdir != "" && !safeRelative(s.Workdir) {
			v.add(p+".workdir", "%q must be a relative path inside the repository, without ..", s.Workdir)
		}
		v.env(p+".env", s.Env)
	}
}

func (v *validator) image(p string, im Image) {
	switch {
	case im.From == "" && im.Dockerfile == "":
		v.add(p, "set either from (an image to pull) or dockerfile (a file of the repository to build)")
		return
	case im.From != "" && im.Dockerfile != "":
		v.add(p, "set either from or dockerfile, not both")
		return
	}
	if im.From != "" && !imageRE.MatchString(im.From) {
		v.add(p+".from", "%q is not a valid image reference (for example debian:13)", im.From)
	}
	if im.Dockerfile != "" && !safeRelative(im.Dockerfile) {
		v.add(p+".dockerfile", "%q must be a relative path inside the repository, without ..", im.Dockerfile)
	}
	if im.Context != "" && !safeRelative(im.Context) && im.Context != "." {
		v.add(p+".context", "%q must be a relative path inside the repository, without ..", im.Context)
	}
	if im.From != "" && (im.Context != "" || len(im.Args) > 0) {
		v.add(p, "context and args only make sense with dockerfile")
	}
	v.env(p+".args", im.Args)
}

func (v *validator) artifact(a *Artifact) {
	if a.Name != "" && !nameRE.MatchString(a.Name) {
		v.add("artifact.name", "%q is not a valid name (letters, digits, . _ -)", a.Name)
	}
	if len(a.Paths) == 0 {
		v.add("artifact.paths", "at least one path is required")
	}
	if len(a.Paths) > 50 || len(a.Exclude) > 50 {
		v.add("artifact", "too many paths or exclusions (the limit is 50 each)")
		return
	}
	bases := map[string]int{}
	for i, p := range a.Paths {
		if !safeRelative(p) {
			v.add(fmt.Sprintf("artifact.paths[%d]", i), "%q must be a relative path inside the build, without ..", p)
			continue
		}
		if strings.ContainsAny(p, "*?[") {
			for _, seg := range strings.Split(path.Clean(p), "/") {
				if _, err := path.Match(seg, ""); err != nil {
					v.add(fmt.Sprintf("artifact.paths[%d]", i), "%q is not a valid pattern", p)
					break
				}
			}
			continue // what a pattern matches is only known when the build has run
		}
		// Each path is stored in the archive under its base name, so two with the same one would collide.
		base := path.Base(path.Clean(p))
		if j, dup := bases[base]; dup {
			v.add(fmt.Sprintf("artifact.paths[%d]", i), "%q has the same base name as paths[%d]: both would be stored as %q", p, j, base)
		}
		bases[base] = i
	}
	for i, e := range a.Exclude {
		if e == "" || len(e) > 200 || strings.ContainsAny(e, "\x00\n") || hasDotDot(e) {
			v.add(fmt.Sprintf("artifact.exclude[%d]", i), "%q is not a valid pattern", e)
		}
	}
}

func (v *validator) deploy(d *Deploy) {
	if !targetRE.MatchString(d.Target) {
		v.add("deploy.target", "%q is not a target name: use the name of a target configured on the dymploy server (lowercase letters, digits and -), never a host or a user", d.Target)
	}
	v.duration("deploy.timeout", d.Timeout, time.Second, time.Hour)
	// Whether steps are required depends on the kind of the target, which only the server knows: an ssh target
	// needs them, an s3 one forbids them. The server checks it before building (deploy.Deployer.AuthorizeDeploy).
	if d.Path != "" && (len(d.Path) > 300 || strings.HasPrefix(d.Path, "/") || strings.HasPrefix(d.Path, "-") ||
		strings.ContainsAny(d.Path, "\\\x00\n") || hasDotDot(d.Path)) {
		v.add("deploy.path", "%q is not a valid path: relative, inside the bucket", d.Path)
	}
	v.deploySteps("deploy.steps", d.Steps)
	v.deploySteps("deploy.steps_undo", d.StepsUndo)
	if len(d.StepsUndo) > 0 && len(d.Steps) == 0 {
		v.add("deploy.steps_undo", "there is nothing to undo without deploy.steps")
	}
	if d.Finally != nil {
		if len(d.Finally.Steps) == 0 {
			v.add("deploy.finally.steps", "a finally section needs at least one step")
		}
		v.deploySteps("deploy.finally.steps", d.Finally.Steps)
	}
	v.deploySteps("deploy.secrets_steps", d.SecretsSteps)
	v.deploySteps("deploy.secrets_undo", d.SecretsUndo)
	if len(d.SecretsSteps) > 0 && !d.Secrets {
		v.add("deploy.secrets_steps", "they work on the target's secrets: set deploy.secrets to true")
	}
	if len(d.SecretsUndo) > 0 && len(d.SecretsSteps) == 0 {
		v.add("deploy.secrets_undo", "there is nothing to undo without deploy.secrets_steps")
	}
	if c := d.Check; c != nil {
		v.run("deploy.check.run", c.Run)
		if c.Attempts < 1 || c.Attempts > 120 {
			v.add("deploy.check.attempts", "must be between 1 and 120")
		}
		v.duration("deploy.check.interval", c.Interval, time.Second, 5*time.Minute)
		v.duration("deploy.check.timeout", c.Timeout, time.Second, 10*time.Minute)
	}
	if d.After != nil {
		if len(d.After.Steps) == 0 {
			v.add("deploy.after.steps", "an after section needs at least one step")
		}
		v.deploySteps("deploy.after.steps", d.After.Steps)
	}
}

func (v *validator) deploySteps(p string, steps []DeployStep) {
	if len(steps) > maxSteps {
		v.add(p, "too many steps (%d, the limit is %d)", len(steps), maxSteps)
		return
	}
	names := map[string]bool{}
	for i, s := range steps {
		sp := fmt.Sprintf("%s[%d]", p, i)
		v.label(sp, s.Name, names)
		v.run(sp+".run", s.Run)
		v.env(sp+".env", s.Env)
	}
}

func (v *validator) duration(p string, d Duration, min, max time.Duration) {
	switch {
	case d.bad != "":
		v.add(p, "%s", d.bad)
	case d.d < min || d.d > max:
		v.add(p, "must be between %s and %s", min, max)
	}
}

func (v *validator) label(p, name string, seen map[string]bool) {
	if name == "" {
		return
	}
	if !labelRE.MatchString(name) {
		v.add(p+".name", "%q is not a valid step name (letters, digits, spaces and . _ : / -, at most 64)", name)
		return
	}
	if seen[name] {
		v.add(p+".name", "duplicate step name %q", name)
	}
	seen[name] = true
}

func (v *validator) run(p, run string) {
	switch {
	case strings.TrimSpace(run) == "":
		v.add(p, "must not be empty")
	case len(run) > maxRunLength:
		v.add(p, "is too long (%d bytes, the limit is %d)", len(run), maxRunLength)
	case strings.ContainsRune(run, 0):
		v.add(p, "must not contain NUL characters")
	}
}

func (v *validator) env(p string, env map[string]string) {
	if len(env) > maxEnv {
		v.add(p, "too many variables (the limit is %d)", maxEnv)
		return
	}
	for k, val := range env {
		switch {
		case !envNameRE.MatchString(k):
			v.add(p, "%q is not a valid variable name", k)
		case strings.HasPrefix(strings.ToUpper(k), "DYMPLOY_"):
			v.add(p, "%q is reserved: names starting with DYMPLOY_ belong to dymploy", k)
		case len(val) > maxEnvValue || strings.ContainsRune(val, 0):
			v.add(p, "the value of %s is too long or contains a NUL character", k)
		}
	}
}

func hasDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// safeRelative accepts a relative path that stays inside its base and cannot be mistaken for an option.
func safeRelative(p string) bool {
	if p == "" || len(p) > 300 || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || strings.ContainsAny(p, "\\\x00\n") {
		return false
	}
	c := path.Clean(p)
	return c != ".." && !strings.HasPrefix(c, "../") && !hasDotDot(p)
}
