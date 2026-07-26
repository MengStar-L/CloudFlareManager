package r2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
)

const (
	AdoptBucketJobType  = "r2.bucket.adopt"
	OrphanScanJobType   = "r2.bucket.orphans.scan"
	RebuildIndexJobType = "r2.index.rebuild"
	RecoverStateJobType = "r2.state.recover"
	RebalanceJobType    = "r2.objects.rebalance"
)

type MaintenanceJobs struct {
	Service Service
	Jobs    *jobs.Store
}

func (h MaintenanceJobs) HandleAdopt(ctx context.Context, job jobs.Job) error {
	bucketID, err := bucketIDFromJob(job)
	if err != nil {
		return err
	}
	if _, err := h.Service.AdoptBucket(ctx, bucketID); err != nil {
		return err
	}
	return h.progress(ctx, job.ID, .95)
}

func (h MaintenanceJobs) HandleOrphanScan(ctx context.Context, job jobs.Job) error {
	bucketID, err := bucketIDFromJob(job)
	if err != nil {
		return err
	}
	if _, err := h.Service.ScanOrphans(ctx, bucketID); err != nil {
		return err
	}
	return h.progress(ctx, job.ID, .95)
}

func (h MaintenanceJobs) HandleRebuild(ctx context.Context, job jobs.Job) error {
	if _, err := h.Service.RebuildIndex(ctx); err != nil {
		return err
	}
	return h.progress(ctx, job.ID, .95)
}

func (h MaintenanceJobs) HandleRecover(ctx context.Context, job jobs.Job) error {
	if err := h.Service.RecoverInterrupted(ctx); err != nil {
		return err
	}
	return h.progress(ctx, job.ID, .95)
}

func (h MaintenanceJobs) HandleRebalance(ctx context.Context, job jobs.Job) error {
	var payload struct {
		SourceBucketID string `json:"source_bucket_id"`
		TargetBucketID string `json:"target_bucket_id"`
		Prefix         string `json:"prefix"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode rebalance job: %w", err)
	}
	if payload.SourceBucketID == "" || payload.TargetBucketID == "" {
		return errors.New("source_bucket_id and target_bucket_id are required")
	}
	if _, err := h.Service.Rebalance(ctx, payload.SourceBucketID, payload.TargetBucketID, payload.Prefix); err != nil {
		return err
	}
	return h.progress(ctx, job.ID, .95)
}

func (h MaintenanceJobs) progress(ctx context.Context, jobID string, value float64) error {
	if h.Jobs == nil {
		return nil
	}
	return h.Jobs.SetProgress(ctx, jobID, value)
}

func bucketIDFromJob(job jobs.Job) (string, error) {
	var payload struct {
		BucketID string `json:"bucket_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return "", fmt.Errorf("decode bucket job: %w", err)
	}
	if payload.BucketID == "" {
		return "", errors.New("bucket_id is required")
	}
	return payload.BucketID, nil
}
