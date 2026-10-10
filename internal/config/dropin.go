package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DropInDir is where the drop-in files live, next to the main configuration file.
const DropInDir = "config.d"

// source is one configuration file, read and syntax-checked.
type source struct {
	name string
	top  *yaml.Node // the top-level mapping; nil for an empty file
}

// dropIns lists config.d/*.yaml next to path in alphabetical order. The directory is optional. Files that
// could be changed by someone else are refused: policy decides what may be built and deployed.
func dropIns(path string) ([]string, error) {
	dir := filepath.Join(filepath.Dir(path), DropInDir)
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s must be a directory", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s can be written by others: restrict it (chmod go-w)", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := os.Stat(p) // follows links: what counts is the file that will be read
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", p)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("%s can be written by others: restrict it (chmod go-w)", p)
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// readSource reads one file and checks it on its own (syntax, unknown keys, types), so that errors name
// the file that has them. Cross-file rules are checked later, on the merged result.
func readSource(path string) (source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return source{}, fmt.Errorf("reading configuration %s: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return source{}, fmt.Errorf("parsing configuration %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(Default()); err != nil && !errors.Is(err, io.EOF) {
		return source{}, fmt.Errorf("parsing configuration %s: %w", path, err)
	}
	if root.Kind == 0 {
		return source{name: path}, nil
	}
	if err := noAliases(&root); err != nil {
		return source{}, fmt.Errorf("configuration %s: %w", path, err)
	}
	// The strict decoding above already refused anything but a mapping (or null, which is an empty file).
	if top := root.Content[0]; top.Kind == yaml.MappingNode {
		return source{name: path, top: top}, nil
	}
	return source{name: path}, nil
}

func noAliases(n *yaml.Node) error {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return fmt.Errorf("line %d: anchors and aliases are not supported", n.Line)
	}
	for _, c := range n.Content {
		if err := noAliases(c); err != nil {
			return err
		}
	}
	return nil
}

// Keys whose content is combined across files; every other setting may be given in one file only.
const (
	listKey = "policy"   // rules are added up
	mapKey  = "webhooks" // entries are added up, each name once
)

// merge combines the files, in order, into one mapping. Lists are concatenated, maps are merged and a
// name defined twice is an error; any other setting given twice is an error too, so what is in force
// never depends on which file happens to come last.
func merge(srcs []source) (*yaml.Node, error) {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	index := map[string]int{}    // key → position of its value in out.Content
	owner := map[string]string{} // key (or key.name) → file that defined it
	for _, s := range srcs {
		if s.top == nil {
			continue
		}
		for i := 0; i+1 < len(s.top.Content); i += 2 {
			k, v := s.top.Content[i], s.top.Content[i+1]
			pos, seen := index[k.Value]
			switch {
			case k.Value == listKey && v.Kind == yaml.SequenceNode:
				if !seen {
					index[k.Value] = len(out.Content) + 1
					out.Content = append(out.Content, k, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
					pos = index[k.Value]
				}
				out.Content[pos].Content = append(out.Content[pos].Content, v.Content...)
			case k.Value == mapKey && v.Kind == yaml.MappingNode:
				if !seen {
					index[k.Value] = len(out.Content) + 1
					out.Content = append(out.Content, k, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
					pos = index[k.Value]
				}
				for j := 0; j+1 < len(v.Content); j += 2 {
					name := v.Content[j].Value
					id := k.Value + "." + name
					if prev, dup := owner[id]; dup {
						return nil, fmt.Errorf("%s is defined in both %s and %s", id, prev, s.name)
					}
					owner[id] = s.name
					out.Content[pos].Content = append(out.Content[pos].Content, v.Content[j], v.Content[j+1])
				}
			default:
				if seen {
					return nil, fmt.Errorf("%s is set in both %s and %s: give it in one place", k.Value, owner[k.Value], s.name)
				}
				index[k.Value] = len(out.Content) + 1
				owner[k.Value] = s.name
				out.Content = append(out.Content, k, v)
			}
		}
	}
	return out, nil
}

// ControlSocket returns the control socket named by the configuration files without validating
// anything else, so that `dymploy reload` can still find the daemon when the new configuration is
// the broken one. The default applies when no file sets it.
func ControlSocket(path string) (string, error) {
	files := []string{path}
	extra, err := dropIns(path)
	if err != nil {
		return "", err
	}
	sock := Default().ControlSocket
	for _, f := range append(files, extra...) {
		raw, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		var part struct {
			ControlSocket string `yaml:"control_socket"`
		}
		if err := yaml.Unmarshal(raw, &part); err != nil {
			return "", fmt.Errorf("parsing configuration %s: %w", f, err)
		}
		if part.ControlSocket != "" {
			sock = part.ControlSocket
		}
	}
	return sock, nil
}
