package r2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
)

const (
	FileMoveJobType   = "r2.files.move"
	FileDeleteJobType = "r2.files.delete"
)

type FileJobPayload struct {
	Source      string `json:"source"`
	Destination string `json:"destination,omitempty"`
	Overwrite   bool   `json:"overwrite,omitempty"`
}

type FileJobs struct {
	Service Service
	Jobs    *jobs.Store
}

func (h FileJobs) HandleMove(ctx context.Context, job jobs.Job) error {
	payload, err := decodeFileJob(job)
	if err != nil {
		return err
	}
	overwrite := payload.Overwrite || job.Attempts > 1
	if err := h.Service.ValidateDirectoryMove(ctx, payload.Source, payload.Destination, overwrite); err != nil {
		if job.Attempts > 1 && errors.Is(err, ErrObjectNotFound) {
			if _, destinationErr := h.Service.ResolveEntry(ctx, payload.Destination); destinationErr == nil {
				return nil
			}
		}
		return err
	}
	total, err := h.Service.Index.CountObjects(ctx, payload.Source)
	if err != nil {
		return err
	}
	if total == 0 {
		return ErrObjectNotFound
	}

	var copied int64
	after := ""
	for {
		page, err := h.Service.List(ctx, ListOptions{Prefix: payload.Source, After: after, Limit: 500})
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			target := payload.Destination + strings.TrimPrefix(object.Key, payload.Source)
			if _, err := h.Service.Copy(ctx, object.Key, target); err != nil {
				return err
			}
			copied++
		}
		if err := h.progress(ctx, job.ID, .05+.60*float64(copied)/float64(total)); err != nil {
			return err
		}
		if page.NextMarker == "" {
			break
		}
		after = page.NextMarker
	}
	if err := h.deletePrefix(ctx, job.ID, payload.Source, total, .65); err != nil {
		return err
	}
	return h.progress(ctx, job.ID, .95)
}

func (h FileJobs) HandleDelete(ctx context.Context, job jobs.Job) error {
	payload, err := decodeFileJob(job)
	if err != nil {
		return err
	}
	if _, err := validateDirectoryPath(payload.Source, false); err != nil {
		return err
	}
	total, err := h.Service.Index.CountObjects(ctx, payload.Source)
	if err != nil {
		return err
	}
	if total == 0 {
		if job.Attempts > 1 {
			return nil
		}
		return ErrObjectNotFound
	}
	if err := h.deletePrefix(ctx, job.ID, payload.Source, total, .05); err != nil {
		return err
	}
	return h.progress(ctx, job.ID, .95)
}

func (h FileJobs) deletePrefix(ctx context.Context, jobID, prefix string, total int64, progressStart float64) error {
	var deleted int64
	for {
		page, err := h.Service.List(ctx, ListOptions{Prefix: prefix, Limit: 500})
		if err != nil {
			return err
		}
		if len(page.Objects) == 0 {
			return nil
		}
		for _, object := range page.Objects {
			if err := h.Service.Delete(ctx, object.Key); err != nil && !errors.Is(err, ErrObjectNotFound) {
				return err
			}
			deleted++
		}
		progress := progressStart + (.95-progressStart)*float64(deleted)/float64(total)
		if progress > .94 {
			progress = .94
		}
		if err := h.progress(ctx, jobID, progress); err != nil {
			return err
		}
	}
}

func (h FileJobs) progress(ctx context.Context, jobID string, value float64) error {
	if h.Jobs == nil {
		return nil
	}
	return h.Jobs.SetProgress(ctx, jobID, value)
}

func decodeFileJob(job jobs.Job) (FileJobPayload, error) {
	var payload FileJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return FileJobPayload{}, fmt.Errorf("decode file job: %w", err)
	}
	if payload.Source == "" {
		return FileJobPayload{}, errors.New("source is required")
	}
	return payload, nil
}
