package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/git/localgit"
	"sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/github"
)

// testRepoClient wraps a real git.RepoClient and overrides FetchRef to work
// with local test repos that don't have GitHub pull request refs. The real code
// fetches "pull/{number}/head"; in tests we fetch the PR branch directly so
// that FETCH_HEAD points to the PR tip.
type testRepoClient struct {
	git.RepoClient
	prBranch         string
	expectedFetchRef string
}

func (t *testRepoClient) FetchRef(ref string) error {
	if t.expectedFetchRef != "" && ref != t.expectedFetchRef {
		return fmt.Errorf("unexpected fetch ref: got %q, want %q", ref, t.expectedFetchRef)
	}
	return t.RepoClient.FetchRef(t.prBranch)
}

type fakeGHC struct {
	refSHA      string
	expectedRef string
}

func (f *fakeGHC) CreateComment(string, string, int, string) error                 { return nil }
func (f *fakeGHC) AddLabel(string, string, int, string) error                      { return nil }
func (f *fakeGHC) RemoveLabel(string, string, int, string) error                   { return nil }
func (f *fakeGHC) GetPullRequest(string, string, int) (*github.PullRequest, error) { return nil, nil }
func (f *fakeGHC) GetRef(_, _, ref string) (string, error) {
	if f.expectedRef != "" && ref != f.expectedRef {
		return "", fmt.Errorf("unexpected ref lookup: got %q, want %q", ref, f.expectedRef)
	}
	return f.refSHA, nil
}
func (f *fakeGHC) ListIssueComments(string, string, int) ([]github.IssueComment, error) {
	return nil, nil
}
func (f *fakeGHC) DeleteComment(string, string, int) error { return nil }
func (f *fakeGHC) IsMember(string, string) (bool, error)   { return false, nil }

func testPullRequest() *github.PullRequest {
	return &github.PullRequest{
		Number: 123,
		Base: github.PullRequestBranch{
			Ref: "main",
			Repo: github.Repo{
				Owner: github.User{Login: "org"},
				Name:  "repo",
			},
		},
		Head: github.PullRequestBranch{
			SHA: "pr-head-sha",
			Ref: "feature-branch",
		},
		User:  github.User{Login: "author"},
		Title: "Test PR",
	}
}

// TestPrepareCandidateRebaseDirection verifies that prepareCandidate rebases PR
// commits onto the base branch, not the other way around.
//
// With the inverted rebase (main onto PR), main's commit history is replayed
// onto the PR tip. When main contains intermediate states that conflict with
// the PR (e.g., a file deleted then re-created), this causes spurious rebase
// conflicts even though the final states are compatible. A correctly-directed
// rebase (PR onto main) replays PR commits onto the main tip and avoids these
// intermediate-state conflicts.
func TestPrepareCandidateRebaseDirection(t *testing.T) {
	lg, clients, err := localgit.NewV2()
	if err != nil {
		t.Fatalf("failed to create localgit: %v", err)
	}
	lg.InitialBranch = "main"
	defer func() {
		if err := lg.Clean(); err != nil {
			t.Errorf("localgit cleanup failed: %v", err)
		}
	}()
	defer func() {
		if err := clients.Clean(); err != nil {
			t.Errorf("client factory cleanup failed: %v", err)
		}
	}()

	if err := lg.MakeFakeRepo("org", "repo"); err != nil {
		t.Fatalf("failed to make fake repo: %v", err)
	}

	// Set up main branch with a base file
	if err := lg.AddCommit("org", "repo", map[string][]byte{"base.txt": []byte("base")}); err != nil {
		t.Fatalf("failed to add base commit: %v", err)
	}

	// Branch off for the PR and add a PR-only file
	if err := lg.CheckoutNewBranch("org", "repo", "pr-branch"); err != nil {
		t.Fatalf("failed to create PR branch: %v", err)
	}
	if err := lg.AddCommit("org", "repo", map[string][]byte{"pr.txt": []byte("pr-change")}); err != nil {
		t.Fatalf("failed to add PR commit: %v", err)
	}

	// Go back to main and add a main-only file (creating divergence)
	if err := lg.Checkout("org", "repo", "main"); err != nil {
		t.Fatalf("failed to checkout main: %v", err)
	}
	if err := lg.AddCommit("org", "repo", map[string][]byte{"main-only.txt": []byte("main-change")}); err != nil {
		t.Fatalf("failed to add main commit: %v", err)
	}

	// Record main SHA before prepareCandidate runs
	mainSHABefore, err := lg.RevParse("org", "repo", "main")
	if err != nil {
		t.Fatalf("failed to rev-parse main: %v", err)
	}
	mainSHABefore = strings.TrimSpace(mainSHABefore)

	// Get a real git client (clones the repo)
	repoClient, err := clients.ClientFor("org", "repo")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer func() {
		if err := repoClient.Clean(); err != nil {
			t.Errorf("repoClient cleanup failed: %v", err)
		}
	}()

	wrapped := &testRepoClient{RepoClient: repoClient, prBranch: "pr-branch", expectedFetchRef: "pull/123/head"}
	s := &server{ghc: &fakeGHC{refSHA: mainSHABefore, expectedRef: "heads/main"}}
	logger := logrus.NewEntry(logrus.StandardLogger())

	if _, err := s.prepareCandidate(wrapped, testPullRequest(), logger); err != nil {
		t.Fatalf("prepareCandidate failed: %v", err)
	}

	dir := repoClient.Directory()

	// Both PR and main files must exist in the working tree
	for _, f := range []string{"pr.txt", "main-only.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist in working tree after rebase: %v", f, err)
		}
	}

	// The main ref in the clone must not have been modified by the rebase.
	// With an inverted rebase, git rewrites the main branch itself (HEAD ends
	// up ON main). With the correct rebase, main is untouched and HEAD is
	// ahead of it with the PR commits on top.
	mainSHAAfter, err := repoClient.RevParse("main")
	if err != nil {
		t.Fatalf("failed to rev-parse main after rebase: %v", err)
	}
	mainSHAAfter = strings.TrimSpace(mainSHAAfter)
	if mainSHAAfter != mainSHABefore {
		t.Fatalf("expected main ref to stay unchanged, before=%s after=%s", mainSHABefore, mainSHAAfter)
	}
	headSHA, err := repoClient.RevParse("HEAD")
	if err != nil {
		t.Fatalf("failed to rev-parse HEAD after rebase: %v", err)
	}
	headSHA = strings.TrimSpace(headSHA)
	if mainSHAAfter == headSHA {
		t.Error("HEAD should be ahead of main after rebasing PR onto main, but they point to the same commit (rebase direction is likely inverted)")
	}

	// Diff between main and HEAD should show exactly the PR file
	changes, err := repoClient.Diff("main", "HEAD")
	if err != nil {
		t.Fatalf("failed to diff: %v", err)
	}
	if len(changes) != 1 || changes[0] != "pr.txt" {
		t.Errorf("expected diff main..HEAD to show only [pr.txt], got %v", changes)
	}
}

func TestHasPathPrefix(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		prefix string
		want   bool
	}{
		{name: "exact match", path: "ci-operator/config", prefix: "ci-operator/config", want: true},
		{name: "child path", path: "ci-operator/config/org/repo.yaml", prefix: "ci-operator/config", want: true},
		{name: "partial name match should not match", path: "ci-operator/configs/foo", prefix: "ci-operator/config", want: false},
		{name: "completely different path", path: "docs/readme.md", prefix: "ci-operator/config", want: false},
		{name: "prefix is longer than path", path: "ci-operator", prefix: "ci-operator/config", want: false},
		{name: "empty path", path: "", prefix: "ci-operator/config", want: false},
		{name: "empty prefix matches exact empty", path: "", prefix: "", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hasPathPrefix(tc.path, tc.prefix)
			if got != tc.want {
				t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", tc.path, tc.prefix, got, tc.want)
			}
		})
	}
}

func TestIsRehearsalRelevantPath(t *testing.T) {
	tests := []struct {
		name                   string
		path                   string
		includeRegistryChanges bool
		want                   bool
	}{
		{name: "ci-operator config", path: "ci-operator/config/org/repo.yaml", includeRegistryChanges: false, want: true},
		{name: "ci-operator jobs", path: "ci-operator/jobs/org/repo/job.yaml", includeRegistryChanges: false, want: true},
		{name: "prow config dir", path: "core-services/prow/02_config/something.yaml", includeRegistryChanges: false, want: true},
		{name: "registry with flag on", path: "ci-operator/step-registry/ipi/install/install.yaml", includeRegistryChanges: true, want: true},
		{name: "registry with flag off", path: "ci-operator/step-registry/ipi/install/install.yaml", includeRegistryChanges: false, want: false},
		{name: "unrelated file", path: "hack/build.sh", includeRegistryChanges: true, want: false},
		{name: "README at root", path: "README.md", includeRegistryChanges: true, want: false},
		{name: "ci-operator root file", path: "ci-operator/OWNERS", includeRegistryChanges: false, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isRehearsalRelevantPath(tc.path, tc.includeRegistryChanges)
			if got != tc.want {
				t.Errorf("isRehearsalRelevantPath(%q, %v) = %v, want %v", tc.path, tc.includeRegistryChanges, got, tc.want)
			}
		})
	}
}

func TestDispatcherImmediateExecution(t *testing.T) {
	d := newHandlerDispatcher(2, 5, time.Minute, 5*time.Second)
	logger := logrus.NewEntry(logrus.StandardLogger())

	executed := make(chan struct{})
	d.dispatch(logger, func() {
		close(executed)
	}, nil)

	select {
	case <-executed:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not executed within timeout")
	}
}

func TestDispatcherQueuesThenExecutes(t *testing.T) {
	d := newHandlerDispatcher(1, 5, 10*time.Second, 30*time.Second)
	logger := logrus.NewEntry(logrus.StandardLogger())

	blocker := make(chan struct{})
	firstStarted := make(chan struct{})

	go d.dispatch(logger, func() {
		close(firstStarted)
		<-blocker
	}, nil)

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first handler did not start")
	}

	secondDone := make(chan struct{})
	go d.dispatch(logger, func() {
		close(secondDone)
	}, nil)

	select {
	case <-secondDone:
		t.Fatal("second handler should not have run while first is blocking")
	case <-time.After(200 * time.Millisecond):
	}

	close(blocker)
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second handler was not executed after first completed")
	}
}

func TestDispatcherDropsWhenQueueFull(t *testing.T) {
	d := newHandlerDispatcher(1, 1, 10*time.Second, 30*time.Second)
	logger := logrus.NewEntry(logrus.StandardLogger())

	blocker := make(chan struct{})
	firstStarted := make(chan struct{})
	go d.dispatch(logger, func() {
		close(firstStarted)
		<-blocker
	}, nil)

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first handler did not start")
	}

	// Fill the single queue slot; this dispatch will block in the select
	// waiting for an execution slot, which holds the queue slot occupied.
	secondStarted := make(chan struct{})
	go d.dispatch(logger, func() {
		close(secondStarted)
	}, nil)

	// We can't directly observe the second handler entering the queue, but
	// we know it can't start (slot is full) and can't be dropped (queue has
	// room). Give the scheduler a moment to let it enter the select.
	time.Sleep(50 * time.Millisecond)

	dropped := make(chan string, 1)
	d.dispatch(logger, func() {}, func(reason string) {
		dropped <- reason
	})

	select {
	case reason := <-dropped:
		if reason != "queue_full" {
			t.Errorf("expected reason 'queue_full', got %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("third handler was not dropped")
	}

	close(blocker)

	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("second handler did not start after blocker released")
	}
}

func TestDispatcherQueueTimeout(t *testing.T) {
	d := newHandlerDispatcher(1, 5, 200*time.Millisecond, 30*time.Second)
	logger := logrus.NewEntry(logrus.StandardLogger())

	blocker := make(chan struct{})
	firstStarted := make(chan struct{})
	go d.dispatch(logger, func() {
		close(firstStarted)
		<-blocker
	}, nil)

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first handler did not start")
	}

	dropped := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.dispatch(logger, func() {
			t.Error("handler should not have run after queue timeout")
		}, func(reason string) {
			dropped <- reason
		})
	}()

	select {
	case reason := <-dropped:
		if reason != "queue_timeout" {
			t.Errorf("expected reason 'queue_timeout', got %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not dropped after queue timeout")
	}

	close(blocker)
	wg.Wait()
}

type commentRecorder struct {
	fakeGHC
	mu       sync.Mutex
	comments []string
}

func (c *commentRecorder) CreateComment(_, _ string, _ int, comment string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comments = append(c.comments, comment)
	return nil
}

func (c *commentRecorder) getComments() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.comments))
	copy(out, c.comments)
	return out
}

func TestNotifyDroppedRequestUserTriggered(t *testing.T) {
	ghc := &commentRecorder{}
	logger := logrus.NewEntry(logrus.StandardLogger())

	notifyDroppedRequest(ghc, "org", "repo", 1, "alice", "queue_full", 5, true, logger)

	comments := ghc.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if !strings.Contains(comments[0], "@alice") {
		t.Errorf("expected comment to mention user, got: %s", comments[0])
	}
	if !strings.Contains(comments[0], "/pj-rehearse") {
		t.Errorf("expected comment to mention /pj-rehearse command for user-triggered, got: %s", comments[0])
	}
}

func TestNotifyDroppedRequestAutomatic(t *testing.T) {
	ghc := &commentRecorder{}
	logger := logrus.NewEntry(logrus.StandardLogger())

	notifyDroppedRequest(ghc, "org", "repo", 1, "alice", "queue_timeout", 5, false, logger)

	comments := ghc.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if !strings.Contains(comments[0], "could not automatically process") {
		t.Errorf("expected automatic event message, got: %s", comments[0])
	}
	if !strings.Contains(comments[0], "5 minutes") {
		t.Errorf("expected timeout duration in message, got: %s", comments[0])
	}
}
