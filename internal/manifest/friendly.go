package manifest

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

var unknownKeyRE = regexp.MustCompile(`line (\d+): field (\S+) not found in type manifest\.(\w+)`)

// places names, in plain words, where each type appears in the file.
var places = map[string]string{
	"Manifest":   "the top level",
	"Build":      "build",
	"Image":      "build.image",
	"Step":       "a build step",
	"Cache":      "a build cache entry",
	"Limits":     "build.limits",
	"Artifact":   "artifact",
	"Deploy":     "deploy",
	"DeployStep": "a deploy step",
	"StepList":   "deploy.after",
	"Check":      "deploy.check",
}

var types = map[string]reflect.Type{
	"Manifest": reflect.TypeOf(Manifest{}), "Build": reflect.TypeOf(Build{}), "Image": reflect.TypeOf(Image{}),
	"Step": reflect.TypeOf(Step{}), "Cache": reflect.TypeOf(Cache{}), "Limits": reflect.TypeOf(Limits{}),
	"Artifact": reflect.TypeOf(Artifact{}), "Deploy": reflect.TypeOf(Deploy{}), "DeployStep": reflect.TypeOf(DeployStep{}),
	"StepList": reflect.TypeOf(StepList{}), "Check": reflect.TypeOf(Check{}),
}

// friendly rewrites the YAML library's message for an unknown key into plain words, with a suggestion.
func friendly(msg string) string {
	m := unknownKeyRE.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	line, key, typ := m[1], m[2], m[3]
	out := fmt.Sprintf("line %s: unknown key %q", line, key)
	if place, ok := places[typ]; ok {
		in := "in " + place
		if place == "the top level" {
			in = "at the top level"
		}
		out += " " + in
	}
	if typ == "Deploy" && key == "rollback" {
		return out + " (there is no rollback: dymploy never undoes a deployment on its own)"
	}
	if best := closest(key, keysOf(types[typ])); best != "" {
		out += fmt.Sprintf(" (did you mean %q?)", best)
	}
	return out
}

func keysOf(t reflect.Type) []string {
	if t == nil {
		return nil
	}
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		keys = append(keys, yamlName(t.Field(i)))
	}
	return keys
}

func yamlName(f reflect.StructField) string { return strings.Split(f.Tag.Get("yaml"), ",")[0] }

// closest returns the candidate within edit distance 2 of key, if there is one.
func closest(key string, candidates []string) string {
	best, bestD := "", 3
	for _, c := range candidates {
		if d := distance(key, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
