// Package controlplane implements the optional, outgoing Dymmer integration.
package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrPending = errors.New("application requires local acceptance")
var ErrStaleRevision = fmt.Errorf("%w: configuration revision changed", ErrPending)

type Repository struct {
	Provider        string   `json:"provider"`
	Owner           string   `json:"owner"`
	Name            string   `json:"name"`
	Branches        []string `json:"branches"`
	Tags            []string `json:"tags"`
	InstallationIDs []int64  `json:"installation_ids"`
}
type Target struct {
	Name   string `json:"name"`
	HostID string `json:"host_id"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	// Addresses are those the control plane has for the host. They let the host be recognised as the machine of the local
	// catalogue even when the control plane writes its name differently (see CheckLocal).
	Addresses     []string `json:"addresses,omitempty"`
	UploadDir     string   `json:"upload_dir"`
	SSHKeyRef     string   `json:"ssh_key_ref"`
	KnownHostsRef string   `json:"known_hosts_ref"`
	SecretsRef    string   `json:"secrets_ref"`
}
type Application struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	OwnerID     string     `json:"owner_id"`
	Environment string     `json:"environment"`
	Repository  Repository `json:"repository"`
	Targets     []Target   `json:"targets"`
}

// Digest is what a person accepts. The addresses of a host are left out: they come from a daemon that reports them as they
// change, and they only help to recognise the machine, so a new one must not take back an approval.
func (a Application) Digest() string {
	a.Targets = slices.Clone(a.Targets)
	for i := range a.Targets {
		a.Targets[i].Addresses = nil
	}
	b, _ := json.Marshal(a)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (a Application) AllowsRef(ref string) bool {
	return slices.Contains(a.Repository.Branches, trimRef(ref, "refs/heads/")) && len(ref) > 11 && ref[:11] == "refs/heads/" || slices.Contains(a.Repository.Tags, trimRef(ref, "refs/tags/")) && len(ref) > 10 && ref[:10] == "refs/tags/"
}
func trimRef(ref, prefix string) string {
	if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
		return ref[len(prefix):]
	}
	return ""
}
func (a Application) RepositoryName() string {
	return a.Repository.Provider + "/" + a.Repository.Owner + "/" + a.Repository.Name
}
func (a Application) TargetNames() []string {
	out := make([]string, 0, len(a.Targets))
	for _, t := range a.Targets {
		out = append(out, t.Name)
	}
	return out
}

type Snapshot struct {
	SchemaVersion int           `json:"schema_version"`
	BuilderID     string        `json:"builder_id"`
	Revision      string        `json:"revision"`
	Applications  []Application `json:"applications"`
}
type Catalog struct{ Path, Source, Authority string }
type catalogState struct {
	Snapshot Snapshot
	Approved map[string]string
}

func (c Catalog) transaction(write bool, fn func(*catalogState) error) error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0700); err != nil {
		return err
	}
	db, err := bolt.Open(c.Path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	run := db.View
	if write {
		run = db.Update
	}
	return run(func(tx *bolt.Tx) error {
		state := catalogState{Approved: map[string]string{}}
		b := tx.Bucket([]byte("sources"))
		if b != nil {
			if raw := b.Get([]byte(c.Authority + "|" + c.Source)); raw != nil {
				if err := json.Unmarshal(raw, &state); err != nil {
					return err
				}
			}
		}
		if err := fn(&state); err != nil {
			return err
		}
		if !write {
			return nil
		}
		b, err = tx.CreateBucketIfNotExists([]byte("sources"))
		if err != nil {
			return err
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return b.Put([]byte(c.Authority+"|"+c.Source), raw)
	})
}
func (c Catalog) Update(s Snapshot) error {
	if s.SchemaVersion != 1 || s.BuilderID != c.Source || s.Applications == nil || len(s.Applications) > 1000 {
		return errors.New("invalid builder snapshot")
	}
	seen := map[string]bool{}
	for _, a := range s.Applications {
		if a.ID == "" || a.ProjectID == "" || a.OwnerID == "" || a.Repository.Provider == "" || a.Repository.Owner == "" || a.Repository.Name == "" || seen[a.ID] {
			return errors.New("invalid application")
		}
		seen[a.ID] = true
	}
	return c.transaction(true, func(state *catalogState) error {
		for id, digest := range state.Approved {
			match := false
			for _, a := range s.Applications {
				if a.ID == id && a.Digest() == digest {
					match = true
				}
			}
			if !match {
				delete(state.Approved, id)
			}
		}
		state.Snapshot = s
		return nil
	})
}
func (c Catalog) Applications() ([]Application, error) {
	var out []Application
	err := c.transaction(false, func(s *catalogState) error { out = s.Snapshot.Applications; return nil })
	return out, err
}
func (c Catalog) Accept(id, digest string) error {
	return c.transaction(true, func(s *catalogState) error {
		for _, a := range s.Snapshot.Applications {
			if a.ID == id && a.Digest() == digest {
				s.Approved[id] = digest
				return nil
			}
		}
		return ErrPending
	})
}
func (c Catalog) Revoke(id string) error {
	return c.transaction(true, func(s *catalogState) error { delete(s.Approved, id); return nil })
}
func (c Catalog) Accepted(id string) (Application, error) { return c.accepted(id, nil) }

// AcceptedAt checks the revision and approval in the same read transaction.
func (c Catalog) AcceptedAt(id, revision string) (Application, error) {
	return c.accepted(id, &revision)
}
func (c Catalog) accepted(id string, revision *string) (Application, error) {
	var out Application
	err := c.transaction(false, func(s *catalogState) error {
		if revision != nil && s.Snapshot.Revision != *revision {
			return ErrStaleRevision
		}
		for _, a := range s.Snapshot.Applications {
			if a.ID == id && s.Approved[id] == a.Digest() {
				out = a
				return nil
			}
		}
		return ErrPending
	})
	return out, err
}

// OriginSource prevents history from crossing control-plane authorities.
func (c Catalog) OriginSource() string {
	if c.Authority == "" {
		return c.Source
	}
	sum := sha256.Sum256([]byte(c.Authority + "|" + c.Source))
	return hex.EncodeToString(sum[:])
}
