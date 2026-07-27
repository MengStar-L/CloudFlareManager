package r2

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const DirectoryContentType = "httpd/unix-directory"

type EntryKind string

const (
	EntryMount     EntryKind = "mount"
	EntryDirectory EntryKind = "directory"
	EntryFile      EntryKind = "file"
)

type FileEntry struct {
	Name         string    `json:"name"`
	Key          string    `json:"key"`
	Kind         EntryKind `json:"kind"`
	Size         int64     `json:"size"`
	ContentType  string    `json:"content_type"`
	ETag         string    `json:"etag,omitempty"`
	LastModified time.Time `json:"last_modified"`
	MountID      string    `json:"mount_id,omitempty"`
	Disabled     bool      `json:"disabled,omitempty"`
}

type DirectoryListOptions struct {
	Path  string
	After string
	Kind  EntryKind
	Limit int
}

type DirectoryList struct {
	Path           string      `json:"path"`
	Entries        []FileEntry `json:"entries"`
	DirectoryCount int         `json:"directory_count"`
	FileCount      int         `json:"file_count"`
	NextMarker     string      `json:"next_marker,omitempty"`
	MountID        string      `json:"mount_id,omitempty"`
	MountName      string      `json:"mount_name,omitempty"`
}

func (s Service) ListWebDAVDirectory(ctx context.Context, credentialID string, options DirectoryListOptions) (DirectoryList, error) {
	if s.Index == nil {
		return DirectoryList{}, errors.New("R2 service is not configured")
	}
	visiblePath, err := validateDirectoryPath(options.Path, true)
	if err != nil {
		return DirectoryList{}, err
	}
	prefix, err := WebDAVMountKey(credentialID, "")
	if err != nil {
		return DirectoryList{}, err
	}
	options.Path = prefix + visiblePath
	if visiblePath != "" {
		entry, err := s.ResolveEntry(ctx, options.Path)
		if err != nil {
			return DirectoryList{}, err
		}
		if entry.Kind != EntryDirectory {
			return DirectoryList{}, ErrInvalidPath
		}
	}
	result, err := s.Index.ListDirectory(ctx, options)
	if err != nil {
		return DirectoryList{}, err
	}
	result.Path = visiblePath
	result.MountID = credentialID
	for index := range result.Entries {
		visible, ok := WebDAVVisibleKey(credentialID, result.Entries[index].Key)
		if !ok {
			return DirectoryList{}, ErrInvalidPath
		}
		result.Entries[index].Key = visible
		result.Entries[index].MountID = credentialID
	}
	return result, nil
}

func (s Service) ResolveWebDAVEntry(ctx context.Context, credentialID, key string) (FileEntry, error) {
	internal, err := WebDAVMountKey(credentialID, key)
	if err != nil || key == "" {
		return FileEntry{}, ErrInvalidPath
	}
	entry, err := s.ResolveEntry(ctx, internal)
	if err != nil {
		return FileEntry{}, err
	}
	visible, ok := WebDAVVisibleKey(credentialID, entry.Key)
	if !ok {
		return FileEntry{}, ErrInvalidPath
	}
	entry.Key = visible
	entry.MountID = credentialID
	return entry, nil
}

func (s Service) CreateWebDAVDirectory(ctx context.Context, credentialID, key string) (FileEntry, error) {
	internal, err := WebDAVMountKey(credentialID, key)
	if err != nil {
		return FileEntry{}, err
	}
	entry, err := s.CreateDirectory(ctx, internal)
	if err != nil {
		return FileEntry{}, err
	}
	visible, ok := WebDAVVisibleKey(credentialID, entry.Key)
	if !ok {
		return FileEntry{}, ErrInvalidPath
	}
	entry.Key = visible
	entry.MountID = credentialID
	return entry, nil
}

func (s Service) ListDirectory(ctx context.Context, options DirectoryListOptions) (DirectoryList, error) {
	if s.Index == nil {
		return DirectoryList{}, errors.New("R2 service is not configured")
	}
	path, err := validateDirectoryPath(options.Path, true)
	if err != nil {
		return DirectoryList{}, err
	}
	if path != "" {
		entry, err := s.ResolveEntry(ctx, path)
		if err != nil {
			return DirectoryList{}, err
		}
		if entry.Kind != EntryDirectory {
			return DirectoryList{}, ErrInvalidPath
		}
	}
	options.Path = path
	return s.Index.ListDirectory(ctx, options)
}

func (s *Store) ListDirectory(ctx context.Context, options DirectoryListOptions) (DirectoryList, error) {
	if options.Kind != "" && options.Kind != EntryDirectory && options.Kind != EntryFile {
		return DirectoryList{}, ErrInvalidPath
	}
	if options.Limit <= 0 || options.Limit > 500 {
		options.Limit = 100
	}
	after := ""
	if options.After != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(options.After)
		if err != nil {
			return DirectoryList{}, ErrInvalidCursor
		}
		after = string(decoded)
	}
	args := []any{options.Path, StateCommitted, escapeLike(options.Path) + "%", options.Path}
	var directoryCount, fileCount int
	if err := s.db.QueryRowContext(ctx, directoryChildrenCTE+`
		SELECT COALESCE(SUM(CASE WHEN is_directory = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN is_directory = 0 THEN 1 ELSE 0 END), 0) FROM children`, args...).Scan(&directoryCount, &fileCount); err != nil {
		return DirectoryList{}, err
	}

	kind := string(options.Kind)
	rows, err := s.db.QueryContext(ctx, directoryChildrenCTE+`
		SELECT child, is_directory, size, content_type, etag, last_modified, sort_key
		FROM children
		WHERE (? = '' OR sort_key > ?)
			AND (? = '' OR (? = 'directory' AND is_directory = 1) OR (? = 'file' AND is_directory = 0))
		ORDER BY sort_key LIMIT ?`, append(args, after, after, kind, kind, kind, options.Limit+1)...)
	if err != nil {
		return DirectoryList{}, err
	}
	defer rows.Close()

	entries := make([]FileEntry, 0, options.Limit+1)
	sortKeys := make([]string, 0, options.Limit+1)
	for rows.Next() {
		var entry FileEntry
		var child, sortKey string
		var directory bool
		var modified int64
		if err := rows.Scan(&child, &directory, &entry.Size, &entry.ContentType, &entry.ETag, &modified, &sortKey); err != nil {
			return DirectoryList{}, err
		}
		entry.Name = strings.TrimSuffix(child, "/")
		entry.Key = options.Path + child
		entry.Kind = EntryFile
		if directory {
			entry.Kind = EntryDirectory
		}
		entry.LastModified = time.Unix(0, modified)
		entries = append(entries, entry)
		sortKeys = append(sortKeys, sortKey)
	}
	if err := rows.Err(); err != nil {
		return DirectoryList{}, err
	}
	result := DirectoryList{
		Path: options.Path, Entries: entries, DirectoryCount: directoryCount, FileCount: fileCount,
	}
	if len(entries) > options.Limit {
		result.NextMarker = base64.RawURLEncoding.EncodeToString([]byte(sortKeys[options.Limit-1]))
		result.Entries = entries[:options.Limit]
	}
	return result, nil
}

func (s Service) ResolveEntry(ctx context.Context, key string) (FileEntry, error) {
	if key == "" {
		return FileEntry{Name: "/", Kind: EntryDirectory, ContentType: DirectoryContentType}, nil
	}
	if err := validateLogicalPath(key); err != nil {
		return FileEntry{}, err
	}
	object, err := s.Stat(ctx, key)
	if err == nil {
		kind := EntryFile
		if strings.HasSuffix(key, "/") || object.Metadata["webdav-directory"] == "true" {
			kind = EntryDirectory
		}
		return entryFromObject(object, kind), nil
	}
	if !errors.Is(err, ErrObjectNotFound) {
		return FileEntry{}, err
	}
	if !strings.HasSuffix(key, "/") {
		return FileEntry{}, ErrObjectNotFound
	}
	list, err := s.List(ctx, ListOptions{Prefix: key, Limit: 1})
	if err != nil {
		return FileEntry{}, err
	}
	if len(list.Objects) == 0 {
		return FileEntry{}, ErrObjectNotFound
	}
	return FileEntry{
		Name: strings.TrimSuffix(lastPathSegment(key), "/"), Key: key, Kind: EntryDirectory,
		ContentType: DirectoryContentType, LastModified: list.Objects[0].LastModified,
	}, nil
}

func (s Service) CreateDirectory(ctx context.Context, key string) (FileEntry, error) {
	key, err := validateDirectoryPath(key, false)
	if err != nil {
		return FileEntry{}, err
	}
	if _, err := s.ResolveEntry(ctx, key); err == nil {
		return FileEntry{}, ErrFileConflict
	} else if !errors.Is(err, ErrObjectNotFound) {
		return FileEntry{}, err
	}
	if _, err := s.Stat(ctx, strings.TrimSuffix(key, "/")); err == nil {
		return FileEntry{}, ErrFileConflict
	} else if !errors.Is(err, ErrObjectNotFound) {
		return FileEntry{}, err
	}
	object, err := s.Put(ctx, PutRequest{
		Key: key, Size: 0, ContentType: DirectoryContentType,
		Metadata: map[string]string{"webdav-directory": "true"},
	})
	if err != nil {
		return FileEntry{}, err
	}
	return entryFromObject(object, EntryDirectory), nil
}

func (s Service) PutFile(ctx context.Context, request PutRequest, overwrite bool) (Object, error) {
	if err := validateLogicalPath(request.Key); err != nil || strings.HasSuffix(request.Key, "/") {
		return Object{}, ErrInvalidPath
	}
	if _, err := s.ResolveEntry(ctx, request.Key+"/"); err == nil {
		return Object{}, ErrFileConflict
	} else if !errors.Is(err, ErrObjectNotFound) {
		return Object{}, err
	}
	if _, err := s.Stat(ctx, request.Key); err == nil && !overwrite {
		return Object{}, ErrFileConflict
	} else if err != nil && !errors.Is(err, ErrObjectNotFound) {
		return Object{}, err
	}
	return s.Put(ctx, request)
}

func (s Service) MoveFile(ctx context.Context, source, destination string, overwrite bool) error {
	if err := validateLogicalPath(source); err != nil || strings.HasSuffix(source, "/") {
		return ErrInvalidPath
	}
	if err := validateLogicalPath(destination); err != nil || strings.HasSuffix(destination, "/") || source == destination {
		return ErrInvalidPath
	}
	entry, err := s.ResolveEntry(ctx, source)
	if err != nil {
		return err
	}
	if entry.Kind != EntryFile {
		return ErrInvalidPath
	}
	if _, err := s.ResolveEntry(ctx, destination+"/"); err == nil {
		return ErrFileConflict
	} else if !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	if _, err := s.Stat(ctx, destination); err == nil && !overwrite {
		return ErrFileConflict
	} else if err != nil && !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	if _, err := s.Copy(ctx, source, destination); err != nil {
		return err
	}
	return s.Delete(ctx, source)
}

func (s Service) ValidateDirectoryMove(ctx context.Context, source, destination string, overwrite bool) error {
	var err error
	if source, err = validateDirectoryPath(source, false); err != nil {
		return err
	}
	if destination, err = validateDirectoryPath(destination, false); err != nil {
		return err
	}
	if source == destination || strings.HasPrefix(destination, source) {
		return ErrInvalidPath
	}
	entry, err := s.ResolveEntry(ctx, source)
	if err != nil {
		return err
	}
	if entry.Kind != EntryDirectory {
		return ErrInvalidPath
	}
	if _, err := s.Stat(ctx, strings.TrimSuffix(destination, "/")); err == nil {
		return ErrFileConflict
	} else if !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	if _, err := s.ResolveEntry(ctx, destination); err == nil && !overwrite {
		return ErrFileConflict
	} else if err != nil && !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	return nil
}

func (s *Store) CountObjects(ctx context.Context, prefix string) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM r2_objects
		WHERE state = ? AND object_key LIKE ? ESCAPE '\'`, StateCommitted, escapeLike(prefix)+"%").Scan(&count)
	return count, err
}

func validateDirectoryPath(value string, allowRoot bool) (string, error) {
	if value == "" && allowRoot {
		return "", nil
	}
	if err := validateLogicalPath(value); err != nil || !strings.HasSuffix(value, "/") {
		return "", ErrInvalidPath
	}
	return value, nil
}

func validateLogicalPath(value string) error {
	maxLength := 1024
	if IsWebDAVInternalKey(value) {
		maxLength = 2048
	}
	if value == "" || len(value) > maxLength || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return ErrInvalidPath
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return ErrInvalidPath
		}
	}
	trimmed := strings.TrimSuffix(value, "/")
	if trimmed == "" {
		return ErrInvalidPath
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ErrInvalidPath
		}
	}
	return nil
}

func entryFromObject(object Object, kind EntryKind) FileEntry {
	return FileEntry{
		Name: strings.TrimSuffix(lastPathSegment(object.Key), "/"), Key: object.Key, Kind: kind,
		Size: object.Size, ContentType: object.ContentType, ETag: object.ETag, LastModified: object.LastModified,
	}
}

func lastPathSegment(key string) string {
	trimmed := strings.TrimSuffix(key, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

const directoryChildrenCTE = `
	WITH matching AS (
		SELECT object_key, size, content_type, etag, last_modified,
			substr(object_key, length(?) + 1) AS relative
		FROM r2_objects
		WHERE state = ? AND object_key LIKE ? ESCAPE '\' AND object_key <> ?
	), classified AS (
		SELECT CASE WHEN instr(relative, '/') > 0 THEN substr(relative, 1, instr(relative, '/')) ELSE relative END AS child,
			CASE WHEN instr(relative, '/') > 0 THEN 1 ELSE 0 END AS is_directory,
			size, content_type, etag, last_modified
		FROM matching WHERE relative <> ''
	), children AS (
		SELECT child, is_directory,
			CASE WHEN is_directory = 1 THEN 0 ELSE MAX(size) END AS size,
			CASE WHEN is_directory = 1 THEN '` + DirectoryContentType + `' ELSE MAX(content_type) END AS content_type,
			CASE WHEN is_directory = 1 THEN '' ELSE MAX(etag) END AS etag,
			MAX(last_modified) AS last_modified,
			(CASE WHEN is_directory = 1 THEN '0:' ELSE '1:' END) || child AS sort_key
		FROM classified GROUP BY child, is_directory
	)
`

var (
	ErrInvalidPath   = errors.New("invalid file path")
	ErrInvalidCursor = errors.New("invalid directory cursor")
	ErrFileConflict  = errors.New("file or directory already exists")
)
