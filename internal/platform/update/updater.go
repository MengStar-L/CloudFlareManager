// Package update implements in-place self-updating from GitHub releases:
// check the latest release, download the platform binary, verify its
// checksum, swap the running executable, and restart.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultRepo is the GitHub repository releases are published to.
const DefaultRepo = "MengStar-L/CloudFlareManager"

const checkCacheTTL = 10 * time.Minute

type ReleaseInfo struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseNotes    string `json:"release_notes,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	AssetName       string `json:"asset_name,omitempty"`
	AssetSize       int64  `json:"asset_size,omitempty"`

	assetURL     string
	checksumsURL string
}

type Updater struct {
	Repo           string
	CurrentVersion string
	// BaseURL overrides the GitHub API endpoint (tests).
	BaseURL string
	Client  *http.Client
	// ExecutablePath overrides os.Executable (tests).
	ExecutablePath string
	Logger         *slog.Logger

	mu       sync.Mutex
	cached   *ReleaseInfo
	cachedAt time.Time
	updating bool
}

func (u *Updater) repo() string {
	if u.Repo == "" {
		return DefaultRepo
	}
	return u.Repo
}

func (u *Updater) baseURL() string {
	if u.BaseURL == "" {
		return "https://api.github.com"
	}
	return strings.TrimRight(u.BaseURL, "/")
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (u *Updater) executable() (string, error) {
	if u.ExecutablePath != "" {
		return u.ExecutablePath, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// AssetName returns the release asset expected for this platform.
func AssetName() string {
	name := "cf-r2-manager-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Check queries the latest GitHub release, with a short cache to stay well
// inside unauthenticated API rate limits.
func (u *Updater) Check(ctx context.Context, force bool) (ReleaseInfo, error) {
	u.mu.Lock()
	if !force && u.cached != nil && time.Since(u.cachedAt) < checkCacheTTL {
		info := *u.cached
		u.mu.Unlock()
		return info, nil
	}
	u.mu.Unlock()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL()+"/repos/"+u.repo()+"/releases/latest", nil)
	if err != nil {
		return ReleaseInfo{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "cf-r2-manager-updater")
	response, err := u.client().Do(request)
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("query GitHub releases: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return ReleaseInfo{}, err
	}
	if response.StatusCode == http.StatusNotFound {
		return ReleaseInfo{}, fmt.Errorf("repository %s has no published releases", u.repo())
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ReleaseInfo{}, fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	var release struct {
		TagName     string `json:"tag_name"`
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return ReleaseInfo{}, fmt.Errorf("decode GitHub release: %w", err)
	}

	info := ReleaseInfo{
		CurrentVersion:  u.CurrentVersion,
		LatestVersion:   release.TagName,
		UpdateAvailable: versionLess(u.CurrentVersion, release.TagName),
		ReleaseNotes:    release.Body,
		PublishedAt:     release.PublishedAt,
	}
	wanted := AssetName()
	for _, asset := range release.Assets {
		switch asset.Name {
		case wanted:
			info.AssetName, info.AssetSize, info.assetURL = asset.Name, asset.Size, asset.URL
		case "checksums.txt":
			info.checksumsURL = asset.URL
		}
	}

	u.mu.Lock()
	u.cached, u.cachedAt = &info, time.Now()
	u.mu.Unlock()
	return info, nil
}

// Apply downloads the latest release binary, verifies it, and swaps the
// current executable. The caller is responsible for restarting afterwards.
func (u *Updater) Apply(ctx context.Context) (string, error) {
	u.mu.Lock()
	if u.updating {
		u.mu.Unlock()
		return "", fmt.Errorf("an update is already in progress")
	}
	u.updating = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.updating = false
		u.mu.Unlock()
	}()

	info, err := u.Check(ctx, true)
	if err != nil {
		return "", err
	}
	if !info.UpdateAvailable {
		return "", fmt.Errorf("already running the latest version (%s)", u.CurrentVersion)
	}
	if info.assetURL == "" {
		return "", fmt.Errorf("release %s has no asset for %s/%s", info.LatestVersion, runtime.GOOS, runtime.GOARCH)
	}

	exe, err := u.executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	staging := exe + ".new"
	if err := u.download(ctx, info.assetURL, staging); err != nil {
		return "", err
	}
	defer os.Remove(staging)

	if info.checksumsURL != "" {
		if err := u.verifyChecksum(ctx, info.checksumsURL, info.AssetName, staging); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", err
	}

	// 原地换刀：先把运行中的旧程序改名（运行中重命名在 Linux/Windows 都允许），
	// 再把新程序挪到原路径；失败则回滚。
	backup := exe + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(exe, backup); err != nil {
		return "", fmt.Errorf("stage current executable: %w", err)
	}
	if err := os.Rename(staging, exe); err != nil {
		_ = os.Rename(backup, exe)
		return "", fmt.Errorf("install new executable: %w", err)
	}
	if u.Logger != nil {
		u.Logger.Info("update installed", "from", u.CurrentVersion, "to", info.LatestVersion)
	}
	return info.LatestVersion, nil
}

func (u *Updater) download(ctx context.Context, url, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "cf-r2-manager-updater")
	// 下载走不限时的客户端（跟随请求 ctx），大文件在慢速链路上可能超过默认超时。
	client := &http.Client{}
	if u.Client != nil {
		client = &http.Client{Transport: u.Client.Transport}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("download update: HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, response.Body); err != nil {
		file.Close()
		os.Remove(destination)
		return fmt.Errorf("write update: %w", err)
	}
	return file.Close()
}

func (u *Updater) verifyChecksum(ctx context.Context, checksumsURL, assetName, path string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumsURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "cf-r2-manager-updater")
	response, err := u.client().Do(request)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	expected := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./")
		if name == assetName {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", assetName)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", assetName, expected, actual)
	}
	return nil
}

// CleanupOld removes the backup left behind by a previous update.
func (u *Updater) CleanupOld() {
	if exe, err := u.executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// versionLess reports whether current is older than latest. Non-semver
// current versions (e.g. "dev") count as older than any published release.
func versionLess(current, latest string) bool {
	if latest == "" {
		return false
	}
	currentParts, currentOK := parseSemver(current)
	latestParts, latestOK := parseSemver(latest)
	if !latestOK {
		return current != latest
	}
	if !currentOK {
		return true
	}
	for i := 0; i < 3; i++ {
		if currentParts[i] != latestParts[i] {
			return currentParts[i] < latestParts[i]
		}
	}
	return false
}

func parseSemver(value string) ([3]int, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	fields := strings.Split(value, ".")
	if len(fields) == 0 || len(fields) > 3 {
		return [3]int{}, false
	}
	var parts [3]int
	for i, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil {
			return [3]int{}, false
		}
		parts[i] = number
	}
	return parts, true
}
