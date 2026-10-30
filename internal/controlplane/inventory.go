package controlplane

import (
	"context"
	"net"
	"sort"

	"github.com/dymmer-code/dymploy/internal/config"
)

// Inventory is what this daemon can do, said to the control plane so that it can be matched with its own records: the
// destinations the local catalogue has and the repositories that may deploy to them, with the installation of the
// provider's App that covers each one. It carries no secret and no path, and it grants nothing: the control plane can only
// choose among these, and a definition still has to be accepted locally.
type Inventory struct {
	SchemaVersion int                   `json:"schema_version"`
	Targets       []InventoryTarget     `json:"targets"`
	Repositories  []InventoryRepository `json:"repositories"`
}

// InventoryTarget is one destination of the local catalogue. Addresses are what Host resolves to, so that the other side
// can recognise the machine by address instead of by the text of a name.
type InventoryTarget struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Host       string   `json:"host,omitempty"`
	Addresses  []string `json:"addresses,omitempty"`
	Port       int      `json:"port,omitempty"`
	User       string   `json:"user,omitempty"`
	HasSecrets bool     `json:"has_secrets"`
}

// InventoryRepository is one repository that may deploy to the targets named, on the branches listed.
type InventoryRepository struct {
	Provider       string   `json:"provider"`
	Owner          string   `json:"owner"`
	Name           string   `json:"name"`
	Branches       []string `json:"branches"`
	Targets        []string `json:"targets"`
	InstallationID int64    `json:"installation_id,omitempty"`
}

// InstallationResolver says which installation of a provider's App covers a repository.
type InstallationResolver func(ctx context.Context, provider, owner, repo string) (int64, error)

// BuildInventory reads the catalogue of cfg. The resolver and the lookup are optional: without the first no installation
// is reported, without the second a host is only reported as it is written when it already is an address.
func BuildInventory(ctx context.Context, cfg *config.Config, resolve InstallationResolver, lookup func(host string) ([]string, error)) Inventory {
	inv := Inventory{SchemaVersion: 1, Targets: []InventoryTarget{}, Repositories: []InventoryRepository{}}
	if lookup == nil {
		lookup = net.LookupHost
	}
	names := make([]string, 0, len(cfg.Targets))
	for name := range cfg.Targets {
		names = append(names, name)
	}
	sort.Strings(names)

	type key struct{ provider, owner, name string }
	repos := map[key]*InventoryRepository{}
	var order []key
	for _, name := range names {
		t := cfg.Targets[name]
		kind := t.Kind
		if kind == "" {
			kind = config.KindSSH
		}
		entry := InventoryTarget{Name: name, Kind: kind, HasSecrets: t.Secrets != nil}
		if kind == config.KindSSH {
			entry.Host, entry.User, entry.Port = t.Host, t.User, t.Port
			if entry.Port == 0 {
				entry.Port = 22
			}
			if addresses, err := lookup(t.Host); err == nil {
				entry.Addresses = addresses
				sort.Strings(entry.Addresses)
			}
		}
		inv.Targets = append(inv.Targets, entry)

		for _, rule := range t.Allow {
			for _, repo := range rule.Repositories {
				k := key{rule.Provider, rule.Owner, repo}
				r := repos[k]
				if r == nil {
					r = &InventoryRepository{Provider: rule.Provider, Owner: rule.Owner, Name: repo, Branches: []string{}, Targets: []string{}}
					repos[k] = r
					order = append(order, k)
				}
				r.Branches = addAll(r.Branches, rule.Branches)
				r.Targets = addAll(r.Targets, []string{name})
			}
		}
	}
	for _, k := range order {
		r := repos[k]
		sort.Strings(r.Branches)
		sort.Strings(r.Targets)
		if resolve != nil {
			if id, err := resolve(ctx, k.provider, k.owner, k.name); err == nil {
				r.InstallationID = id
			}
		}
		inv.Repositories = append(inv.Repositories, *r)
	}
	return inv
}

func addAll(list, more []string) []string {
	for _, m := range more {
		found := false
		for _, l := range list {
			if l == m {
				found = true
				break
			}
		}
		if !found {
			list = append(list, m)
		}
	}
	return list
}
