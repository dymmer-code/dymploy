package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

func examplesDir(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "examples")
}

func TestEveryExampleIsValid(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(examplesDir(t), "*.yaml"))
	if len(files) < 3 {
		t.Fatalf("expected the shipped examples, found %v", files)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(b); err != nil {
				t.Fatalf("an example that does not validate teaches the wrong thing: %v", err)
			}
		})
	}
}

func TestTheExamplesCoverDeployingAndNot(t *testing.T) {
	read := func(n string) *Manifest {
		b, _ := os.ReadFile(filepath.Join(examplesDir(t), n))
		return mustParse(t, string(b))
	}
	if !read("elixir-release.yaml").HasDeploy() || !read("static-site.yaml").HasDeploy() {
		t.Fatal("the release and static site examples deploy")
	}
	if read("book.yaml").HasDeploy() {
		t.Fatal("the book example only builds")
	}
}

func TestInvalidExamplesAreRefusedForTheStatedReason(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(examplesDir(t), "invalid", "*.yaml"))
	if len(files) == 0 {
		t.Fatal("no invalid examples")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, _ := os.ReadFile(f)
			first := strings.SplitN(string(b), "\n", 2)[0]
			want := strings.TrimSpace(strings.TrimPrefix(first, "# expect:"))
			if want == "" || want == first {
				t.Fatalf("the first line must be `# expect: <text>`: %q", first)
			}
			_, err := Parse(b)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected an error mentioning %q, got %v", want, err)
			}
		})
	}
}

// --- the JSON Schema must describe exactly what the parser accepts ---

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(Schema(), &s); err != nil {
		t.Fatalf("the embedded schema is not JSON: %v", err)
	}
	return s
}

func resolve(root, node map[string]any) map[string]any {
	for {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node
		}
		node = root["$defs"].(map[string]any)[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
	}
}

func underlying(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t
}

func walk(t *testing.T, root, node map[string]any, typ reflect.Type, where string) {
	t.Helper()
	node = resolve(root, node)
	typ = underlying(typ)
	if typ.Kind() != reflect.Struct || typ.Name() == "Duration" {
		return
	}
	props, _ := node["properties"].(map[string]any)
	if node["additionalProperties"] != false {
		t.Errorf("%s: objects must set additionalProperties: false, like the parser does", where)
	}
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := yamlName(f)
		fields = append(fields, name)
		sub, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("%s: the Go field %q has no property in the schema", where, name)
			continue
		}
		if items, ok := resolve(root, sub)["items"].(map[string]any); ok {
			sub = items
		}
		walk(t, root, sub, f.Type, where+"."+name)
	}
	var inSchema []string
	for k := range props {
		inSchema = append(inSchema, k)
	}
	sort.Strings(fields)
	sort.Strings(inSchema)
	if !reflect.DeepEqual(fields, inSchema) {
		t.Errorf("%s: schema properties %v differ from the Go fields %v", where, inSchema, fields)
	}
}

func TestSchemaAndParserDescribeTheSameFields(t *testing.T) {
	root := loadSchema(t)
	walk(t, root, root, reflect.TypeOf(Manifest{}), "$")
}

func TestSchemaRequiredKeysMatchTheParser(t *testing.T) {
	root := loadSchema(t)
	var req []string
	for _, r := range root["required"].([]any) {
		req = append(req, r.(string))
	}
	sort.Strings(req)
	if strings.Join(req, ",") != "artifact,build,version" {
		t.Fatalf("%v", req)
	}
	// and the parser really requires them
	for _, drop := range []string{"build", "artifact"} {
		src := minimal
		if drop == "build" {
			src = "version: 1\nartifact:\n  paths: [out]\n"
		} else {
			src = "version: 1\nbuild:\n  image: {from: \"debian:13\"}\n  steps: [{run: make}]\n"
		}
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("a manifest without %s must be refused", drop)
		}
	}
}

func TestSchemaIsServedByThePackage(t *testing.T) {
	if len(Schema()) == 0 || !strings.Contains(string(Schema()), "dymploy.yaml") {
		t.Fatal("empty schema")
	}
}

// --- more refusals ---

func TestMoreInvalidManifests(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"context without dockerfile", strings.Replace(minimal, `{from: "debian:13"}`, `{from: "debian:13", context: "."}`, 1), "dockerfile"},
		{"bad cache name", strings.Replace(minimal, "  steps:", "  cache: [{name: BAD NAME, path: /a}]\n  steps:", 1), "build.cache[0].name"},
		{"duplicate cache", strings.Replace(minimal, "  steps:", "  cache: [{name: a, path: /a}, {name: a, path: /b}]\n  steps:", 1), "duplicate cache"},
		{"bad step image", strings.Replace(minimal, "- run: make", "- {image: \"-x\", run: make}", 1), "build.steps[0].image"},
		{"huge env", strings.Replace(minimal, "  steps:", "  env: {A: \""+strings.Repeat("x", 5000)+"\"}\n  steps:", 1), "build.env"},
		{"exclude with ..", strings.Replace(minimal, "paths: [out/app]", "paths: [out/app]\n  exclude: [\"../x\"]", 1), "artifact.exclude[0]"},
		{"two paths one base name", strings.Replace(minimal, "paths: [out/app]", "paths: [a/app, b/app]", 1), "base name"},
		{"path starts with dash", strings.Replace(minimal, "out/app", "-rf", 1), "artifact.paths[0]"},
		{"too many paths", strings.Replace(minimal, "paths: [out/app]", "paths: ["+strings.TrimSuffix(strings.Repeat("p, ", 51), ", ")+"]", 1), "artifact"},
		{"deploy timeout too long", minimal + "deploy:\n  target: production\n  timeout: 5h\n  steps: [{run: x}]\n", "deploy.timeout"},
		{"check interval", minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n  check: {run: y, interval: 1h}\n", "deploy.check.interval"},
		{"check timeout", minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n  check: {run: y, timeout: 1h}\n", "deploy.check.timeout"},
		{"deploy step env", minimal + "deploy:\n  target: production\n  steps: [{run: x, env: {DYMPLOY_X: y}}]\n", "DYMPLOY_"},
		{"nul in run", strings.Replace(minimal, "run: make", "run: \"a\\0b\"", 1), "NUL"},
		{"negative duration", strings.Replace(minimal, "  steps:", "  limits: {timeout: -5m}\n  steps:", 1), "timeout"},
		{"garbage duration", strings.Replace(minimal, "  steps:", "  limits: {timeout: soon}\n  steps:", 1), "timeout"},
		{"too many deploy steps", minimal + "deploy:\n  target: production\n  steps:\n" + strings.Repeat("    - run: x\n", 101), "deploy.steps"},
		{"version as float", strings.Replace(minimal, "version: 1", "version: 1.5", 1), "version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(t, c.yaml)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}

func TestArtifactNameDefaults(t *testing.T) {
	if n := mustParse(t, minimal).Artifact.Name; n != "app" {
		t.Fatalf("the default name is the base name of the first path, got %q", n)
	}
	if n := mustParse(t, strings.Replace(minimal, "out/app", "dist/", 1)).Artifact.Name; n != "dist" {
		t.Fatalf("%q", n)
	}
}

// --- the schema must agree with the parser on real files ---

func schemaValidate(t *testing.T, yamlText string) error {
	t.Helper()
	var doc any
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	var raw any
	if err := json.Unmarshal(Schema(), &raw); err != nil {
		t.Fatal(err)
	}
	if err := c.AddResource("dymploy.json", raw); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("dymploy.json")
	if err != nil {
		t.Fatalf("the schema does not compile: %v", err)
	}
	// round-trip through JSON so numbers and maps have the types a JSON document would
	b, _ := json.Marshal(doc)
	var inst any
	_ = json.Unmarshal(b, &inst)
	return sch.Validate(inst)
}

func TestTheSchemaAcceptsTheValidExamples(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(examplesDir(t), "*.yaml"))
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, _ := os.ReadFile(f)
			if err := schemaValidate(t, string(b)); err != nil {
				t.Fatalf("the editor would flag a valid file: %v", err)
			}
		})
	}
	for _, src := range []string{minimal, full} {
		if err := schemaValidate(t, src); err != nil {
			t.Fatalf("%v", err)
		}
	}
}

func TestTheSchemaRejectsTheInvalidExamples(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(examplesDir(t), "invalid", "*.yaml"))
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, _ := os.ReadFile(f)
			if err := schemaValidate(t, string(b)); err == nil {
				t.Fatal("the editor would not flag a file the parser refuses")
			}
		})
	}
}

func TestTheSchemaAndTheParserAgreeOnValues(t *testing.T) {
	cases := map[string]string{
		"bare number timeout":  strings.Replace(minimal, "  steps:", "  limits: {timeout: 20}\n  steps:", 1),
		"host as target":       minimal + "deploy:\n  target: root@host\n  steps: [{run: x}]\n",
		"absolute artifact":    strings.Replace(minimal, "out/app", "/etc/passwd", 1),
		"escaping artifact":    strings.Replace(minimal, "out/app", "../x", 1),
		"bad network":          strings.Replace(minimal, "  steps:", "  network: host\n  steps:", 1),
		"bad memory":           strings.Replace(minimal, "  steps:", "  limits: {memory: lots}\n  steps:", 1),
		"zero cpus":            strings.Replace(minimal, "  steps:", "  limits: {cpus: 0}\n  steps:", 1),
		"empty steps":          strings.Replace(minimal, "  steps:\n    - run: make\n", "  steps: []\n", 1),
		"relative cache path":  strings.Replace(minimal, "  steps:", "  cache: [{name: a, path: rel}]\n  steps:", 1),
		"too many attempts":    minimal + "deploy:\n  target: production\n  steps: [{run: x}]\n  check: {run: y, attempts: 1000}\n",
		"image and dockerfile": strings.Replace(minimal, `{from: "debian:13"}`, `{from: "debian:13", dockerfile: Dockerfile}`, 1),
		"both missing":         strings.Replace(minimal, `  image: {from: "debian:13"}`, `  image: {}`, 1),
		"version two":          strings.Replace(minimal, "version: 1", "version: 2", 1),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatal("the parser must refuse it")
			}
			if err := schemaValidate(t, src); err == nil {
				t.Fatal("the schema must refuse it too")
			}
		})
	}
}
