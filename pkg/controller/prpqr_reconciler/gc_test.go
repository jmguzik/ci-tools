package prpqr_reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/openshift/ci-tools/pkg/api/pullrequestpayloadqualification/v1"
)

func TestCollect(t *testing.T) {
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	twoDaysAgo := metav1.NewTime(now.Add(-48 * time.Hour))
	tenDaysAgo := metav1.NewTime(now.Add(-240 * time.Hour))
	oneHourAgo := metav1.NewTime(now.Add(-1 * time.Hour))

	finishedCondition := metav1.Condition{
		Type:               conditionAllJobsFinished,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: twoDaysAgo,
		Reason:             conditionAllJobsFinished,
		Message:            "All jobs have finished.",
	}
	runningCondition := metav1.Condition{
		Type:               conditionAllJobsFinished,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: twoDaysAgo,
		Reason:             conditionAllJobsFinished,
		Message:            "jobs [some-job] still running",
	}

	tests := []struct {
		name        string
		maxAge      time.Duration
		prpqrs      []ctrlruntimeclient.Object
		wantDeleted []string
		wantKept    []string
	}{
		{
			name:   "deletes old finished PRPQRs",
			maxAge: 24 * time.Hour,
			prpqrs: []ctrlruntimeclient.Object{
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "old-finished",
						Namespace:         "ci",
						CreationTimestamp: tenDaysAgo,
					},
					Status: v1.PullRequestPayloadTestStatus{
						Conditions: []metav1.Condition{finishedCondition},
					},
				},
			},
			wantDeleted: []string{"old-finished"},
		},
		{
			name:   "keeps young finished PRPQRs",
			maxAge: 24 * time.Hour,
			prpqrs: []ctrlruntimeclient.Object{
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "young-finished",
						Namespace:         "ci",
						CreationTimestamp: oneHourAgo,
					},
					Status: v1.PullRequestPayloadTestStatus{
						Conditions: []metav1.Condition{finishedCondition},
					},
				},
			},
			wantKept: []string{"young-finished"},
		},
		{
			name:   "keeps old still-running PRPQRs",
			maxAge: 24 * time.Hour,
			prpqrs: []ctrlruntimeclient.Object{
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "old-running",
						Namespace:         "ci",
						CreationTimestamp: tenDaysAgo,
					},
					Status: v1.PullRequestPayloadTestStatus{
						Conditions: []metav1.Condition{runningCondition},
					},
				},
			},
			wantKept: []string{"old-running"},
		},
		{
			name:   "keeps old PRPQRs with no conditions",
			maxAge: 24 * time.Hour,
			prpqrs: []ctrlruntimeclient.Object{
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "old-no-conditions",
						Namespace:         "ci",
						CreationTimestamp: tenDaysAgo,
					},
				},
			},
			wantKept: []string{"old-no-conditions"},
		},
		{
			name:   "mixed scenario",
			maxAge: 24 * time.Hour,
			prpqrs: []ctrlruntimeclient.Object{
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "old-finished-1",
						Namespace:         "ci",
						CreationTimestamp: tenDaysAgo,
					},
					Status: v1.PullRequestPayloadTestStatus{
						Conditions: []metav1.Condition{finishedCondition},
					},
				},
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "old-finished-2",
						Namespace:         "ci",
						CreationTimestamp: twoDaysAgo,
					},
					Status: v1.PullRequestPayloadTestStatus{
						Conditions: []metav1.Condition{finishedCondition},
					},
				},
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "young-finished",
						Namespace:         "ci",
						CreationTimestamp: oneHourAgo,
					},
					Status: v1.PullRequestPayloadTestStatus{
						Conditions: []metav1.Condition{finishedCondition},
					},
				},
				&v1.PullRequestPayloadQualificationRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "old-running",
						Namespace:         "ci",
						CreationTimestamp: tenDaysAgo,
					},
					Status: v1.PullRequestPayloadTestStatus{
						Conditions: []metav1.Condition{runningCondition},
					},
				},
			},
			wantDeleted: []string{"old-finished-1", "old-finished-2"},
			wantKept:    []string{"young-finished", "old-running"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakectrlruntimeclient.NewClientBuilder().
				WithObjects(tt.prpqrs...).
				Build()

			gc := &prpqrGarbageCollector{
				logger:    logrus.WithField("test", tt.name),
				client:    client,
				namespace: "ci",
				maxAge:    tt.maxAge,
				now:       func() time.Time { return now },
			}

			gc.collect(context.Background())

			for _, name := range tt.wantDeleted {
				prpqr := &v1.PullRequestPayloadQualificationRun{}
				err := client.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: "ci", Name: name}, prpqr)
				if err == nil {
					t.Errorf("expected PRPQR %s to be deleted, but it still exists", name)
				}
			}

			for _, name := range tt.wantKept {
				prpqr := &v1.PullRequestPayloadQualificationRun{}
				err := client.Get(context.Background(), ctrlruntimeclient.ObjectKey{Namespace: "ci", Name: name}, prpqr)
				if err != nil {
					t.Errorf("expected PRPQR %s to be kept, but got error: %v", name, err)
				}
			}
		})
	}
}

func TestIsFinished(t *testing.T) {
	tests := []struct {
		name     string
		prpqr    *v1.PullRequestPayloadQualificationRun
		expected bool
	}{
		{
			name: "finished",
			prpqr: &v1.PullRequestPayloadQualificationRun{
				Status: v1.PullRequestPayloadTestStatus{
					Conditions: []metav1.Condition{
						{Type: "AllJobsTriggered", Status: metav1.ConditionTrue},
						{Type: conditionAllJobsFinished, Status: metav1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
		{
			name: "not finished - still running",
			prpqr: &v1.PullRequestPayloadQualificationRun{
				Status: v1.PullRequestPayloadTestStatus{
					Conditions: []metav1.Condition{
						{Type: conditionAllJobsFinished, Status: metav1.ConditionFalse},
					},
				},
			},
			expected: false,
		},
		{
			name: "not finished - no conditions",
			prpqr: &v1.PullRequestPayloadQualificationRun{
				Status: v1.PullRequestPayloadTestStatus{},
			},
			expected: false,
		},
		{
			name:     "not finished - empty status",
			prpqr:    &v1.PullRequestPayloadQualificationRun{},
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFinished(tt.prpqr); got != tt.expected {
				t.Errorf("isFinished() = %v, want %v", got, tt.expected)
			}
		})
	}
}
