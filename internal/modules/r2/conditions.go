package r2

import (
	"errors"
	"strings"
	"time"
)

// EntityTag is the parsed opaque value of an HTTP entity-tag.
type EntityTag struct {
	Value string
	Weak  bool
}

// EntityTagSet represents an If-Match or If-None-Match field value.
type EntityTagSet struct {
	Wildcard bool
	Tags     []EntityTag
}

// MutationConditions are evaluated atomically against the committed logical
// object before a write intent is created.
type MutationConditions struct {
	IfMatch           *EntityTagSet
	IfNoneMatch       *EntityTagSet
	IfUnmodifiedSince *time.Time
}

func checkMutationConditions(object *Object, conditions MutationConditions) error {
	exists := object != nil
	if conditions.IfMatch != nil {
		if !exists || !conditions.IfMatch.stronglyMatches(object.ETag) {
			return ErrPreconditionFailed
		}
	} else if conditions.IfUnmodifiedSince != nil && exists {
		modified := object.LastModified.Truncate(time.Second)
		if modified.After(conditions.IfUnmodifiedSince.UTC().Truncate(time.Second)) {
			return ErrPreconditionFailed
		}
	}
	if conditions.IfNoneMatch != nil && exists && conditions.IfNoneMatch.weaklyMatches(object.ETag) {
		return ErrPreconditionFailed
	}
	return nil
}

func (condition EntityTagSet) stronglyMatches(current string) bool {
	if condition.Wildcard {
		return true
	}
	current = strings.Trim(current, `"`)
	for _, candidate := range condition.Tags {
		if !candidate.Weak && candidate.Value == current {
			return true
		}
	}
	return false
}

func (condition EntityTagSet) weaklyMatches(current string) bool {
	if condition.Wildcard {
		return true
	}
	current = strings.Trim(current, `"`)
	for _, candidate := range condition.Tags {
		if candidate.Value == current {
			return true
		}
	}
	return false
}

func quoteETag(value string) string {
	if value == "" {
		return ""
	}
	return `"` + strings.Trim(value, `"`) + `"`
}

var (
	ErrPreconditionFailed         = errors.New("mutation precondition failed")
	ErrConditionalRequestConflict = errors.New("conditional request conflict")
	ErrRateLimited                = errors.New("upstream rate limited")
)
