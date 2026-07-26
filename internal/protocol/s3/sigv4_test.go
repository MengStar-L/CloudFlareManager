package s3protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestAuthenticatorVerifiesAWSV4HeaderSignature(t *testing.T) {
	t.Parallel()

	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	now := time.Date(2026, 7, 25, 8, 30, 0, 0, time.UTC)
	request, err := http.NewRequest(http.MethodGet, "http://localhost:9000/storage/docs/readme.txt?versionId=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(payload[:])
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	credentials := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}
	if err := awsv4.NewSigner().SignHTTP(context.Background(), credentials, request, payloadHash, "s3", "auto", now); err != nil {
		t.Fatal(err)
	}

	authenticator := Authenticator{
		Now: func() time.Time { return now },
		Resolve: func(_ context.Context, got string) (Identity, string, error) {
			if got != accessKey {
				t.Fatalf("access key = %q", got)
			}
			return Identity{PublicID: got, Scopes: []string{"r2:read"}}, secretKey, nil
		},
	}
	identity, err := authenticator.Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if identity.PublicID != accessKey || !identity.HasScope("r2:read") {
		t.Fatalf("identity = %#v", identity)
	}

	request.Header.Set("X-Amz-Date", "20260725T083100Z")
	if _, err := authenticator.Authenticate(request); err != ErrSignatureMismatch {
		t.Fatalf("modified request error = %v", err)
	}
}

func TestAuthenticatorVerifiesAWSPresignedRequest(t *testing.T) {
	t.Parallel()

	const accessKey = "CFR2EXAMPLE"
	const secretKey = "super-secret-value"
	now := time.Date(2026, 7, 25, 8, 45, 0, 0, time.UTC)
	request, err := http.NewRequest(http.MethodGet, "http://localhost:9000/storage/docs/readme.txt?response-content-type=text%2Fplain&X-Amz-Expires=300", nil)
	if err != nil {
		t.Fatal(err)
	}
	signedURL, signedHeaders, err := awsv4.NewSigner().PresignHTTP(
		context.Background(), aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		request, "UNSIGNED-PAYLOAD", "s3", "auto", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	presigned, err := http.NewRequest(http.MethodGet, signedURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	presigned.Header = signedHeaders
	authenticator := Authenticator{
		Now: func() time.Time { return now.Add(time.Minute) },
		Resolve: func(_ context.Context, got string) (Identity, string, error) {
			return Identity{PublicID: got, Scopes: []string{"r2:read"}}, secretKey, nil
		},
	}
	if _, err := authenticator.Authenticate(presigned); err != nil {
		t.Fatalf("authenticate presigned request: %v", err)
	}

	query := presigned.URL.Query()
	query.Set("response-content-type", "application/json")
	presigned.URL.RawQuery = query.Encode()
	if _, err := authenticator.Authenticate(presigned); err != ErrSignatureMismatch {
		t.Fatalf("modified presigned request error = %v", err)
	}
}
