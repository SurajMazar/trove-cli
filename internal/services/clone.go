// Package services implements multi-step workflows on top of the forge and
// git abstractions: bulk cloning and diagnostics.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/git"
)

// CloneState is the lifecycle of one repository in a bulk clone.
type CloneState string

const (
	CloneQueued  CloneState = "queued"
	CloneRunning CloneState = "cloning"
	CloneDone    CloneState = "cloned"
	CloneSkipped CloneState = "skipped"
	CloneFailed  CloneState = "failed"
)

// CloneJob is one repository to clone.
type CloneJob struct {
	Repo domain.Repository `json:"repository"`
	URL  string            `json:"url"`
	Dest string            `json:"dest"`
	// Helper is the git credential helper for this job's provider ("" for SSH
	// or when git's own helpers should be used).
	Helper string `json:"-"`
	// SSHKey is the private key for SSH clones ("" = git's default).
	SSHKey string `json:"-"`
}

// CloneEvent reports progress.
type CloneEvent struct {
	Index   int
	Job     CloneJob
	State   CloneState
	Attempt int
	Err     error
	Reason  string
}

// CloneResult is the outcome for one job.
type CloneResult struct {
	Repository string     `json:"repository"`
	Provider   string     `json:"provider"`
	Dest       string     `json:"dest"`
	State      CloneState `json:"state"`
	Reason     string     `json:"reason,omitempty"`
	Error      string     `json:"error,omitempty"`
	Attempts   int        `json:"attempts"`
}

// CloneSummary aggregates results.
type CloneSummary struct {
	Cloned  int           `json:"cloned"`
	Skipped int           `json:"skipped"`
	Failed  int           `json:"failed"`
	Results []CloneResult `json:"results"`
}

// FailedResults returns the results that failed.
func (s CloneSummary) FailedResults() []CloneResult {
	var out []CloneResult
	for _, r := range s.Results {
		if r.State == CloneFailed {
			out = append(out, r)
		}
	}
	return out
}

// Cloner runs clone jobs with a bounded worker pool.
type Cloner struct {
	Git         *git.Git
	Concurrency int
	// Retries is the number of extra attempts for failed clones.
	Retries int
	// RetryDelay is the base delay between attempts.
	RetryDelay time.Duration
	// Events, when non-nil, receives progress events. The Cloner never
	// closes it; Run returns after the last event is sent.
	Events chan<- CloneEvent
}

// Run clones every job. Failures do not stop unrelated jobs. When ctx is
// canceled, queued jobs are not started and running clones are terminated.
func (c *Cloner) Run(ctx context.Context, jobs []CloneJob) CloneSummary {
	n := c.Concurrency
	if n < 1 {
		n = 1
	}
	if n > len(jobs) && len(jobs) > 0 {
		n = len(jobs)
	}
	results := make([]CloneResult, len(jobs))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				results[i] = c.one(ctx, i, jobs[i])
			}
		}()
	}
feed:
	for i := range jobs {
		select {
		case <-ctx.Done():
			// Mark the remainder as skipped.
			for j := i; j < len(jobs); j++ {
				results[j] = CloneResult{Repository: jobs[j].Repo.FullName, Provider: jobs[j].Repo.Provider,
					Dest: jobs[j].Dest, State: CloneSkipped, Reason: "canceled"}
				c.emit(CloneEvent{Index: j, Job: jobs[j], State: CloneSkipped, Reason: "canceled"})
			}
			break feed
		case work <- i:
		}
	}
	close(work)
	wg.Wait()

	var s CloneSummary
	s.Results = results
	for _, r := range results {
		switch r.State {
		case CloneDone:
			s.Cloned++
		case CloneSkipped:
			s.Skipped++
		case CloneFailed:
			s.Failed++
		}
	}
	return s
}

func (c *Cloner) emit(e CloneEvent) {
	if c.Events != nil {
		c.Events <- e
	}
}

func (c *Cloner) one(ctx context.Context, i int, job CloneJob) CloneResult {
	res := CloneResult{Repository: job.Repo.FullName, Provider: job.Repo.Provider, Dest: job.Dest}
	if err := ctx.Err(); err != nil {
		res.State, res.Reason = CloneSkipped, "canceled"
		c.emit(CloneEvent{Index: i, Job: job, State: CloneSkipped, Reason: res.Reason})
		return res
	}
	if exists, err := nonEmptyDir(job.Dest); err != nil {
		res.State, res.Error = CloneFailed, err.Error()
		c.emit(CloneEvent{Index: i, Job: job, State: CloneFailed, Err: err})
		return res
	} else if exists {
		res.State, res.Reason = CloneSkipped, "already exists"
		c.emit(CloneEvent{Index: i, Job: job, State: CloneSkipped, Reason: res.Reason})
		return res
	}
	if job.Repo.Empty {
		// Cloning an empty repository works, but the user usually wants to
		// know; still clone it.
		res.Reason = "empty repository"
	}
	g := c.Git
	if job.Helper != "" {
		g = g.WithCredentialHelper(job.Helper)
	}
	if job.SSHKey != "" {
		g = g.WithSSHKey(job.SSHKey)
	}
	var err error
	for attempt := 1; attempt <= c.Retries+1; attempt++ {
		res.Attempts = attempt
		c.emit(CloneEvent{Index: i, Job: job, State: CloneRunning, Attempt: attempt})
		if err = os.MkdirAll(filepath.Dir(job.Dest), 0o755); err != nil {
			break
		}
		err = g.Clone(ctx, job.URL, job.Dest, git.CloneOptions{})
		if err == nil {
			res.State = CloneDone
			c.emit(CloneEvent{Index: i, Job: job, State: CloneDone, Attempt: attempt})
			return res
		}
		// git removes a directory it created on failure, but a killed git
		// (cancellation) may leave a partial clone behind.
		_ = os.RemoveAll(job.Dest)
		if ctx.Err() != nil || attempt > c.Retries || !retryableCloneError(err) {
			break
		}
		delay := c.RetryDelay
		if delay == 0 {
			delay = time.Second
		}
		t := time.NewTimer(delay * time.Duration(attempt))
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	if ctx.Err() != nil {
		res.State, res.Reason = CloneSkipped, "canceled"
		c.emit(CloneEvent{Index: i, Job: job, State: CloneSkipped, Reason: res.Reason})
		return res
	}
	res.State, res.Error = CloneFailed, err.Error()
	c.emit(CloneEvent{Index: i, Job: job, State: CloneFailed, Err: err, Attempt: res.Attempts})
	return res
}

// retryableCloneError is conservative: authentication and not-found errors
// will not fix themselves.
func retryableCloneError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, perm := range []string{"authentication failed", "permission denied", "not found", "could not read username",
		"repository not found", "does not appear to be a git repository", "access denied", "403"} {
		if strings.Contains(msg, perm) {
			return false
		}
	}
	return true
}

func nonEmptyDir(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !st.IsDir() {
		return false, fmt.Errorf("%s exists and is not a directory", p)
	}
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return false, nil // empty directory: safe to clone into
	}
	if err != nil {
		return false, err
	}
	return len(names) > 0, nil
}

// Layout names.
const (
	LayoutFlat      = "flat"
	LayoutNamespace = "namespace"
	LayoutHost      = "host"
)

// Destination computes where a repository is cloned. Path components are
// sanitized so a hostile repository name cannot escape base.
func Destination(base, layout, host string, repo domain.Repository) (string, error) {
	var parts []string
	switch layout {
	case LayoutFlat:
		parts = []string{repo.Name}
	case LayoutHost:
		parts = append([]string{host}, strings.Split(repo.Namespace, "/")...)
		parts = append(parts, repo.Name)
	default:
		parts = append(strings.Split(repo.Namespace, "/"), repo.Name)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `/\`) || strings.ContainsRune(p, 0) {
			return "", errs.New(errs.ErrInvalidArgument, "refusing unsafe clone path component %q", p)
		}
	}
	dest := filepath.Join(append([]string{base}, parts...)...)
	rel, err := filepath.Rel(base, dest)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errs.New(errs.ErrInvalidArgument, "clone destination escapes %s", base)
	}
	return dest, nil
}

// FailureRecord is persisted so `trove repo clone --retry-failed` can retry.
type FailureRecord struct {
	Time time.Time  `json:"time"`
	Jobs []CloneJob `json:"jobs"`
}

// SaveFailures records failed jobs to path (or removes the file when none).
func SaveFailures(path string, jobs []CloneJob, summary CloneSummary) error {
	var failed []CloneJob
	for i, r := range summary.Results {
		if r.State == CloneFailed && i < len(jobs) {
			failed = append(failed, jobs[i])
		}
	}
	if len(failed) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(FailureRecord{Time: time.Now(), Jobs: failed}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadFailures reads the last failure record.
func LoadFailures(path string) (*FailureRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errs.New(errs.ErrNotFound, "no failed clones recorded")
		}
		return nil, err
	}
	var r FailureRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
