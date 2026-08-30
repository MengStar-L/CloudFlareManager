package webdavprotocol

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

var errInvalidConditionalHeader = errors.New("invalid conditional header")

// conditionState is the current state of one WebDAV resource. ETag accepts
// either the service's unquoted opaque value or a complete entity-tag.
type conditionState struct {
	Exists       bool
	Collection   bool
	Size         int64
	ObjectID     string
	ETag         string
	LastModified time.Time
	LockTokens   map[string]struct{}
}

type conditionStateResolver func(resource string) (conditionState, error)

type conditionOutcome uint8

const (
	conditionProceed conditionOutcome = iota
	conditionNotModified
	conditionPreconditionFailed
)

type conditionEvaluation struct {
	Outcome     conditionOutcome
	IgnoreRange bool
}

type requestConditions struct {
	requestResource   string
	ifMatch           *entityTagSet
	ifUnmodifiedSince *time.Time
	ifNoneMatch       *entityTagSet
	ifModifiedSince   *time.Time
	ifRange           *rangeCondition
	davIf             *davIfHeader
}

type entityTag struct {
	Weak   bool
	Opaque string
}

func (t entityTag) String() string {
	prefix := ""
	if t.Weak {
		prefix = "W/"
	}
	return prefix + `"` + t.Opaque + `"`
}

type entityTagSet struct {
	Any  bool
	Tags []entityTag
}

type rangeCondition struct {
	Tag  *entityTag
	Date *time.Time
}

// strongETag formats the server-owned opaque ETag consistently for HTTP and
// DAV:getetag. Invalid or empty values are omitted instead of emitted as an
// invalid validator.
func strongETag(value string) string {
	tag, ok := currentEntityTag(value)
	if !ok {
		return ""
	}
	tag.Weak = false
	return tag.String()
}

// parseRequestConditions parses every conditional field understood by the
// WebDAV handler. OPTIONS, CONNECT, and TRACE do not select or modify a
// representation, so their conditional fields are ignored.
func parseRequestConditions(request *http.Request) (requestConditions, error) {
	if request == nil || request.URL == nil {
		return requestConditions{}, fmt.Errorf("%w: missing request URL", errInvalidConditionalHeader)
	}
	resource, err := canonicalConditionResource(request.URL)
	if err != nil {
		return requestConditions{}, fmt.Errorf("%w: request URI: %v", errInvalidConditionalHeader, err)
	}
	conditions := requestConditions{requestResource: resource}
	if request.Method == http.MethodOptions || request.Method == http.MethodConnect || request.Method == http.MethodTrace {
		return conditions, nil
	}

	if values, present := conditionalHeaderValues(request.Header, "If-Match"); present {
		conditions.ifMatch, err = parseEntityTagSet(values)
		if err != nil {
			return requestConditions{}, invalidConditionalField("If-Match", err)
		}
	}
	if values, present := conditionalHeaderValues(request.Header, "If-Unmodified-Since"); present {
		conditions.ifUnmodifiedSince, err = parseConditionalDate(values)
		if err != nil {
			// RFC 9110 requires invalid or list-valued HTTP dates to be ignored.
			conditions.ifUnmodifiedSince = nil
		}
	}
	if values, present := conditionalHeaderValues(request.Header, "If-None-Match"); present {
		conditions.ifNoneMatch, err = parseEntityTagSet(values)
		if err != nil {
			return requestConditions{}, invalidConditionalField("If-None-Match", err)
		}
	}
	if values, present := conditionalHeaderValues(request.Header, "If-Modified-Since"); present && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		conditions.ifModifiedSince, err = parseConditionalDate(values)
		if err != nil {
			conditions.ifModifiedSince = nil
		}
	}
	if values, present := conditionalHeaderValues(request.Header, "If-Range"); present && request.Method == http.MethodGet && request.Header.Get("Range") != "" {
		conditions.ifRange, err = parseRangeCondition(values)
		if err != nil {
			// An invalid If-Range cannot validate a partial response. Retain an
			// always-stale condition so evaluation drops Range and sends 200.
			conditions.ifRange = &rangeCondition{}
		}
	}
	if values, present := conditionalHeaderValues(request.Header, "If"); present {
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return requestConditions{}, invalidConditionalField("If", errors.New("expected exactly one non-empty field value"))
		}
		parsed, parseErr := parseDAVIf(values[0], request)
		if parseErr != nil {
			return requestConditions{}, invalidConditionalField("If", parseErr)
		}
		conditions.davIf = &parsed
	}
	return conditions, nil
}

func invalidConditionalField(name string, err error) error {
	return fmt.Errorf("%w: %s: %v", errInvalidConditionalHeader, name, err)
}

// evaluate applies RFC 9110 precondition precedence with the DAV If field
// between lost-update validators and cache validators. The caller supplies
// the already selected Request-URI state; tagged DAV resources are resolved
// lazily through resolve.
func (c requestConditions) evaluate(method string, current conditionState, hasRange bool, resolve conditionStateResolver) (conditionEvaluation, error) {
	result := conditionEvaluation{Outcome: conditionProceed}
	if method == http.MethodOptions || method == http.MethodConnect || method == http.MethodTrace {
		return result, nil
	}
	if c.ifMatch != nil && !c.ifMatch.matchesStrong(current) {
		result.Outcome = conditionPreconditionFailed
		return result, nil
	}
	if c.ifMatch == nil && c.ifUnmodifiedSince != nil && !unmodifiedSince(current, *c.ifUnmodifiedSince) {
		result.Outcome = conditionPreconditionFailed
		return result, nil
	}
	if c.davIf != nil {
		davResolve := func(resource string) (conditionState, error) {
			if resource == c.requestResource {
				return current, nil
			}
			if resolve == nil {
				return conditionState{}, fmt.Errorf("no resolver for tagged resource %q", resource)
			}
			return resolve(resource)
		}
		matched, err := c.davIf.evaluate(davResolve)
		if err != nil {
			return conditionEvaluation{}, err
		}
		if !matched {
			result.Outcome = conditionPreconditionFailed
			return result, nil
		}
	}
	if c.ifNoneMatch != nil && !c.ifNoneMatch.matchesNone(current) {
		if method == http.MethodGet || method == http.MethodHead {
			result.Outcome = conditionNotModified
		} else {
			result.Outcome = conditionPreconditionFailed
		}
		return result, nil
	}
	if c.ifNoneMatch == nil && c.ifModifiedSince != nil && (method == http.MethodGet || method == http.MethodHead) && !modifiedSince(current, *c.ifModifiedSince) {
		result.Outcome = conditionNotModified
		return result, nil
	}
	if method == http.MethodGet && hasRange && c.ifRange != nil && !c.ifRange.matches(current) {
		result.IgnoreRange = true
	}
	return result, nil
}

func (s entityTagSet) matchesStrong(current conditionState) bool {
	if s.Any {
		return current.Exists
	}
	if !current.Exists {
		return false
	}
	actual, ok := currentEntityTag(current.ETag)
	if !ok || actual.Weak {
		return false
	}
	for _, candidate := range s.Tags {
		if !candidate.Weak && candidate.Opaque == actual.Opaque {
			return true
		}
	}
	return false
}

func (s entityTagSet) matchesNone(current conditionState) bool {
	if !current.Exists {
		return true
	}
	if s.Any {
		return false
	}
	actual, ok := currentEntityTag(current.ETag)
	if !ok {
		return true
	}
	for _, candidate := range s.Tags {
		if candidate.Opaque == actual.Opaque {
			return false
		}
	}
	return true
}

func unmodifiedSince(current conditionState, limit time.Time) bool {
	if !current.Exists || current.LastModified.IsZero() {
		return true
	}
	return !current.LastModified.Truncate(time.Second).After(limit)
}

func modifiedSince(current conditionState, limit time.Time) bool {
	if !current.Exists || current.LastModified.IsZero() {
		return true
	}
	return current.LastModified.Truncate(time.Second).After(limit)
}

func (condition rangeCondition) matches(current conditionState) bool {
	if !current.Exists {
		return false
	}
	if condition.Tag != nil {
		actual, ok := currentEntityTag(current.ETag)
		return ok && !condition.Tag.Weak && !actual.Weak && condition.Tag.Opaque == actual.Opaque
	}
	// A one-second HTTP date cannot safely validate representations that may
	// change more than once per second. Treat date validators as stale and send
	// the complete representation; strong ETags remain supported.
	return false
}

func parseEntityTagSet(values []string) (*entityTagSet, error) {
	if len(values) == 0 {
		return nil, errors.New("empty field value")
	}
	joined := strings.Join(values, ",")
	position := skipOWS(joined, 0)
	if position == len(joined) {
		return nil, errors.New("empty field value")
	}
	if joined[position] == '*' {
		if skipOWS(joined, position+1) != len(joined) {
			return nil, errors.New("wildcard cannot be combined with entity-tags")
		}
		return &entityTagSet{Any: true}, nil
	}

	set := &entityTagSet{}
	for {
		position = skipOWS(joined, position)
		tag, consumed, ok := scanEntityTag(joined[position:])
		if !ok {
			return nil, errors.New("expected entity-tag")
		}
		set.Tags = append(set.Tags, tag)
		position = skipOWS(joined, position+consumed)
		if position == len(joined) {
			return set, nil
		}
		if joined[position] != ',' {
			return nil, errors.New("expected comma between entity-tags")
		}
		position = skipOWS(joined, position+1)
		if position == len(joined) || joined[position] == ',' || joined[position] == '*' {
			return nil, errors.New("empty or wildcard list member")
		}
	}
}

func parseConditionalDate(values []string) (*time.Time, error) {
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return nil, errors.New("expected exactly one non-empty HTTP-date")
	}
	parsed, err := http.ParseTime(strings.TrimSpace(values[0]))
	if err != nil {
		return nil, errors.New("invalid HTTP-date")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func parseRangeCondition(values []string) (*rangeCondition, error) {
	if len(values) != 1 {
		return nil, errors.New("expected exactly one field value")
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return nil, errors.New("empty field value")
	}
	if tag, consumed, ok := scanEntityTag(value); ok && consumed == len(value) {
		return &rangeCondition{Tag: &tag}, nil
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return nil, errors.New("expected entity-tag or HTTP-date")
	}
	parsed = parsed.UTC()
	return &rangeCondition{Date: &parsed}, nil
}

func scanEntityTag(value string) (entityTag, int, bool) {
	position := 0
	weak := false
	if strings.HasPrefix(value, "W/") {
		weak = true
		position = 2
	}
	if position >= len(value) || value[position] != '"' {
		return entityTag{}, 0, false
	}
	start := position + 1
	for position = start; position < len(value); position++ {
		character := value[position]
		switch {
		case character == '"':
			return entityTag{Weak: weak, Opaque: value[start:position]}, position + 1, true
		case validETagByte(character):
		default:
			return entityTag{}, 0, false
		}
	}
	return entityTag{}, 0, false
}

func validETagByte(character byte) bool {
	return character == 0x21 || character >= 0x23 && character <= 0x7e || character >= 0x80
}

func currentEntityTag(value string) (entityTag, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return entityTag{}, false
	}
	if tag, consumed, ok := scanEntityTag(value); ok && consumed == len(value) {
		return tag, true
	}
	for index := 0; index < len(value); index++ {
		if !validETagByte(value[index]) {
			return entityTag{}, false
		}
	}
	return entityTag{Opaque: value}, true
}

func conditionalHeaderValues(header http.Header, name string) ([]string, bool) {
	var result []string
	present := false
	for key, values := range header {
		if strings.EqualFold(key, name) {
			present = true
			result = append(result, values...)
		}
	}
	return result, present
}

func skipOWS(value string, position int) int {
	for position < len(value) && (value[position] == ' ' || value[position] == '\t') {
		position++
	}
	return position
}

type davIfHeader struct {
	lists []davIfList
}

type davIfList struct {
	resource   string
	conditions []davIfCondition
}

type davIfCondition struct {
	not   bool
	token string
	tag   *entityTag
}

func parseDAVIf(value string, request *http.Request) (davIfHeader, error) {
	requestResource, err := canonicalConditionResource(request.URL)
	if err != nil {
		return davIfHeader{}, err
	}
	parser := davIfParser{value: value, request: request, requestResource: requestResource}
	return parser.parse()
}

type davIfParser struct {
	value           string
	position        int
	request         *http.Request
	requestResource string
}

func (p *davIfParser) parse() (davIfHeader, error) {
	p.position = skipOWS(p.value, p.position)
	if p.position == len(p.value) {
		return davIfHeader{}, errors.New("empty field value")
	}
	var header davIfHeader
	switch p.value[p.position] {
	case '(':
		for {
			list, err := p.parseList(p.requestResource)
			if err != nil {
				return davIfHeader{}, err
			}
			header.lists = append(header.lists, list)
			p.position = skipOWS(p.value, p.position)
			if p.position == len(p.value) {
				return header, nil
			}
			if p.value[p.position] != '(' {
				return davIfHeader{}, errors.New("cannot mix tagged and untagged lists")
			}
		}
	case '<':
		for p.position < len(p.value) {
			rawResource, err := p.parseAngleValue()
			if err != nil {
				return davIfHeader{}, fmt.Errorf("resource tag: %w", err)
			}
			resource, err := canonicalTaggedResource(rawResource, p.request)
			if err != nil {
				return davIfHeader{}, err
			}
			count := 0
			for {
				p.position = skipOWS(p.value, p.position)
				if p.position >= len(p.value) || p.value[p.position] != '(' {
					break
				}
				list, err := p.parseList(resource)
				if err != nil {
					return davIfHeader{}, err
				}
				header.lists = append(header.lists, list)
				count++
			}
			if count == 0 {
				return davIfHeader{}, errors.New("resource tag has no state list")
			}
			if p.position == len(p.value) {
				return header, nil
			}
			if p.value[p.position] != '<' {
				return davIfHeader{}, errors.New("cannot mix tagged and untagged lists")
			}
		}
	default:
		return davIfHeader{}, errors.New("expected tagged or untagged state list")
	}
	return davIfHeader{}, errors.New("incomplete field value")
}

func (p *davIfParser) parseList(resource string) (davIfList, error) {
	if p.position >= len(p.value) || p.value[p.position] != '(' {
		return davIfList{}, errors.New("expected opening parenthesis")
	}
	p.position++
	list := davIfList{resource: resource}
	for {
		p.position = skipOWS(p.value, p.position)
		if p.position >= len(p.value) {
			return davIfList{}, errors.New("unterminated state list")
		}
		if p.value[p.position] == ')' {
			if len(list.conditions) == 0 {
				return davIfList{}, errors.New("empty state list")
			}
			p.position++
			return list, nil
		}
		condition, err := p.parseDAVCondition()
		if err != nil {
			return davIfList{}, err
		}
		list.conditions = append(list.conditions, condition)
	}
}

func (p *davIfParser) parseDAVCondition() (davIfCondition, error) {
	condition := davIfCondition{}
	if strings.HasPrefix(p.value[p.position:], "Not") {
		next := p.position + len("Not")
		if next == len(p.value) || p.value[next] == ' ' || p.value[next] == '\t' || p.value[next] == '<' || p.value[next] == '[' {
			condition.not = true
			p.position = skipOWS(p.value, next)
		}
	}
	if p.position >= len(p.value) {
		return davIfCondition{}, errors.New("missing condition after Not")
	}
	switch p.value[p.position] {
	case '<':
		token, err := p.parseAngleValue()
		if err != nil {
			return davIfCondition{}, fmt.Errorf("state token: %w", err)
		}
		parsed, err := url.Parse(token)
		if err != nil || !parsed.IsAbs() || parsed.Fragment != "" || strings.TrimSpace(token) != token {
			return davIfCondition{}, errors.New("state token must be an absolute URI")
		}
		condition.token = token
	case '[':
		p.position++
		tag, consumed, ok := scanEntityTag(p.value[p.position:])
		if !ok {
			return davIfCondition{}, errors.New("invalid entity-tag condition")
		}
		p.position += consumed
		if p.position >= len(p.value) || p.value[p.position] != ']' {
			return davIfCondition{}, errors.New("entity-tag condition must end immediately with ]")
		}
		p.position++
		condition.tag = &tag
	default:
		return davIfCondition{}, errors.New("expected state token or entity-tag condition")
	}
	return condition, nil
}

func (p *davIfParser) parseAngleValue() (string, error) {
	if p.position >= len(p.value) || p.value[p.position] != '<' {
		return "", errors.New("expected <")
	}
	end := strings.IndexByte(p.value[p.position+1:], '>')
	if end < 0 {
		return "", errors.New("unterminated coded URL")
	}
	end += p.position + 1
	value := p.value[p.position+1 : end]
	p.position = end + 1
	if value == "" || strings.TrimSpace(value) != value {
		return "", errors.New("empty or whitespace-padded coded URL")
	}
	return value, nil
}

func (h davIfHeader) submittedLockTokens() []string {
	set := make(map[string]struct{})
	for _, list := range h.lists {
		for _, condition := range list.conditions {
			if condition.token != "" {
				set[condition.token] = struct{}{}
			}
		}
	}
	tokens := make([]string, 0, len(set))
	for token := range set {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	return tokens
}

// evaluateResource reports whether the header has lists for resource and,
// when it does, whether at least one such list evaluates to true.
func (h davIfHeader) evaluateResource(resource string, state conditionState) (applicable bool, satisfied bool) {
	for _, list := range h.lists {
		if list.resource != resource {
			continue
		}
		applicable = true
		if list.matches(state) {
			return true, true
		}
	}
	return applicable, false
}

func (h davIfHeader) evaluate(resolve conditionStateResolver) (bool, error) {
	if resolve == nil {
		return false, errors.New("condition state resolver is required")
	}
	states := make(map[string]conditionState)
	for _, list := range h.lists {
		state, ok := states[list.resource]
		if !ok {
			var err error
			state, err = resolve(list.resource)
			if err != nil {
				return false, err
			}
			states[list.resource] = state
		}
		if list.matches(state) {
			return true, nil
		}
	}
	return false, nil
}

func (list davIfList) matches(state conditionState) bool {
	for _, condition := range list.conditions {
		matched := false
		if condition.token != "" {
			_, matched = state.LockTokens[condition.token]
		} else if condition.tag != nil && state.Exists {
			actual, ok := currentEntityTag(state.ETag)
			matched = ok && !condition.tag.Weak && !actual.Weak && condition.tag.Opaque == actual.Opaque
		}
		if condition.not {
			matched = !matched
		}
		if !matched {
			return false
		}
	}
	return true
}

func canonicalTaggedResource(value string, request *http.Request) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("invalid resource tag URI")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("resource tag query is not supported")
	}
	if parsed.IsAbs() {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", errors.New("resource tag must use HTTP or HTTPS")
		}
		if parsed.Host == "" || !strings.EqualFold(parsed.Host, request.Host) {
			return "", errors.New("resource tag authority does not match request")
		}
		if request.URL.Scheme != "" && !strings.EqualFold(parsed.Scheme, request.URL.Scheme) {
			return "", errors.New("resource tag scheme does not match request")
		}
	} else if parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "", errors.New("resource tag must be an absolute URI or absolute path")
	}
	return canonicalConditionResource(parsed)
}

func canonicalConditionResource(value *url.URL) (string, error) {
	if value == nil {
		return "", errors.New("missing URI")
	}
	for _, segment := range strings.Split(value.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("dot segments are not allowed")
		}
	}
	trailing := strings.HasSuffix(value.Path, "/")
	cleaned := path.Clean("/" + value.Path)
	if trailing && cleaned != "/" {
		cleaned += "/"
	}
	// Keep the internal resource identifier URI-escaped. URL.Path has already
	// been decoded once by net/url, so re-escaping it here prevents literal
	// percent-encoded names from being decoded a second time during rechecks.
	cleaned = (&url.URL{Path: cleaned}).EscapedPath()
	if value.RawQuery != "" {
		cleaned += "?" + value.RawQuery
	}
	return cleaned, nil
}
