package services

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/git"
)

func TestDestinationLayouts(t *testing.T) {
	base := t.TempDir()
	r := domain.Repository{Name: "project", Namespace: "group/sub"}
	cases := map[string]string{
		LayoutFlat:      filepath.Join(base, "project"),
		LayoutNamespace: filepath.Join(base, "group", "sub", "project"),
		LayoutHost:      filepath.Join(base, "gitlab.com", "group", "sub", "project"),
	}
	for layout, want := range cases {
		got, err := Destination(base, layout, "gitlab.com", r)
		if err != nil || got != want {
			t.Errorf("Destination(%s) = %q, %v; want %q", layout, got, err, want)
		}
	}
}

func TestDestinationRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	for _, r := range []domain.Repository{
		{Name: "..", Namespace: "x"},
		{Name: "ok", Namespace: "../../etc"},
		{Name: "a/b", Namespace: "x"},
		{Name: "", Namespace: "x"},
	} {
		if _, err := Destination(base, LayoutNamespace, "h", r); err == nil {
			t.Errorf("Destination accepted unsafe repo %+v", r)
		}
	}
}

func makeRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "--quiet"}, {"-c", "user.name=t", "-c", "user.email=t@e", "commit", "--quiet", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestClonerBoundedConcurrencyAndIsolation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	dst := t.TempDir()
	var jobs []CloneJob
	for i := 0; i < 8; i++ {
		name := string(rune('a' + i))
		repoDir := filepath.Join(src, name)
		os.MkdirAll(repoDir, 0o755)
		makeRepo(t, repoDir)
		jobs = append(jobs, CloneJob{Repo: domain.Repository{Name: name, Namespace: "ns", FullName: "ns/" + name}, URL: repoDir, Dest: filepath.Join(dst, name)})
	}
	// One failure and one pre-existing destination.
	jobs = append(jobs, CloneJob{Repo: domain.Repository{Name: "missing", FullName: "ns/missing"}, URL: filepath.Join(src, "nope"), Dest: filepath.Join(dst, "missing")})
	os.MkdirAll(filepath.Join(dst, "a"), 0o755)
	os.WriteFile(filepath.Join(dst, "a", "keep"), []byte("x"), 0o644)

	events := make(chan CloneEvent, 64)
	var running, maxRunning int32
	var mu sync.Mutex
	states := map[string]CloneState{}
	done := make(chan struct{})
	go func() {
		for e := range events {
			mu.Lock()
			switch e.State {
			case CloneRunning:
				if n := atomic.AddInt32(&running, 1); n > atomic.LoadInt32(&maxRunning) {
					atomic.StoreInt32(&maxRunning, n)
				}
			case CloneDone, CloneFailed:
				atomic.AddInt32(&running, -1)
			}
			states[e.Job.Repo.FullName] = e.State
			mu.Unlock()
		}
		close(done)
	}()
	c := &Cloner{Git: git.New(), Concurrency: 3, Events: events}
	s := c.Run(context.Background(), jobs)
	close(events)
	<-done
	if s.Cloned != 7 || s.Skipped != 1 || s.Failed != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if maxRunning > 3 {
		t.Fatalf("concurrency bound violated: %d", maxRunning)
	}
	if states["ns/missing"] != CloneFailed || states["ns/a"] != CloneSkipped {
		t.Fatalf("states = %v", states)
	}
	if _, err := os.Stat(filepath.Join(dst, "a", "keep")); err != nil {
		t.Fatal("existing directory was modified")
	}
	// Failure record round trip.
	path := filepath.Join(t.TempDir(), "failures.json")
	if err := SaveFailures(path, jobs, s); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadFailures(path)
	if err != nil || len(rec.Jobs) != 1 || rec.Jobs[0].Repo.FullName != "ns/missing" {
		t.Fatalf("LoadFailures = %+v, %v", rec, err)
	}
	if err := SaveFailures(path, jobs[:1], CloneSummary{Results: []CloneResult{{State: CloneDone}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failure record should be removed when nothing failed")
	}
}

func TestClonerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	jobs := []CloneJob{{Repo: domain.Repository{FullName: "a/b"}, URL: "/nonexistent", Dest: filepath.Join(t.TempDir(), "b")}}
	s := (&Cloner{Git: git.New(), Concurrency: 2}).Run(ctx, jobs)
	if s.Skipped != 1 || s.Cloned != 0 || s.Failed != 0 {
		t.Fatalf("canceled run = %+v", s)
	}
}

func TestRetryableCloneError(t *testing.T) {
	if retryableCloneError(errString("fatal: Authentication failed for 'https://x'")) {
		t.Error("auth failures must not be retried")
	}
	if !retryableCloneError(errString("fatal: early EOF")) {
		t.Error("transient errors should be retried")
	}
	_ = time.Second
}

type errString string

func (e errString) Error() string { return string(e) }
