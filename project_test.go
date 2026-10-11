package harnesssdk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectProfilesNeedTrust(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("p", "harness: fake\neffect: pure\nmode: echo\ndescription: global")
	repo := t.TempDir()
	sub := filepath.Join(repo, "src", "pkg")
	os.MkdirAll(sub, 0o755)
	proj := filepath.Join(repo, ".harness")
	os.MkdirAll(filepath.Join(proj, "profiles"), 0o755)
	path := filepath.Join(proj, "profiles", "p.md")
	os.WriteFile(path, []byte("---\nharness: fake\neffect: pure\nmode: args\ndescription: project\n---\n"), 0o644)

	// A project file that nobody trusted is refused, not silently replaced
	// by the global one.
	if _, err := f.home.Profile("p", sub); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("got %v", err)
	}
	entries, err := f.home.TrustStatus(proj)
	if err != nil || len(entries) != 1 || entries[0].State != "new" {
		t.Fatalf("%+v %v", entries, err)
	}
	if _, err := f.home.ApproveProject(proj); err != nil {
		t.Fatal(err)
	}
	p, err := f.home.Profile("p", sub)
	if err != nil || p.Scope != "project" || p.Description != "project" {
		t.Fatalf("%+v %v", p, err)
	}
	// Outside the project the global profile applies.
	if p, err := f.home.Profile("p", t.TempDir()); err != nil || p.Scope != "global" {
		t.Fatalf("%+v %v", p, err)
	}
	// Any change needs trusting again.
	os.WriteFile(path, []byte("---\nharness: fake\neffect: pure\nmode: args\nextra_args: --dangerous\n---\n"), 0o644)
	if _, err := f.home.Profile("p", sub); err == nil || !strings.Contains(err.Error(), "changed since it was trusted") {
		t.Fatalf("got %v", err)
	}
}

func TestProjectWritesAreTrusted(t *testing.T) {
	f := newFixture(t, nil)
	repo := t.TempDir()
	proj, err := f.home.ProjectDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ParseProfile("local", []byte("---\nharness: fake\neffect: pure\nmode: echo\n---\n"))
	if err := f.home.SaveProfile(p, proj, repo); err != nil {
		t.Fatal(err)
	}
	got, err := f.home.Profile("local", repo)
	if err != nil || got.Scope != "project" {
		t.Fatalf("%+v %v", got, err)
	}
	items, err := f.home.List("profiles", repo)
	if err != nil || len(items) != 1 || items[0].Scope != "project" {
		t.Fatalf("%+v %v", items, err)
	}
	// Runs in the project use it.
	f.r, _ = NewRunner(f.home)
	reps, err := f.r.Start(t.Context(), "local", []string{"hi"}, repo, nil, nil)
	if err != nil || reps[0].Output != "echo: hi" {
		t.Fatalf("%+v %v", reps, err)
	}
}

// The global data folder is never mistaken for a project, even when a run's
// folder is inside the folder that holds it.
func TestGlobalHomeIsNotAProject(t *testing.T) {
	base := t.TempDir()
	h, err := NewHome(filepath.Join(base, ".harness"))
	if err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(base, "work")
	os.MkdirAll(inner, 0o755)
	if dir, ok := h.FindProject(inner); ok {
		t.Fatalf("found %s", dir)
	}
}
