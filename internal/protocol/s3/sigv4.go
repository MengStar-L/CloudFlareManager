package s3protocol

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const algorithm = "AWS4-HMAC-SHA256"

type Identity struct {
	ID       string
	PublicID string
	Scopes   []string
}

func (i Identity) HasScope(scope string) bool {
	for _, candidate := range i.Scopes {
		if candidate == scope || candidate == "r2:*" || candidate == "*" {
			return true
		}
	}
	return false
}

type SecretResolver func(context.Context, string) (Identity, string, error)

type Authenticator struct {
	Resolve SecretResolver
	Now     func() time.Time
}

func (a Authenticator) Authenticate(request *http.Request) (Identity, error) {
	if a.Resolve == nil {
		return Identity{}, ErrInvalidAccessKey
	}
	if request.URL.Query().Get("X-Amz-Algorithm") != "" {
		return a.authenticatePresigned(request)
	}
	return a.authenticateHeader(request)
}

func (a Authenticator) authenticateHeader(request *http.Request) (Identity, error) {
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, algorithm+" ") {
		return Identity{}, ErrAuthorizationRequired
	}
	fields, err := parseAuthorization(strings.TrimPrefix(authorization, algorithm+" "))
	if err != nil {
		return Identity{}, err
	}
	credential, err := parseCredential(fields["Credential"])
	if err != nil {
		return Identity{}, err
	}
	signedHeaders := fields["SignedHeaders"]
	signature := fields["Signature"]
	if signedHeaders == "" || len(signature) != sha256.Size*2 {
		return Identity{}, ErrMalformedAuthorization
	}
	requestTime, err := parseRequestTime(request.Header.Get("X-Amz-Date"))
	if err != nil {
		return Identity{}, err
	}
	if !within(requestTime, a.now(), 15*time.Minute) {
		return Identity{}, ErrRequestTimeSkewed
	}
	identity, secret, err := a.Resolve(request.Context(), credential.AccessKey)
	if err != nil {
		return Identity{}, ErrInvalidAccessKey
	}
	payloadHash := request.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = emptyPayloadHash
	}
	canonical, err := canonicalRequest(request, signedHeaders, payloadHash, false)
	if err != nil {
		return Identity{}, err
	}
	want := calculateSignature(secret, credential, requestTime, canonical)
	if !constantHexEqual(signature, want) {
		return Identity{}, ErrSignatureMismatch
	}
	return identity, nil
}

func (a Authenticator) authenticatePresigned(request *http.Request) (Identity, error) {
	query := request.URL.Query()
	if query.Get("X-Amz-Algorithm") != algorithm {
		return Identity{}, ErrMalformedAuthorization
	}
	credential, err := parseCredential(query.Get("X-Amz-Credential"))
	if err != nil {
		return Identity{}, err
	}
	requestTime, err := parseRequestTime(query.Get("X-Amz-Date"))
	if err != nil {
		return Identity{}, err
	}
	expires, err := strconv.ParseInt(query.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 0 || expires > 7*24*60*60 {
		return Identity{}, ErrMalformedAuthorization
	}
	now := a.now()
	if now.Before(requestTime.Add(-15*time.Minute)) || now.After(requestTime.Add(time.Duration(expires)*time.Second)) {
		return Identity{}, ErrRequestExpired
	}
	identity, secret, err := a.Resolve(request.Context(), credential.AccessKey)
	if err != nil {
		return Identity{}, ErrInvalidAccessKey
	}
	signedHeaders := query.Get("X-Amz-SignedHeaders")
	signature := query.Get("X-Amz-Signature")
	canonical, err := canonicalRequest(request, signedHeaders, "UNSIGNED-PAYLOAD", true)
	if err != nil {
		return Identity{}, err
	}
	want := calculateSignature(secret, credential, requestTime, canonical)
	if !constantHexEqual(signature, want) {
		return Identity{}, ErrSignatureMismatch
	}
	return identity, nil
}

type credentialScope struct {
	AccessKey string
	Date      string
	Region    string
	Service   string
	Terminal  string
}

func parseCredential(value string) (credentialScope, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 5 || parts[0] == "" || len(parts[1]) != 8 || parts[3] != "s3" || parts[4] != "aws4_request" {
		return credentialScope{}, ErrMalformedAuthorization
	}
	return credentialScope{AccessKey: parts[0], Date: parts[1], Region: parts[2], Service: parts[3], Terminal: parts[4]}, nil
}

func parseAuthorization(value string) (map[string]string, error) {
	result := make(map[string]string)
	for _, part := range strings.Split(value, ",") {
		key, item, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || key == "" || item == "" {
			return nil, ErrMalformedAuthorization
		}
		result[key] = item
	}
	return result, nil
}

func canonicalRequest(request *http.Request, signedHeaders, payloadHash string, presigned bool) (string, error) {
	headers := strings.Split(signedHeaders, ";")
	if len(headers) == 0 || signedHeaders == "" {
		return "", ErrMalformedAuthorization
	}
	canonicalHeaders := strings.Builder{}
	previous := ""
	for _, name := range headers {
		if name == "" || name != strings.ToLower(name) || (previous != "" && name <= previous) {
			return "", ErrMalformedAuthorization
		}
		previous = name
		var values []string
		if name == "host" {
			values = []string{request.Host}
		} else if name == "content-length" && request.ContentLength >= 0 {
			values = []string{strconv.FormatInt(request.ContentLength, 10)}
		} else {
			values = request.Header.Values(name)
		}
		if len(values) == 0 {
			return "", ErrMalformedAuthorization
		}
		for index := range values {
			values[index] = collapseSpaces(values[index])
		}
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.Join(values, ","))
		canonicalHeaders.WriteByte('\n')
	}
	return strings.Join([]string{
		request.Method,
		canonicalURI(request.URL),
		canonicalQuery(request.URL.RawQuery, presigned),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n"), nil
}

func calculateSignature(secret string, credential credentialScope, requestTime time.Time, canonical string) string {
	hash := sha256.Sum256([]byte(canonical))
	scope := strings.Join([]string{credential.Date, credential.Region, credential.Service, credential.Terminal}, "/")
	stringToSign := algorithm + "\n" + requestTime.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + hex.EncodeToString(hash[:])
	dateKey := hmacSHA256([]byte("AWS4"+secret), credential.Date)
	regionKey := hmacSHA256(dateKey, credential.Region)
	serviceKey := hmacSHA256(regionKey, credential.Service)
	signingKey := hmacSHA256(serviceKey, credential.Terminal)
	return hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
}

func canonicalURI(value *url.URL) string {
	path := value.EscapedPath()
	if path == "" {
		return "/"
	}
	return strings.ReplaceAll(path, "+", "%20")
}

func canonicalQuery(raw string, excludeSignature bool) string {
	values, _ := url.ParseQuery(raw)
	type pair struct{ key, value string }
	var pairs []pair
	for key, items := range values {
		if excludeSignature && strings.EqualFold(key, "X-Amz-Signature") {
			continue
		}
		if len(items) == 0 {
			items = []string{""}
		}
		for _, item := range items {
			pairs = append(pairs, pair{awsEncode(key), awsEncode(item)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	parts := make([]string, len(pairs))
	for index, item := range pairs {
		parts[index] = item.key + "=" + item.value
	}
	return strings.Join(parts, "&")
}

func awsEncode(value string) string {
	const hexChars = "0123456789ABCDEF"
	var encoded strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' || character == '~' {
			encoded.WriteByte(character)
		} else {
			encoded.WriteByte('%')
			encoded.WriteByte(hexChars[character>>4])
			encoded.WriteByte(hexChars[character&15])
		}
	}
	return encoded.String()
}

func collapseSpaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func hmacSHA256(key []byte, value string) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}

func constantHexEqual(left, right string) bool {
	leftBytes, leftErr := hex.DecodeString(left)
	rightBytes, rightErr := hex.DecodeString(right)
	return leftErr == nil && rightErr == nil && len(leftBytes) == len(rightBytes) && subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}

func parseRequestTime(value string) (time.Time, error) {
	parsed, err := time.Parse("20060102T150405Z", value)
	if err != nil {
		return time.Time{}, ErrMalformedAuthorization
	}
	return parsed, nil
}

func within(left, right time.Time, duration time.Duration) bool {
	difference := left.Sub(right)
	return difference >= -duration && difference <= duration
}

func (a Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var (
	ErrAuthorizationRequired  = errors.New("authorization header is required")
	ErrMalformedAuthorization = errors.New("authorization is malformed")
	ErrInvalidAccessKey       = errors.New("invalid access key")
	ErrSignatureMismatch      = errors.New("signature does not match")
	ErrRequestTimeSkewed      = errors.New("request time is too skewed")
	ErrRequestExpired         = errors.New("presigned request has expired")
)
