package accounts

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestCapabilityJobDoesNotPublishResultsFromReplacedAPIToken(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		response *http.Response
		err      error
	}{
		{
			name: "completed detection",
			response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
			},
		},
		{name: "failed detection", err: errors.New("probe connection failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cipher, err := secret.NewCipher(bytes.Repeat([]byte{11}, secret.KeySize))
			if err != nil {
				t.Fatal(err)
			}
			store := NewStore(db, secret.NewRepository(db, cipher))
			ctx := context.Background()
			account, err := store.Create(ctx, CreateInput{
				Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "old-token",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetCapabilities(ctx, account.ID, []Capability{{Name: "stale", Available: true}}); err != nil {
				t.Fatal(err)
			}
			if err := store.SetHealth(ctx, account.ID, "healthy", "stale health"); err != nil {
				t.Fatal(err)
			}

			started := make(chan struct{})
			release := make(chan struct{})
			transport := &blockingProbeTransport{started: started, release: release, response: test.response, err: test.err}
			handler := CapabilityJobHandler{
				Store:    store,
				Verifier: Verifier{BaseURL: "https://cloudflare.invalid", Client: &http.Client{Transport: transport}},
			}
			job := jobs.Job{Payload: []byte(`{"account_id":"` + account.ID + `"}`)}
			done := make(chan error, 1)
			go func() { done <- handler.Handle(ctx, job) }()
			<-started
			newToken := "new-token"
			if _, err := store.UpdateCredentials(ctx, account.ID, UpdateCredentialsInput{APIToken: &newToken}); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("stale capability job returned error: %v", err)
			}
			got, err := store.Get(ctx, account.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.HealthStatus != "unknown" || got.HealthError != "" || len(got.Capabilities) != 0 {
				t.Fatalf("stale capability job overwrote reset state: %#v", got)
			}
		})
	}
}

func TestCapabilityJobCompletesWhenAccountIsDeletedDuringDetection(t *testing.T) {
	t.Parallel()

	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{12}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, secret.NewRepository(db, cipher))
	ctx := context.Background()
	account, err := store.Create(ctx, CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	handler := CapabilityJobHandler{
		Store: store,
		Verifier: Verifier{
			BaseURL: "https://cloudflare.invalid",
			Client: &http.Client{Transport: &blockingProbeTransport{
				started: started,
				release: release,
				response: &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
				},
			}},
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- handler.Handle(ctx, jobs.Job{Payload: []byte(`{"account_id":"` + account.ID + `"}`)})
	}()
	<-started
	if err := store.Delete(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("capability job after account deletion = %v, want successful completion", err)
	}
}

type blockingProbeTransport struct {
	started  chan struct{}
	release  chan struct{}
	response *http.Response
	err      error
	once     bool
}

func (t *blockingProbeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if !t.once {
		t.once = true
		close(t.started)
		<-t.release
	}
	if t.err != nil {
		return nil, t.err
	}
	response := *t.response
	response.Body = http.NoBody
	return &response, nil
}
