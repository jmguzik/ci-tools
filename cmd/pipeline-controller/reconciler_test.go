package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
	"sigs.k8s.io/prow/pkg/kube"
)

// testLoggerReconciler creates a discarded logger for tests
func testLoggerReconciler() *logrus.Entry {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return logrus.NewEntry(logger)
}

type fakeGhClient struct {
	closed sets.Int
}

func (c fakeGhClient) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	if c.closed.Has(number) {
		return &github.PullRequest{State: github.PullRequestStateClosed}, nil
	}
	return &github.PullRequest{State: github.PullRequestStateOpen}, nil

}

func (c fakeGhClient) CreateComment(owner, repo string, number int, comment string) error {
	return nil
}

func (c fakeGhClient) GetPullRequestChanges(org string, repo string, number int) ([]github.PullRequestChange, error) {
	return []github.PullRequestChange{}, nil
}

func (c fakeGhClient) CreateStatus(org, repo, ref string, s github.Status) error {
	return nil
}

type FakeReader struct {
	pjs v1.ProwJobList
}

func (tr FakeReader) Get(ctx context.Context, n ctrlruntimeclient.ObjectKey, o ctrlruntimeclient.Object, opts ...ctrlruntimeclient.GetOption) error {
	return nil
}

func (tr FakeReader) List(ctx context.Context, list ctrlruntimeclient.ObjectList, opts ...ctrlruntimeclient.ListOption) error {
	pjList, ok := list.(*v1.ProwJobList)
	if !ok {
		return errors.New("conversion to pj list error")
	}
	pjList.Items = tr.pjs.Items
	return nil
}

type fakeGhClientWithTracking struct {
	closed      sets.Int
	commentSent bool
}

func (c *fakeGhClientWithTracking) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	if c.closed.Has(number) {
		return &github.PullRequest{State: github.PullRequestStateClosed}, nil
	}
	return &github.PullRequest{State: github.PullRequestStateOpen}, nil
}

func (c *fakeGhClientWithTracking) CreateComment(owner, repo string, number int, comment string) error {
	c.commentSent = true
	return nil
}

func (c *fakeGhClientWithTracking) GetPullRequestChanges(org string, repo string, number int) ([]github.PullRequestChange, error) {
	return []github.PullRequestChange{}, nil
}

func (c *fakeGhClientWithTracking) CreateStatus(org, repo, ref string, s github.Status) error {
	return nil
}

func composePresubmit(name string, state v1.ProwJobState, sha string) v1.ProwJob {
	timeNow := time.Now().Truncate(time.Hour)
	pj := v1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				kube.ProwJobTypeLabel: "presubmit",
				kube.OrgLabel:         "org",
				kube.RepoLabel:        "repo",
				kube.PullLabel:        "123",
			},
			CreationTimestamp: metav1.Time{
				Time: timeNow.Add(-3 * time.Hour),
			},
			ResourceVersion: "999",
		},
		Status: v1.ProwJobStatus{
			State: state,
		},
		Spec: v1.ProwJobSpec{
			Type: v1.PresubmitJob,
			Refs: &v1.Refs{
				BaseRef: "master",
				Repo:    "repo",
				Pulls: []v1.Pull{
					{
						Number: 123,
						SHA:    sha,
					},
				},
			},
			Job:    name,
			Report: true,
		},
	}
	if state == v1.SuccessState || state == v1.FailureState || state == v1.AbortedState {
		pj.Status.CompletionTime = &metav1.Time{Time: timeNow.Add(-2 * time.Hour)}
	}
	return pj
}

func Test_reconciler_reportSuccessOnPR(t *testing.T) {
	var objs []runtime.Object
	fakeClient := fakectrlruntimeclient.NewClientBuilder().WithRuntimeObjects(objs...).Build()
	baseSha := "sha"
	dummyPJ := composePresubmit("org-repo-master-ps1", v1.SuccessState, baseSha)
	defaultGhClient := fakeGhClient{closed: sets.NewInt()}

	type fields struct {
		lister FakeReader
		ghc    minimalGhClient
	}
	type args struct {
		ctx        context.Context
		presubmits presubmitTests
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "all tests are required and passed successfully, trigger protected",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps2", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.SuccessState, baseSha),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}, {JobBase: config.JobBase{Name: "org-repo-other-branch-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    true,
			wantErr: false,
		},
		{
			name: "all tests are required and passed successfully, do not trigger protected as PR is closed",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps2", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.SuccessState, baseSha),
				}}},
				ghc: fakeGhClient{closed: sets.NewInt([]int{123}...)},
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "all required complete but conditionally required failed",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps2", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.FailureState, baseSha),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "all required complete only some of cond required executed",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps2", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.SuccessState, baseSha),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}, {JobBase: config.JobBase{Name: "org-repo-master-ps4"}}, {JobBase: config.JobBase{Name: "org-repo-master-ps5"}}},
				},
			},
			want:    true,
			wantErr: false,
		},
		{
			name: "all required complete but always required is aborted",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps2", v1.AbortedState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.SuccessState, baseSha),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "do not trigger as user is manually triggering",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps1", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps2", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.SuccessState, baseSha),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}, {JobBase: config.JobBase{Name: "org-repo-master-ps4"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "do not trigger as required are not complete",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps2", v1.PendingState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.TriggeredState, baseSha),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "only protected tests exist",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{}}},
				ghc:    defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					alwaysRequired:        []config.Presubmit{},
					conditionallyRequired: []config.Presubmit{},
				},
			},
			want:    true,
			wantErr: false,
		},
		{
			name: "batch with one sha is analyzed but different sha has already passed",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps2", v1.SuccessState, "other-sha"),
					composePresubmit("org-repo-master-ps3", v1.SuccessState, "other-sha"),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "do not trigger as user is manually triggering",
			fields: fields{
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					composePresubmit("org-repo-master-ps1", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps2", v1.SuccessState, baseSha),
					composePresubmit("org-repo-master-ps3", v1.SuccessState, baseSha),
				}}},
				ghc: defaultGhClient,
			},
			args: args{
				ctx: context.Background(),
				presubmits: presubmitTests{
					protected:             []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps1"}}, {JobBase: config.JobBase{Name: "org-repo-master-ps4"}}},
					alwaysRequired:        []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					conditionallyRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps3"}}},
				},
			},
			want:    false,
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &reconciler{
				pjclientset: fakeClient,
				lister:      tc.fields.lister,
				configDataProvider: &ConfigDataProvider{
					previousRepoList: []string{},
					logger:           testLoggerReconciler(),
				},
				ghc:            tc.fields.ghc,
				ids:            sync.Map{},
				closedPRsCache: closedPRsCache{prs: map[string]pullRequest{}, m: sync.Mutex{}, ghc: tc.fields.ghc, clearTime: time.Now()},
			}
			got, err := r.reportSuccessOnPR(tc.args.ctx, &dummyPJ, tc.args.presubmits)
			if (err != nil) != tc.wantErr {
				t.Errorf("reconciler.reportSuccessOnPR() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if got != tc.want {
				t.Errorf("reconciler.reportSuccessOnPR() = %v, want %v", got, tc.want)
			}
		})
	}
}

func Test_reconciler_reconcile_with_modes(t *testing.T) {
	baseSha := "sha"
	// Create a ProwJob with all required fields
	dummyPJ := v1.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "org-repo-master-ps1",
			Namespace: "test-namespace",
			Labels: map[string]string{
				kube.ProwJobTypeLabel: "presubmit",
				kube.OrgLabel:         "org",
				kube.RepoLabel:        "repo",
				kube.PullLabel:        "123",
				kube.BaseRefLabel:     "master",
			},
			CreationTimestamp: metav1.Time{
				Time: time.Now(),
			},
			ResourceVersion: "999",
		},
		Status: v1.ProwJobStatus{
			State: v1.SuccessState,
		},
		Spec: v1.ProwJobSpec{
			Type: v1.PresubmitJob,
			Refs: &v1.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "master",
				Pulls: []v1.Pull{
					{
						Number: 123,
						SHA:    baseSha,
					},
				},
			},
			Job:    "org-repo-master-ps1",
			Report: true,
		},
	}

	type fields struct {
		watcherConfig     enabledConfig
		presubmits        map[string]presubmitTests
		expectSendComment bool
	}
	tests := []struct {
		name   string
		fields fields
	}{
		{
			name: "auto mode: should send comment",
			fields: fields{
				watcherConfig: enabledConfig{Orgs: []struct {
					Org   string     `yaml:"org"`
					Repos []RepoItem `yaml:"repos"`
				}{
					{
						Org: "org",
						Repos: []RepoItem{
							{
								Name: "repo",
								Mode: struct {
									Trigger string
								}{
									Trigger: "auto",
								},
							},
						},
					},
				}},
				presubmits: map[string]presubmitTests{
					"org/repo": {
						protected:      []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps-protected"}}},
						alwaysRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					},
				},
				expectSendComment: true,
			},
		},
		{
			name: "manual mode: should not send comment",
			fields: fields{
				watcherConfig: enabledConfig{Orgs: []struct {
					Org   string     `yaml:"org"`
					Repos []RepoItem `yaml:"repos"`
				}{
					{
						Org: "org",
						Repos: []RepoItem{
							{
								Name: "repo",
								Mode: struct {
									Trigger string
								}{
									Trigger: "manual",
								},
							},
						},
					},
				}},
				presubmits: map[string]presubmitTests{
					"org/repo": {
						protected:      []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps-protected"}}},
						alwaysRequired: []config.Presubmit{{JobBase: config.JobBase{Name: "org-repo-master-ps2"}}},
					},
				},
				expectSendComment: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ghc := &fakeGhClientWithTracking{closed: sets.NewInt()}

			// Create a successful always-required job in the lister
			successfulJob := v1.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "org-repo-master-ps2",
					Namespace: "test-namespace",
					Labels: map[string]string{
						kube.ProwJobTypeLabel: "presubmit",
						kube.OrgLabel:         "org",
						kube.RepoLabel:        "repo",
						kube.PullLabel:        "123",
						kube.BaseRefLabel:     "master",
					},
					CreationTimestamp: metav1.Time{
						Time: time.Now(),
					},
				},
				Status: v1.ProwJobStatus{
					State: v1.SuccessState,
				},
				Spec: v1.ProwJobSpec{
					Type: v1.PresubmitJob,
					Job:  "org-repo-master-ps2",
					Refs: &v1.Refs{
						Org:     "org",
						Repo:    "repo",
						BaseRef: "master",
						Pulls: []v1.Pull{
							{
								Number: 123,
								SHA:    baseSha,
							},
						},
					},
				},
			}

			r := &reconciler{
				pjclientset: fakectrlruntimeclient.NewClientBuilder().WithRuntimeObjects(&dummyPJ).Build(),
				lister: FakeReader{pjs: v1.ProwJobList{Items: []v1.ProwJob{
					successfulJob,
				}}},
				configDataProvider: &ConfigDataProvider{
					updatedPresubmits: tc.fields.presubmits,
					previousRepoList:  []string{},
					logger:            testLoggerReconciler(),
				},
				ghc:     ghc,
				ids:     sync.Map{},
				watcher: &watcher{config: tc.fields.watcherConfig},
				closedPRsCache: closedPRsCache{
					prs:       map[string]pullRequest{},
					m:         sync.Mutex{},
					ghc:       ghc,
					clearTime: time.Now(),
				},
			}

			ctx := context.Background()
			err := r.reconcile(ctx, reconcile.Request{
				NamespacedName: ctrlruntimeclient.ObjectKey{
					Namespace: dummyPJ.Namespace,
					Name:      dummyPJ.Name,
				},
			})

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if ghc.commentSent != tc.fields.expectSendComment {
				t.Errorf("expected comment sent = %v, got %v", tc.fields.expectSendComment, ghc.commentSent)
			}
		})
	}
}
