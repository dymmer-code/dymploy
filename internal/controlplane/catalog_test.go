package controlplane

import (
	"path/filepath"
	"testing"
)

func TestAcceptancePinsDefinitionAndSource(t *testing.T) {
	c := Catalog{Path: filepath.Join(t.TempDir(), "catalog.db"), Source: "builder-a"}
	s := Snapshot{SchemaVersion: 1, BuilderID: "builder-a", Applications: []Application{{ID: "app", ProjectID: "project", OwnerID: "owner", Repository: Repository{Provider: "github", Owner: "acme", Name: "app", Branches: []string{"main"}}}}}
	if err := c.Update(s); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Accepted("app"); err == nil {
		t.Fatal("unapproved app accepted")
	}
	digest := s.Applications[0].Digest()
	if err := c.Accept("app", "old"); err == nil {
		t.Fatal("stale acceptance")
	}
	if err := c.Accept("app", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Accepted("app"); err != nil {
		t.Fatal(err)
	}
	s.Applications[0].OwnerID = "other"
	if err := c.Update(s); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Accepted("app"); err == nil {
		t.Fatal("ownership change remained accepted")
	}
	if err := c.Accept("app", s.Applications[0].Digest()); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke("app"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Accepted("app"); err == nil {
		t.Fatal("revoked app accepted")
	}
	s.BuilderID = "builder-b"
	if err := c.Update(s); err == nil {
		t.Fatal("source confusion")
	}
}

func TestCatalogueRemovalAndAuthorityInvalidateAcceptance(t *testing.T) {
	s, _, snapshot := testService(t)
	apps, err := s.Catalog.Applications()
	if err != nil || len(apps) != 1 {
		t.Fatal(apps, err)
	}
	other := s.Catalog
	other.Authority = "wss://other.example/builders/websocket"
	if _, err := other.Accepted("app-a"); err == nil {
		t.Fatal("acceptance leaked to another authority")
	}
	empty := snapshot
	empty.Applications = []Application{}
	if err := s.Catalog.Update(empty); err != nil {
		t.Fatal(err)
	}
	if err := s.Catalog.Update(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Catalog.Accepted("app-a"); err == nil {
		t.Fatal("removed app silently accepted again")
	}
	snapshot.Applications = append(snapshot.Applications, snapshot.Applications[0])
	if s.Catalog.Update(snapshot) == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestHistorySourceIncludesControlPlaneAuthority(t *testing.T) {
	a := Catalog{Source: "builder", Authority: "wss://one.example/builders/websocket"}
	b := a
	b.Authority = "wss://two.example/builders/websocket"
	if a.OriginSource() == b.OriginSource() || len(a.OriginSource()) != 64 {
		t.Fatal("history authority collision")
	}
}

func TestApprovalAndRevisionMustMatchTheSameSnapshot(t *testing.T) {
	s, _, snapshot := testService(t)
	if _, err := s.Catalog.AcceptedAt("app-a", snapshot.Revision); err != nil {
		t.Fatal(err)
	}
	snapshot.Revision = "changed"
	snapshot.Applications[0].Targets = []Target{{Name: "other"}}
	_ = s.Catalog.Update(snapshot)
	a := snapshot.Applications[0]
	_ = s.Catalog.Accept(a.ID, a.Digest())
	if _, err := s.Catalog.AcceptedAt(a.ID, ""); err != ErrStaleRevision {
		t.Fatal("old request used new approval", err)
	}
	if _, err := s.Catalog.AcceptedAt(a.ID, "changed"); err != nil {
		t.Fatal(err)
	}
}
