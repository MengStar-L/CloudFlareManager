package r2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const OrphanFinding = "orphan"

type RemoteObject struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	Metadata     map[string]string
	LastModified time.Time
}

type RemoteObjectList struct {
	Objects           []RemoteObject
	ContinuationToken string
}

type MaintenanceBackend interface {
	Head(context.Context, Target, string) (RemoteObject, error)
	ListRemote(context.Context, Target, string, string, int32) (RemoteObjectList, error)
}

type ScanReport struct {
	Scanned   int   `json:"scanned"`
	Imported  int   `json:"imported"`
	Conflicts int   `json:"conflicts"`
	Orphans   int   `json:"orphans"`
	Bytes     int64 `json:"bytes"`
}

func (s Service) AdoptBucket(ctx context.Context, bucketID string) (ScanReport, error) {
	backend, err := s.maintenanceBackend()
	if err != nil {
		return ScanReport{}, err
	}
	target, err := s.target(ctx, bucketID)
	if err != nil {
		return ScanReport{}, err
	}
	var report ScanReport
	var continuation string
	for {
		page, err := backend.ListRemote(ctx, target, "", continuation, 1000)
		if err != nil {
			return report, err
		}
		for _, listed := range page.Objects {
			report.Scanned++
			report.Bytes += listed.Size
			remote, err := backend.Head(ctx, target, listed.Key)
			if err != nil {
				return report, fmt.Errorf("read metadata for %q: %w", listed.Key, err)
			}
			if _, err := s.Index.AdoptObject(ctx, bucketID, remote); err != nil {
				if errors.Is(err, ErrObjectConflict) {
					report.Conflicts++
					continue
				}
				return report, err
			}
			report.Imported++
		}
		if page.ContinuationToken == "" {
			break
		}
		continuation = page.ContinuationToken
	}
	return report, s.Index.FinishBucketScan(ctx, bucketID, report.Bytes, true)
}

func (s Service) ScanOrphans(ctx context.Context, bucketID string) (ScanReport, error) {
	backend, err := s.maintenanceBackend()
	if err != nil {
		return ScanReport{}, err
	}
	bucket, err := s.Index.GetBucket(ctx, bucketID)
	if err != nil {
		return ScanReport{}, err
	}
	target, err := s.target(ctx, bucketID)
	if err != nil {
		return ScanReport{}, err
	}
	if err := s.Index.ClearScanFindings(ctx, bucketID, OrphanFinding); err != nil {
		return ScanReport{}, err
	}
	var report ScanReport
	var continuation string
	for {
		page, err := backend.ListRemote(ctx, target, "", continuation, 1000)
		if err != nil {
			return report, err
		}
		for _, remote := range page.Objects {
			report.Scanned++
			report.Bytes += remote.Size
			mapped, err := s.Index.HasPhysicalMapping(ctx, bucketID, remote.Key)
			if err != nil {
				return report, err
			}
			if mapped {
				continue
			}
			report.Orphans++
			if err := s.Index.RecordScanFinding(ctx, ScanFinding{
				BucketID: bucketID, Key: remote.Key, Kind: OrphanFinding,
				Detail: fmt.Sprintf("unindexed remote object (%d bytes)", remote.Size),
			}); err != nil {
				return report, err
			}
		}
		if page.ContinuationToken == "" {
			break
		}
		continuation = page.ContinuationToken
	}
	return report, s.Index.FinishBucketScan(ctx, bucketID, report.Bytes, bucket.Adopted)
}

func (s Service) RebuildIndex(ctx context.Context) (ScanReport, error) {
	buckets, err := s.Index.ListBuckets(ctx)
	if err != nil {
		return ScanReport{}, err
	}
	var total ScanReport
	for _, bucket := range buckets {
		report, err := s.AdoptBucket(ctx, bucket.ID)
		total.Scanned += report.Scanned
		total.Imported += report.Imported
		total.Conflicts += report.Conflicts
		total.Bytes += report.Bytes
		if err != nil {
			return total, fmt.Errorf("rebuild bucket %s: %w", bucket.Name, err)
		}
	}
	return total, nil
}

func (s Service) RecoverInterrupted(ctx context.Context) error {
	backend, err := s.maintenanceBackend()
	if err != nil {
		return err
	}
	var after string
	for {
		page, err := s.Index.ListObjectsByStates(ctx, []ObjectState{StatePending, StateDeleting}, after, 500)
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			target, err := s.target(ctx, object.BucketID)
			if err != nil {
				return err
			}
			switch object.State {
			case StatePending:
				remote, err := backend.Head(ctx, target, object.PhysicalKey)
				if err != nil {
					_ = s.Index.FailPut(ctx, object.ObjectID, "interrupted upload was not found upstream")
					continue
				}
				if err := s.Index.CommitPut(ctx, object.ObjectID, remote.ETag, remote.Size); err != nil {
					return err
				}
			case StateDeleting:
				if err := s.Backend.Delete(ctx, target, object.PhysicalKey); err != nil {
					_ = s.Index.FailDelete(ctx, object.Key, err.Error())
					continue
				}
				if err := s.Index.CompleteDelete(ctx, object.Key); err != nil {
					return err
				}
			}
		}
		if page.NextMarker == "" {
			break
		}
		after = page.NextMarker
	}

	initiating, err := s.Index.ListMultipartByStatus(ctx, MultipartInitiating, 1000)
	if err != nil {
		return err
	}
	for _, upload := range initiating {
		_ = s.Index.FailMultipart(ctx, upload.ID)
	}
	completing, err := s.Index.ListMultipartByStatus(ctx, MultipartCompleting, 1000)
	if err != nil {
		return err
	}
	for _, upload := range completing {
		target, err := s.target(ctx, upload.BucketID)
		if err != nil {
			return err
		}
		remote, err := backend.Head(ctx, target, upload.Key)
		if err != nil {
			_ = s.Index.ResetMultipart(ctx, upload.ID)
			continue
		}
		if _, err := s.Index.CommitMultipart(ctx, upload, remote.ETag, remote.Size); err != nil {
			return err
		}
	}
	return nil
}

func (s Service) Rebalance(ctx context.Context, sourceBucketID, targetBucketID, prefix string) (int, error) {
	if sourceBucketID == "" || targetBucketID == "" || sourceBucketID == targetBucketID {
		return 0, errors.New("distinct source and target bucket ids are required")
	}
	targetBucket, err := s.Index.GetBucket(ctx, targetBucketID)
	if err != nil {
		return 0, err
	}
	if !targetBucket.Writable || targetBucket.HealthStatus != "healthy" {
		return 0, errors.New("target bucket is not healthy and writable")
	}
	var moved int
	var after string
	for {
		page, err := s.Index.ListObjectsByBucket(ctx, sourceBucketID, after, 500)
		if err != nil {
			return moved, err
		}
		for _, object := range page.Objects {
			if prefix != "" && !strings.HasPrefix(object.Key, prefix) {
				continue
			}
			if err := s.moveObject(ctx, object, targetBucketID); err != nil {
				return moved, fmt.Errorf("move %q: %w", object.Key, err)
			}
			moved++
		}
		if page.NextMarker == "" {
			return moved, nil
		}
		after = page.NextMarker
	}
}

func (s Service) moveObject(ctx context.Context, object Object, targetBucketID string) error {
	sourceTarget, err := s.target(ctx, object.BucketID)
	if err != nil {
		return err
	}
	target, err := s.target(ctx, targetBucketID)
	if err != nil {
		return err
	}
	result, err := s.Backend.Get(ctx, sourceTarget, object.PhysicalKey, GetOptions{})
	if err != nil {
		return err
	}
	defer result.Body.Close()
	etag, err := s.Backend.Put(ctx, target, object.Key, result.Body, object.Size, object.ContentType, object.Metadata)
	if err != nil {
		return err
	}
	if err := s.Index.MoveObjectMapping(ctx, object.ObjectID, targetBucketID, etag); err != nil {
		_ = s.Backend.Delete(ctx, target, object.Key)
		return err
	}
	if err := s.Backend.Delete(ctx, sourceTarget, object.PhysicalKey); err != nil {
		return fmt.Errorf("mapping moved but source cleanup failed: %w", err)
	}
	return nil
}

func (s Service) maintenanceBackend() (MaintenanceBackend, error) {
	if s.Index == nil || s.Accounts == nil || s.Backend == nil {
		return nil, errors.New("R2 service is not configured")
	}
	backend, ok := s.Backend.(MaintenanceBackend)
	if !ok {
		return nil, errors.New("R2 backend does not support maintenance operations")
	}
	return backend, nil
}
