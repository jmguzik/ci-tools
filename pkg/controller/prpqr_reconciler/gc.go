package prpqr_reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "github.com/openshift/ci-tools/pkg/api/pullrequestpayloadqualification/v1"
)

const (
	conditionAllJobsFinished = "AllJobsFinished"
	gcInterval               = 30 * time.Minute
)

// prpqrGarbageCollector periodically deletes finished PRPQRs older than maxAge.
type prpqrGarbageCollector struct {
	logger    *logrus.Entry
	client    ctrlruntimeclient.Client
	namespace string
	maxAge    time.Duration
	now       func() time.Time
}

func (gc *prpqrGarbageCollector) Start(ctx context.Context) error {
	gc.logger.WithField("max_age", gc.maxAge).WithField("interval", gcInterval).Info("Starting PRPQR garbage collector")
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()

	// Run immediately on startup, then on interval
	gc.collect(ctx)
	for {
		select {
		case <-ctx.Done():
			gc.logger.Info("PRPQR garbage collector stopped")
			return nil
		case <-ticker.C:
			gc.collect(ctx)
		}
	}
}

func (gc *prpqrGarbageCollector) collect(ctx context.Context) {
	logger := gc.logger.WithField("action", "gc-cycle")
	logger.Info("Starting PRPQR garbage collection cycle")

	prpqrs := &v1.PullRequestPayloadQualificationRunList{}
	if err := gc.client.List(ctx, prpqrs, ctrlruntimeclient.InNamespace(gc.namespace)); err != nil {
		logger.WithError(err).Error("Failed to list PRPQRs for garbage collection")
		return
	}

	cutoff := gc.now().Add(-gc.maxAge)
	var deleted, skipped int
	for i := range prpqrs.Items {
		prpqr := &prpqrs.Items[i]

		if prpqr.CreationTimestamp.Time.After(cutoff) {
			continue
		}

		if !isFinished(prpqr) {
			skipped++
			continue
		}

		if err := gc.client.Delete(ctx, prpqr); err != nil {
			logger.WithError(err).WithField("prpqr", prpqr.Name).Error("Failed to delete PRPQR")
			continue
		}
		deleted++
	}

	logger.WithFields(logrus.Fields{
		"total":   len(prpqrs.Items),
		"deleted": deleted,
		"skipped": skipped,
		"cutoff":  cutoff.Format(time.RFC3339),
	}).Info("PRPQR garbage collection cycle complete")
}

func isFinished(prpqr *v1.PullRequestPayloadQualificationRun) bool {
	for _, c := range prpqr.Status.Conditions {
		if c.Type == conditionAllJobsFinished && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// NeedLeaderElection implements manager.LeaderElectionRunnable to ensure
// only one replica runs GC at a time.
func (gc *prpqrGarbageCollector) NeedLeaderElection() bool {
	return true
}

func newGarbageCollector(client ctrlruntimeclient.Client, namespace string, maxAge time.Duration) *prpqrGarbageCollector {
	return &prpqrGarbageCollector{
		logger:    logrus.WithField("component", "prpqr-gc"),
		client:    client,
		namespace: namespace,
		maxAge:    maxAge,
		now:       time.Now,
	}
}

// startGarbageCollector registers the GC runnable with the controller manager.
// maxAge of 0 disables garbage collection.
func startGarbageCollector(mgr manager.Manager, namespace string, maxAge time.Duration) error {
	if maxAge <= 0 {
		logrus.Info("PRPQR garbage collection disabled (max-prpqr-age <= 0)")
		return nil
	}
	gc := newGarbageCollector(mgr.GetClient(), namespace, maxAge)
	if err := mgr.Add(gc); err != nil {
		return fmt.Errorf("failed to add PRPQR garbage collector to manager: %w", err)
	}
	return nil
}
