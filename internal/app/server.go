package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	aimodule "github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai"
	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/d1"
	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/audit"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/auth"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/config"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/credentials"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/httpapi"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/metrics"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/update"
	aiprotocol "github.com/cf-r2-manager/cf-r2-manager/internal/protocol/ai"
	s3protocol "github.com/cf-r2-manager/cf-r2-manager/internal/protocol/s3"
	webdavprotocol "github.com/cf-r2-manager/cf-r2-manager/internal/protocol/webdav"
	webassets "github.com/cf-r2-manager/cf-r2-manager/web"
)

type Server struct {
	Config  config.Config
	Version string
	Logger  *slog.Logger
}

func (s Server) Run(ctx context.Context) error {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	key, err := secret.LoadMasterKey(secret.ResolveMasterKeyPath(s.Config.MasterKeyFile))
	if err != nil {
		return err
	}
	cipher, err := secret.NewCipher(key)
	if err != nil {
		return err
	}
	db, err := database.Open(s.Config.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	authStore := auth.NewStore(db)
	initialized, err := authStore.IsInitialized(ctx)
	if err != nil {
		return err
	}
	if !initialized {
		return errors.New("administrator is not initialized; run the init command first")
	}
	secretStore := secret.NewRepository(db, cipher)
	accountStore := accounts.NewStore(db, secretStore)
	credentialStore := credentials.NewStore(db, secretStore)
	jobStore := jobs.NewStore(db)
	auditStore := audit.NewStore(db)
	r2Store := r2.NewStore(db, r2.Limits{
		StorageBytes: s.Config.R2.StorageSoftLimit, ClassA: s.Config.R2.ClassASoftLimit, ClassB: s.Config.R2.ClassBSoftLimit,
	})
	webDAVCredentials, err := credentialStore.List(ctx, credentials.KindWebDAV)
	if err != nil {
		return fmt.Errorf("list WebDAV credentials for namespace migration: %w", err)
	}
	webDAVCredentialIDs := make([]string, 0, len(webDAVCredentials))
	for index := len(webDAVCredentials) - 1; index >= 0; index-- {
		webDAVCredentialIDs = append(webDAVCredentialIDs, webDAVCredentials[index].ID)
	}
	migration, err := r2Store.EnsureWebDAVNamespaces(ctx, webDAVCredentialIDs)
	if err != nil {
		return fmt.Errorf("migrate WebDAV namespaces: %w", err)
	}
	if migration.MigratedObjects > 0 {
		logger.Info("legacy R2 objects assigned to WebDAV mount", "credential_id", migration.TargetCredentialID, "objects", migration.MigratedObjects)
	}
	r2.CleanupStagedUploads(s.Config.R2.TempDir, logger)
	r2Service := r2.Service{
		Index: r2Store, Accounts: accountStore, Backend: r2.AWSBackend{},
		TempDir: s.Config.R2.TempDir, ChunkBytes: s.Config.R2.UploadChunkBytes,
	}
	d1Client := &d1.Client{Accounts: accountStore, DB: db, Backups: r2Service}
	aiGateway := &aimodule.Gateway{
		Accounts: accountStore, DB: db, NeuronSoftLimit: s.Config.AI.NeuronSoftLimit,
		MaxRetryAccounts: s.Config.AI.MaxRetryAccounts,
	}
	aiManagement := &aimodule.Management{Accounts: accountStore}
	runner := jobs.NewRunner(jobStore)
	runner.Logger = logger
	capabilityHandler := accounts.CapabilityJobHandler{Store: accountStore, Verifier: accounts.Verifier{}}
	runner.Register(accounts.CapabilityJobType, capabilityHandler.Handle)
	maintenanceJobs := r2.MaintenanceJobs{Service: r2Service, Jobs: jobStore}
	runner.Register(r2.AdoptBucketJobType, maintenanceJobs.HandleAdopt)
	runner.Register(r2.OrphanScanJobType, maintenanceJobs.HandleOrphanScan)
	runner.Register(r2.RebuildIndexJobType, maintenanceJobs.HandleRebuild)
	runner.Register(r2.RecoverStateJobType, maintenanceJobs.HandleRecover)
	runner.Register(r2.RebalanceJobType, maintenanceJobs.HandleRebalance)
	fileJobs := r2.FileJobs{Service: r2Service, Jobs: jobStore}
	runner.Register(r2.FileMoveJobType, fileJobs.HandleMove)
	runner.Register(r2.FileDeleteJobType, fileJobs.HandleDelete)
	if _, err := jobStore.Enqueue(ctx, r2.RecoverStateJobType, map[string]string{"source": "startup"}, 3); err != nil {
		return fmt.Errorf("schedule R2 recovery: %w", err)
	}

	updater := &update.Updater{Repo: update.DefaultRepo, CurrentVersion: s.Version, Logger: logger}
	updater.CleanupOld()

	registry := metrics.NewRegistry()
	adminHandler := httpapi.New(httpapi.Dependencies{
		DB: db, Auth: authStore, Accounts: accountStore, Jobs: jobStore, Audit: auditStore,
		Credentials: credentialStore, R2: r2Store, R2Service: &r2Service, Updater: updater, D1: d1Client,
		AI: aiGateway, AIManagement: aiManagement, Static: webassets.Handler(), Version: s.Version,
		LogicalBucket: s.Config.R2.LogicalBucket,
	})
	s3Handler := s3protocol.Handler{
		Bucket: s.Config.R2.LogicalBucket, Objects: r2Service,
		Auth: s3protocol.Authenticator{Resolve: func(ctx context.Context, accessKey string) (s3protocol.Identity, string, error) {
			secretValue, credential, err := credentialStore.Secret(ctx, credentials.KindS3, accessKey)
			if err != nil {
				return s3protocol.Identity{}, "", err
			}
			return s3protocol.Identity{ID: credential.ID, PublicID: credential.PublicID, Scopes: credential.Scopes}, secretValue, nil
		}},
	}
	webdavHandler := webdavprotocol.Handler{
		Objects: r2Service, Locks: webdavprotocol.NewLockStore(db),
		Verify: func(ctx context.Context, username, password string) (webdavprotocol.Identity, error) {
			credential, err := credentialStore.Verify(ctx, credentials.KindWebDAV, username, password)
			if err != nil {
				return webdavprotocol.Identity{}, err
			}
			return webdavprotocol.Identity{ID: credential.ID, Scopes: credential.Scopes}, nil
		},
	}
	aiHandler := aiprotocol.Handler{
		Gateway: aiGateway,
		Models: func(ctx context.Context) ([]map[string]any, error) {
			account, err := aiManagement.PickAccount(ctx)
			if err != nil {
				return nil, err
			}
			return aiManagement.ListModels(ctx, account.ID)
		},
		Verify: func(ctx context.Context, publicID, secretValue string) (aiprotocol.Identity, error) {
			credential, err := credentialStore.Verify(ctx, credentials.KindAI, publicID, secretValue)
			if err != nil {
				return aiprotocol.Identity{}, err
			}
			return aiprotocol.Identity{ID: credential.ID, Scopes: credential.Scopes}, nil
		},
	}
	for name, address := range map[string]string{
		"s3": s.Config.Listeners.S3, "webdav": s.Config.Listeners.WebDAV, "ai": s.Config.Listeners.AI,
	} {
		if address != "" {
			logger.Warn("legacy protocol listener ignored; use the unified HTTP listener", "listener", name, "address", address, "http_address", s.Config.Listeners.HTTP)
		}
	}
	sharedHandler := protocolMux{
		Admin:  registry.Instrument("admin", adminHandler),
		S3:     registry.Instrument("s3", s3Handler),
		WebDAV: registry.Instrument("webdav", webdavHandler),
		AI:     registry.Instrument("ai", aiHandler),
	}
	servers := []*http.Server{
		newHTTPServer("http", s.Config.Listeners.HTTP, sharedHandler),
		newHTTPServer("metrics", s.Config.Listeners.Metrics, registry.Handler(db, s.Version)),
	}

	errCh := make(chan error, len(servers)+1)
	go func() { errCh <- runner.Run(ctx) }()
	for _, server := range servers {
		server := server
		go func() {
			logger.Info("listener started", "name", server.ErrorLog.Prefix(), "address", server.Addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listen on %s: %w", server.Addr, err)
				return
			}
			errCh <- nil
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdownCtx)
	}
	return nil
}

func newHTTPServer(name, address string, handler http.Handler) *http.Server {
	errorLog := slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)
	errorLog.SetPrefix(name + " ")
	return &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
		ErrorLog: errorLog,
	}
}

func unavailableHandler(message string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{%q:%q}`, "error", message)
	})
}
