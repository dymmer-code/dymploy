package manifest

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const minimal = `
version: 1
build:
  image: {from: "debian:13"}
  steps:
    - run: make
artifact:
  paths: [out/app]
`

const full = `
version: 1
build:
  image:
    dockerfile: docker/Dockerfile.build
    context: .
    args: {OTP_VERSION: "28.1"}
  env: {MIX_ENV: prod}
  network: none
  limits: {cpus: 2, memory: 2g, timeout: 20m, nofile: 65536}
  cache:
    - {name: hex, path: /root/.hex}
  steps:
    - name: deps
      run: mix deps.get --only prod
    - name: docs
      image: asciidoctor/docker-asciidoctor:latest
      workdir: doc
      env: {LANG: C.UTF-8}
      run: |
        asciidoctor book.adoc
artifact:
  name: shop
  paths: [_build/prod/rel/shop]
  exclude: ["*.log"]
deploy:
  target: production
  timeout: 15m
  steps:
    - name: stop
      run: sudo systemctl stop shop
    - name: install
      env: {MODE: replace}
      run: tar -xzf "$DYMPLOY_ARTIFACT" -C /srv/apps/shop
  check:
    run: curl -fsS http://127.0.0.1:4022/healthz
    attempts: 10
    interval: 3s
    timeout: 5s
  after:
    steps:
      - run: echo done
`

func parse(t *testing.T, s string) (*Manifest, error) {
	t.Helper()
	return Parse([]byte(s))
}

func mustParse(t *testing.T, s string) *Manifest {
	t.Helper()
	m, err := parse(t, s)
	if err != nil {
		t.Fatalf("expected a valid manifest: %v", err)
	}
	return m
}

func TestMinimalManifestGetsDefaults(t *testing.T) {
	m := mustParse(t, minimal)
	if m.Version != 1 || m.Build.Image.From != "debian:13" || len(m.Build.Steps) != 1 || m.Build.Steps[0].Run != "make" {
		t.Fatalf("%+v", m)
	}
	if m.Build.Network != "default" || m.Build.Limits.Timeout.D() != 30*time.Minute {
		t.Fatalf("defaults: %+v", m.Build)
	}
	if m.Deploy != nil || m.HasDeploy() {
		t.Fatal("a manifest without deploy only builds")
	}
}

func TestFullManifest(t *testing.T) {
	m := mustParse(t, full)
	b := m.Build
	if b.Image.Dockerfile != "docker/Dockerfile.build" || b.Image.Args["OTP_VERSION"] != "28.1" || b.Env["MIX_ENV"] != "prod" ||
		b.Network != "none" || b.Limits.CPUs == nil || *b.Limits.CPUs != 2 || b.Limits.Memory != "2g" || b.Limits.Timeout.D() != 20*time.Minute || b.Limits.NoFile == nil || *b.Limits.NoFile != 65536 ||
		len(b.Cache) != 1 || b.Cache[0].Path != "/root/.hex" {
		t.Fatalf("%+v", b)
	}
	if b.Steps[1].Image != "asciidoctor/docker-asciidoctor:latest" || b.Steps[1].Workdir != "doc" || b.Steps[1].Env["LANG"] != "C.UTF-8" {
		t.Fatalf("%+v", b.Steps[1])
	}
	if m.Artifact.Name != "shop" || m.Artifact.Paths[0] != "_build/prod/rel/shop" || m.Artifact.Exclude[0] != "*.log" {
		t.Fatalf("%+v", m.Artifact)
	}
	d := m.Deploy
	if !m.HasDeploy() || d.Target != "production" || d.Timeout.D() != 15*time.Minute || len(d.Steps) != 2 || d.Steps[1].Env["MODE"] != "replace" {
		t.Fatalf("%+v", d)
	}
	if d.Check == nil || d.Check.Attempts != 10 || d.Check.Interval.D() != 3*time.Second || d.Check.Timeout.D() != 5*time.Second {
		t.Fatalf("%+v", d.Check)
	}
	if d.After == nil || len(d.After.Steps) != 1 {
		t.Fatalf("%+v", d)
	}
}

func TestThereIsNoRollbackAndTheErrorSaysSo(t *testing.T) {
	_, err := Parse([]byte(minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n  rollback: {steps: [{run: y}]}\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown key "rollback" in deploy`) || !strings.Contains(err.Error(), "never undoes") {
		t.Fatalf("a removed key should be explained: %v", err)
	}
}

func TestCheckDefaults(t *testing.T) {
	m := mustParse(t, strings.Replace(full, "    attempts: 10\n    interval: 3s\n    timeout: 5s\n", "", 1))
	c := m.Deploy.Check
	if c.Attempts != 5 || c.Interval.D() != 3*time.Second || c.Timeout.D() != 10*time.Second {
		t.Fatalf("%+v", c)
	}
	if m.Deploy.Timeout.D() != 15*time.Minute {
		t.Fatal("explicit timeouts are kept")
	}
	noTimeout := mustParse(t, strings.Replace(full, "  timeout: 15m\n", "", 1))
	if noTimeout.Deploy.Timeout.D() != 10*time.Minute {
		t.Fatal("deploy timeout defaults to 10m")
	}
}

// Every case must be refused, and the message must say WHERE the problem is.
func TestInvalidManifests(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"empty", ``, "empty"},
		{"not a mapping", `- a`, "mapping"},
		{"no version", strings.Replace(minimal, "version: 1\n", "", 1), "version"},
		{"future version", strings.Replace(minimal, "version: 1", "version: 2", 1), "unsupported version 2"},
		{"version is not a number", strings.Replace(minimal, "version: 1", "version: one", 1), "version"},
		{"unknown top-level key", minimal + "buidl: {}\n", "buidl"},
		{"unknown build key", strings.Replace(minimal, "build:\n", "build:\n  stepz: []\n", 1), "stepz"},
		{"no build steps", strings.Replace(minimal, "  steps:\n    - run: make\n", "", 1), "build.steps"},
		{"no image", strings.Replace(minimal, "  image: {from: \"debian:13\"}\n", "", 1), "build.image"},
		{"image from and dockerfile", strings.Replace(minimal, `{from: "debian:13"}`, `{from: "debian:13", dockerfile: Dockerfile}`, 1), "build.image"},
		{"image with spaces", strings.Replace(minimal, "debian:13", "debian 13", 1), "build.image.from"},
		{"image as option", strings.Replace(minimal, "debian:13", "--privileged", 1), "build.image.from"},
		{"dockerfile absolute", strings.Replace(minimal, `{from: "debian:13"}`, `{dockerfile: /etc/passwd}`, 1), "build.image.dockerfile"},
		{"dockerfile escapes", strings.Replace(minimal, `{from: "debian:13"}`, `{dockerfile: ../x/Dockerfile}`, 1), "build.image.dockerfile"},
		{"empty run", strings.Replace(minimal, "run: make", `run: "  "`, 1), "build.steps[0].run"},
		{"step without run", strings.Replace(minimal, "- run: make", "- name: nothing", 1), "build.steps[0].run"},
		{"duplicate step names", strings.Replace(minimal, "- run: make", "- {name: a, run: x}\n    - {name: a, run: y}", 1), "duplicate"},
		{"bad step name", strings.Replace(minimal, "- run: make", "- {name: \"bad\\nname\", run: x}", 1), "build.steps[0].name"},
		{"step image with space", strings.Replace(minimal, "- run: make", "- {image: \"a b\", run: x}", 1), "build.steps[0].image"},
		{"workdir escapes", strings.Replace(minimal, "- run: make", "- {workdir: ../../etc, run: x}", 1), "build.steps[0].workdir"},
		{"env name", strings.Replace(minimal, "  steps:", "  env: {\"bad-name\": x}\n  steps:", 1), "build.env"},
		{"reserved env name", strings.Replace(minimal, "  steps:", "  env: {DYMPLOY_COMMIT: x}\n  steps:", 1), "DYMPLOY_"},
		{"network", strings.Replace(minimal, "  steps:", "  network: host\n  steps:", 1), "build.network"},
		{"cpus", strings.Replace(minimal, "  steps:", "  limits: {cpus: 0}\n  steps:", 1), "build.limits.cpus"},
		{"too many cpus", strings.Replace(minimal, "  steps:", "  limits: {cpus: 1000}\n  steps:", 1), "build.limits.cpus"},
		{"memory", strings.Replace(minimal, "  steps:", "  limits: {memory: lots}\n  steps:", 1), "build.limits.memory"},
		{"nofile zero", strings.Replace(minimal, "  steps:", "  limits: {nofile: 0}\n  steps:", 1), "build.limits.nofile"},
		{"nofile negative", strings.Replace(minimal, "  steps:", "  limits: {nofile: -1}\n  steps:", 1), "build.limits.nofile"},
		{"nofile too big", strings.Replace(minimal, "  steps:", "  limits: {nofile: 2000000}\n  steps:", 1), "build.limits.nofile"},
		{"timeout without unit", strings.Replace(minimal, "  steps:", "  limits: {timeout: 20}\n  steps:", 1), "timeout"},
		{"timeout too long", strings.Replace(minimal, "  steps:", "  limits: {timeout: 99h}\n  steps:", 1), "build.limits.timeout"},
		{"cache path relative", strings.Replace(minimal, "  steps:", "  cache: [{name: a, path: relative}]\n  steps:", 1), "build.cache[0].path"},
		{"cache path escapes", strings.Replace(minimal, "  steps:", "  cache: [{name: a, path: /a/../etc}]\n  steps:", 1), "build.cache[0].path"},
		{"no artifact paths", strings.Replace(minimal, "paths: [out/app]", "name: x", 1), "artifact.paths"},
		{"artifact absolute", strings.Replace(minimal, "out/app", "/etc/passwd", 1), "artifact.paths[0]"},
		{"artifact escapes", strings.Replace(minimal, "out/app", "../../etc", 1), "artifact.paths[0]"},
		{"artifact name", strings.Replace(minimal, "paths:", "name: \"../x\"\n  paths:", 1), "artifact.name"},
		{"deploy without target", minimal + "deploy:\n  steps:\n    - run: x\n", "deploy.target"},
		{"deploy target is a host", minimal + "deploy:\n  target: root@10.0.0.1\n  steps:\n    - run: x\n", "deploy.target"},
		{"deploy without steps", minimal + "deploy:\n  target: production\n", "deploy.steps"},
		{"deploy step image", minimal + "deploy:\n  target: production\n  steps:\n    - {run: x, image: foo}\n", "image"},
		{"check without run", minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n  check: {attempts: 3}\n", "deploy.check.run"},
		{"check too many retries", minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n  check: {run: y, attempts: 1000}\n", "deploy.check.attempts"},
		{"empty after", minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n  after: {steps: []}\n", "deploy.after.steps"},
		{"run too long", strings.Replace(minimal, "run: make", "run: "+strings.Repeat("x", 9000), 1), "build.steps[0].run"},
		{"too many steps", strings.Replace(minimal, "    - run: make\n", strings.Repeat("    - run: x\n", 101), 1), "build.steps"},
		{"nul in env", strings.Replace(minimal, "  steps:", "  env: {A: \"a\\0b\"}\n  steps:", 1), "build.env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(t, c.yaml)
			if err == nil {
				t.Fatalf("must be refused:\n%s", c.yaml)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the message should mention %q, got:\n%v", c.want, err)
			}
		})
	}
}

func TestAllProblemsAreReportedAtOnce(t *testing.T) {
	bad := `
version: 1
build:
  image: {from: "bad image"}
  network: host
  steps:
    - name: x
artifact:
  paths: [/abs]
`
	_, err := parse(t, bad)
	var errs Errors
	if !errors.As(err, &errs) || len(errs) < 4 {
		t.Fatalf("a developer fixes everything in one go: %v", err)
	}
	for _, want := range []string{"build.image.from", "build.network", "build.steps[0].run", "artifact.paths[0]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in\n%v", want, err)
		}
	}
}

func TestSyntaxErrorsCarryTheLine(t *testing.T) {
	_, err := parse(t, "version: 1\nbuild:\n  image: {from: x}\n  steps: [\n")
	if err == nil || !strings.Contains(err.Error(), "line") {
		t.Fatalf("%v", err)
	}
	_, err = parse(t, "version: 1\nbuild:\n  imagen: x\n")
	if err == nil || !strings.Contains(err.Error(), "line 3") || !strings.Contains(err.Error(), "imagen") {
		t.Fatalf("an unknown key is reported with its line: %v", err)
	}
	if !strings.Contains(err.Error(), `did you mean "image"`) || strings.Contains(err.Error(), "manifest.") {
		t.Fatalf("suggest the likely key and never show internal type names: %v", err)
	}
}

func TestUnknownKeysSaySoInPlainWords(t *testing.T) {
	cases := map[string]string{
		"version: 1\nbuidl: {}\n":                                  `unknown key "buidl" at the top level (did you mean "build"?)`,
		"version: 1\nbuild:\n  stepz: []\n":                        `unknown key "stepz" in build (did you mean "steps"?)`,
		"version: 1\nbuild:\n  image: {frm: x}\n":                  `unknown key "frm" in build.image (did you mean "from"?)`,
		"version: 1\nbuild:\n  steps: [{runn: x}]\n":               `unknown key "runn" in a build step (did you mean "run"?)`,
		"version: 1\nbuild:\n  limits: {memry: 1g}\n":              `in build.limits`,
		"version: 1\nartifact:\n  pats: []\n":                      `unknown key "pats" in artifact (did you mean "paths"?)`,
		"version: 1\ndeploy:\n  targt: x\n":                        `unknown key "targt" in deploy (did you mean "target"?)`,
		"version: 1\ndeploy:\n  steps: [{run: x, image: y}]\n":     `in a deploy step`,
		"version: 1\ndeploy:\n  check: {runn: x}\n":                `in deploy.check`,
		"version: 1\ndeploy:\n  after: {stepz: []}\n":              `in deploy.after`,
		"version: 1\nbuild:\n  cache: [{nme: a}]\n":                `in a build cache entry`,
		"version: 1\nbuild:\n  limits: {cpus: 1, zzzzzzzzzz: 1}\n": `unknown key "zzzzzzzzzz"`,
	}
	for src, want := range cases {
		_, err := Parse([]byte(src))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q\nwant %q\ngot  %v", src, want, err)
		}
	}
}

func TestDurationProblemsAreReportedWithTheirFieldAndTogetherWithTheRest(t *testing.T) {
	bad := strings.Replace(minimal, "  steps:", "  limits: {timeout: 20}\n  network: host\n  steps:", 1)
	_, err := parse(t, bad)
	if err == nil || !strings.Contains(err.Error(), "build.limits.timeout") || !strings.Contains(err.Error(), "build.network") || !strings.Contains(err.Error(), "unit") {
		t.Fatalf("the unit problem names its field and does not hide the others: %v", err)
	}
}

func TestOversizedAndHostileInput(t *testing.T) {
	if _, err := Parse([]byte(minimal + "# " + strings.Repeat("x", MaxSize))); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("%v", err)
	}
	bomb := "a: &a [x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\nd: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\n"
	if _, err := Parse([]byte("version: 1\n" + bomb)); err == nil {
		t.Fatal("unknown keys (and alias bombs) are refused")
	}
	if _, err := Parse([]byte(strings.Replace(minimal, "build:", "x: &x {a: 1}\nbuild:", 1))); err == nil {
		t.Fatal("anchors have no place in this format")
	}
}

func TestFind(t *testing.T) {
	fsys := fstest.MapFS{"dymploy.yaml": {Data: []byte(minimal)}}
	if name, err := Find(fsys); err != nil || name != "dymploy.yaml" {
		t.Fatalf("%q %v", name, err)
	}
	if name, err := Find(fstest.MapFS{"dymploy.yml": {Data: []byte(minimal)}}); err != nil || name != "dymploy.yml" {
		t.Fatalf("%q %v", name, err)
	}
	both := fstest.MapFS{"dymploy.yaml": {}, "dymploy.yml": {}}
	if _, err := Find(both); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("ambiguity is an error: %v", err)
	}
	if _, err := Find(fstest.MapFS{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestCheckFilesNeedsTheDockerfile(t *testing.T) {
	m := mustParse(t, strings.Replace(minimal, `{from: "debian:13"}`, `{dockerfile: build/Dockerfile, context: build}`, 1))
	if err := m.CheckFiles(fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "build/Dockerfile") {
		t.Fatalf("a missing Dockerfile is found before the build: %v", err)
	}
	if err := m.CheckFiles(fstest.MapFS{"build/Dockerfile": {}}); err != nil {
		t.Fatal(err)
	}
	if err := mustParse(t, minimal).CheckFiles(fstest.MapFS{}); err != nil {
		t.Fatalf("an image to pull needs no files: %v", err)
	}
}

func TestSummary(t *testing.T) {
	s := mustParse(t, full).Summary()
	for _, want := range []string{"build: 2 steps", "artifact: shop", "deploy: production"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
	if s := mustParse(t, minimal).Summary(); strings.Contains(s, "deploy") || !strings.Contains(s, "build: 1 step,") {
		t.Fatalf("%q", s)
	}
}

func TestDeploySecretsIsAnOptInBoolean(t *testing.T) {
	m, err := Parse([]byte(minimal + "deploy:\n  target: production\n  secrets: true\n  steps: [{run: x}]\n"))
	if err != nil || !m.Deploy.Secrets {
		t.Fatalf("%v", err)
	}
	m, err = Parse([]byte(minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n"))
	if err != nil || m.Deploy.Secrets {
		t.Fatalf("secrets are off unless asked for: %v", err)
	}
	// the definition can say THAT it wants secrets, never from where
	if _, err := Parse([]byte(minimal + "deploy:\n  target: production\n  secrets: {project: other}\n  steps: [{run: x}]\n")); err == nil {
		t.Fatal("a definition must not be able to name a secrets source")
	}
}
