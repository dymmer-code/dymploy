package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layout writes files (relative to a fresh directory) and returns the path of config.yaml.
func layout(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, ok := files["config.yaml"]; !ok {
		files["config.yaml"] = ""
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "config.yaml")
}

const hooks = "webhooks:\n  github:\n    secret_file: /etc/dymploy/s\n"

func rule(owner string) string {
	return "policy:\n  - {provider: github, owner: " + owner + ", repositories: [\"*\"], branches: [main]}\n"
}

func TestDropInsAreMergedInAlphabeticalOrder(t *testing.T) {
	p := layout(t, map[string]string{
		"config.yaml":             "listen: 127.0.0.1:9\n" + rule("first"),
		"config.d/20-policy.yaml": rule("third"),
		"config.d/10-hooks.yaml":  hooks + rule("second"),
		"config.d/README.txt":     "not yaml: ignored",
		"config.d/.hidden.yaml":   "this: is ignored",
	})
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var owners []string
	for _, r := range c.Policy {
		owners = append(owners, r.Owner)
	}
	if strings.Join(owners, ",") != "first,second,third" || c.Listen != "127.0.0.1:9" || c.Webhooks["github"].SecretFile != "/etc/dymploy/s" {
		t.Fatalf("owners=%v cfg=%+v", owners, c)
	}
}

func TestWebhooksFromSeveralFilesAreCombined(t *testing.T) {
	p := layout(t, map[string]string{
		"config.d/10-a.yaml": "webhooks:\n  github:\n    secret_file: /a\n",
		"config.d/20-b.yaml": "webhooks:\n  gitlab:\n    secret_file: /b\n",
	})
	c, err := Load(p)
	if err != nil || len(c.Webhooks) != 2 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestWithoutDropInsOrMainFileContentItStillWorks(t *testing.T) {
	if _, err := Load(layout(t, map[string]string{"config.yaml": ""})); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(layout(t, map[string]string{"config.yaml": "# only a comment\n", "config.d/10.yaml": "# empty\n"})); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(layout(t, map[string]string{"config.d/10.yaml": "null\n"})); err != nil {
		t.Fatalf("a file that is just null is an empty file: %v", err)
	}
	p := layout(t, map[string]string{"config.yaml": "", "config.d/10.yaml": "listen: 127.0.0.1:7\n"})
	if c, err := Load(p); err != nil || c.Listen != "127.0.0.1:7" {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestConflictsNameBothFiles(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  []string
	}{
		"a setting twice":      {map[string]string{"config.yaml": "listen: 127.0.0.1:1\n", "config.d/10.yaml": "listen: 127.0.0.1:2\n"}, []string{"listen is set in both", "config.yaml", "10.yaml"}},
		"a webhook twice":      {map[string]string{"config.d/10.yaml": hooks, "config.d/20.yaml": hooks}, []string{"webhooks.github is defined in both", "10.yaml", "20.yaml"}},
		"a section twice":      {map[string]string{"config.d/10.yaml": "queue: {max_concurrent: 1}\n", "config.d/20.yaml": "queue: {max_pending: 5}\n"}, []string{"queue is set in both"}},
		"a typo in a drop-in":  {map[string]string{"config.d/10.yaml": "lisen: x\n"}, []string{"10.yaml", "lisen"}},
		"broken yaml":          {map[string]string{"config.d/10.yaml": "listen: [\n"}, []string{"10.yaml"}},
		"not a mapping":        {map[string]string{"config.d/10.yaml": "- a\n- b\n"}, []string{"cannot unmarshal", "10.yaml"}},
		"an alias":             {map[string]string{"config.d/10.yaml": "listen: &a 127.0.0.1:1\n"}, []string{"anchors and aliases"}},
		"rule without webhook": {map[string]string{"config.d/10.yaml": rule("x")}, []string{"no entry under webhooks"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(layout(t, c.files))
			if err == nil {
				t.Fatal("accepted")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q: %v", w, err)
				}
			}
		})
	}
}

func TestFilesOthersCouldChangeAreRefused(t *testing.T) {
	p := layout(t, map[string]string{"config.d/10.yaml": "listen: 127.0.0.1:2\n"})
	dir := filepath.Join(filepath.Dir(p), "config.d")
	if err := os.Chmod(filepath.Join(dir, "10.yaml"), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "written by others") {
		t.Fatalf("file: %v", err)
	}
	os.Chmod(filepath.Join(dir, "10.yaml"), 0o640)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "written by others") {
		t.Fatalf("directory: %v", err)
	}
}

func TestOddDropInDirectories(t *testing.T) {
	p := layout(t, map[string]string{"config.d": "I am a file"})
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("%v", err)
	}
	p = layout(t, map[string]string{"config.d/10.yaml": ""})
	os.Remove(filepath.Join(filepath.Dir(p), "config.d", "10.yaml"))
	if err := os.Mkdir(filepath.Join(filepath.Dir(p), "config.d", "sub.yaml"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("a directory named .yaml is skipped: %v", err)
	}
	if err := os.Symlink("/nonexistent", filepath.Join(filepath.Dir(p), "config.d", "dangling.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("a dangling link cannot be read")
	}
	os.Remove(filepath.Join(filepath.Dir(p), "config.d", "dangling.yaml"))
	if err := os.Symlink("/dev/null", filepath.Join(filepath.Dir(p), "config.d", "null.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("%v", err)
	}
}

func TestAMissingMainFileIsStillAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("accepted")
	}
}

func TestUnreadableThingsAreReportedNotIgnored(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions do not apply to root")
	}
	// config.d that cannot be listed
	p := layout(t, map[string]string{"config.d/10.yaml": ""})
	dir := filepath.Join(filepath.Dir(p), "config.d")
	os.Chmod(dir, 0o000)
	if _, err := Load(p); err == nil {
		t.Fatal("a drop-in directory that cannot be read must fail")
	}
	os.Chmod(dir, 0o750)
	// a drop-in that cannot be read
	os.Chmod(filepath.Join(dir, "10.yaml"), 0o000)
	if _, err := Load(p); err == nil {
		t.Fatal("a drop-in that cannot be read must fail")
	}
	os.Chmod(filepath.Join(dir, "10.yaml"), 0o640)
	// the directory that holds config.d cannot be searched
	root := filepath.Dir(p)
	os.Chmod(root, 0o000)
	defer os.Chmod(root, 0o750)
	if _, err := Load(p); err == nil {
		t.Fatal("must fail")
	}
}

func TestControlSocketIsFoundEvenIfTheRestIsInvalid(t *testing.T) {
	p := layout(t, map[string]string{
		"config.yaml":      "listen: nonsense\ncontrol_socket: /run/a.sock\n",
		"config.d/10.yaml": "unknown_key: 1\n",
		"config.d/20.yaml": "control_socket: /run/b.sock\n",
	})
	if got, err := ControlSocket(p); err != nil || got != "/run/b.sock" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := ControlSocket(layout(t, map[string]string{})); err != nil || got != Default().ControlSocket {
		t.Fatalf("default: %q, %v", got, err)
	}
	if _, err := ControlSocket(layout(t, map[string]string{"config.yaml": "listen: [\n"})); err == nil {
		t.Fatal("unparseable YAML has no socket")
	}
	if _, err := ControlSocket(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("a missing file")
	}
	bad := layout(t, map[string]string{})
	os.MkdirAll(filepath.Join(filepath.Dir(bad), "config.d"), 0o777)
	os.Chmod(filepath.Join(filepath.Dir(bad), "config.d"), 0o777)
	if _, err := ControlSocket(bad); err == nil {
		t.Fatal("a drop-in directory others can write is refused here too")
	}
}

func TestTheExampleConfigurationLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "examples", "config", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Policy) != 2 || c.Webhooks["github"].AppID != 123456 || c.Runner.Kind != "docker" || c.Queue.OnBusy != "coalesce" {
		t.Fatalf("%+v", c)
	}
}
