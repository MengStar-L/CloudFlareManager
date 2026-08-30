package s3protocol

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

var errInvalidReadCondition = errors.New("invalid conditional request header")

type readEntityTag struct {
	opaque string
	weak   bool
}

type readEntityTagSet struct {
	wildcard bool
	tags     []readEntityTag
}

type ifRangeCondition struct {
	tag   *readEntityTag
	date  *time.Time
	valid bool
}

type readConditions struct {
	ifMatch           *readEntityTagSet
	ifUnmodifiedSince *time.Time
	ifNoneMatch       *readEntityTagSet
	ifModifiedSince   *time.Time
	ifRange           *ifRangeCondition
}

type readConditionEvaluation struct {
	status int
	range_ string
}

func evaluateReadConditions(request *http.Request, object r2.Object) (readConditionEvaluation, error) {
	conditions, err := parseReadConditions(request)
	if err != nil {
		return readConditionEvaluation{}, err
	}
	current := strings.Trim(object.ETag, `"`)
	if conditions.ifMatch != nil && !conditions.ifMatch.matches(current, true) {
		return readConditionEvaluation{status: http.StatusPreconditionFailed}, nil
	}
	if conditions.ifMatch == nil && conditions.ifUnmodifiedSince != nil &&
		object.LastModified.UTC().Truncate(time.Second).After(conditions.ifUnmodifiedSince.UTC().Truncate(time.Second)) {
		return readConditionEvaluation{status: http.StatusPreconditionFailed}, nil
	}
	if conditions.ifNoneMatch != nil && conditions.ifNoneMatch.matches(current, false) {
		return readConditionEvaluation{status: http.StatusNotModified}, nil
	}
	if conditions.ifNoneMatch == nil && conditions.ifModifiedSince != nil && !object.LastModified.IsZero() &&
		!object.LastModified.UTC().Truncate(time.Second).After(conditions.ifModifiedSince.UTC().Truncate(time.Second)) {
		return readConditionEvaluation{status: http.StatusNotModified}, nil
	}
	evaluation := readConditionEvaluation{range_: request.Header.Get("Range")}
	if evaluation.range_ != "" && conditions.ifRange != nil && !conditions.ifRange.matches(current, object.LastModified) {
		evaluation.range_ = ""
	}
	return evaluation, nil
}

func parseReadConditions(request *http.Request) (readConditions, error) {
	var result readConditions
	var err error
	if values := request.Header.Values("If-Match"); len(values) != 0 {
		result.ifMatch, err = parseReadEntityTagSet(values)
		if err != nil {
			return readConditions{}, errInvalidReadCondition
		}
	}
	if values := request.Header.Values("If-Unmodified-Since"); len(values) == 1 {
		result.ifUnmodifiedSince = parseHTTPDate(values[0])
	}
	if values := request.Header.Values("If-None-Match"); len(values) != 0 {
		result.ifNoneMatch, err = parseReadEntityTagSet(values)
		if err != nil {
			return readConditions{}, errInvalidReadCondition
		}
	}
	if values := request.Header.Values("If-Modified-Since"); len(values) == 1 {
		result.ifModifiedSince = parseHTTPDate(values[0])
	}
	if values := request.Header.Values("If-Range"); len(values) != 0 {
		result.ifRange = parseIfRange(values)
	}
	return result, nil
}

func parseReadEntityTagSet(values []string) (*readEntityTagSet, error) {
	value := strings.Join(values, ",")
	position := skipReadOWS(value, 0)
	if position < len(value) && value[position] == '*' {
		if skipReadOWS(value, position+1) != len(value) {
			return nil, errInvalidReadCondition
		}
		return &readEntityTagSet{wildcard: true}, nil
	}
	result := &readEntityTagSet{}
	for position < len(value) {
		tag, next, ok := parseReadEntityTag(value, position)
		if !ok {
			return nil, errInvalidReadCondition
		}
		result.tags = append(result.tags, tag)
		position = skipReadOWS(value, next)
		if position == len(value) {
			break
		}
		if value[position] != ',' {
			return nil, errInvalidReadCondition
		}
		position = skipReadOWS(value, position+1)
		if position == len(value) {
			return nil, errInvalidReadCondition
		}
	}
	if len(result.tags) == 0 {
		return nil, errInvalidReadCondition
	}
	return result, nil
}

func parseReadEntityTag(value string, position int) (readEntityTag, int, bool) {
	position = skipReadOWS(value, position)
	tag := readEntityTag{}
	if strings.HasPrefix(value[position:], "W/") {
		tag.weak = true
		position += 2
	}
	if position >= len(value) || value[position] != '"' {
		return readEntityTag{}, position, false
	}
	start := position + 1
	for position = start; position < len(value); position++ {
		current := value[position]
		if current == '"' {
			tag.opaque = value[start:position]
			return tag, position + 1, true
		}
		if current < 0x21 || current == 0x7f {
			return readEntityTag{}, position, false
		}
	}
	return readEntityTag{}, position, false
}

func skipReadOWS(value string, position int) int {
	for position < len(value) && (value[position] == ' ' || value[position] == '\t') {
		position++
	}
	return position
}

func (set readEntityTagSet) matches(current string, strong bool) bool {
	if set.wildcard {
		return true
	}
	for _, candidate := range set.tags {
		if candidate.opaque == current && (!strong || !candidate.weak) {
			return true
		}
	}
	return false
}

func parseIfRange(values []string) *ifRangeCondition {
	result := &ifRangeCondition{}
	if len(values) != 1 {
		return result
	}
	value := strings.TrimSpace(values[0])
	if parsed := parseHTTPDate(value); parsed != nil {
		result.date, result.valid = parsed, true
		return result
	}
	tag, position, ok := parseReadEntityTag(value, 0)
	if !ok || tag.weak || skipReadOWS(value, position) != len(value) {
		return result
	}
	result.tag, result.valid = &tag, true
	return result
}

func (condition ifRangeCondition) matches(current string, modified time.Time) bool {
	if !condition.valid {
		return false
	}
	if condition.tag != nil {
		return !condition.tag.weak && condition.tag.opaque == current
	}
	return condition.date != nil && !modified.IsZero() &&
		modified.UTC().Truncate(time.Second).Equal(condition.date.UTC().Truncate(time.Second))
}
