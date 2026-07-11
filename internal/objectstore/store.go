package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	blobPrefix = "tooltend-blob-v1\n"
	treePrefix = "tooltend-tree-v1\n"
)

type Store struct{ root string }

func New(root string) (*Store, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("objectstore: root must be absolute")
	}
	return &Store{root: filepath.Clean(root)}, nil
}

func (s *Store) Root() string { return s.root }

type EntryKind string

const (
	EntryFile    EntryKind = "file"
	EntryDir     EntryKind = "dir"
	EntrySymlink EntryKind = "symlink"
)

type TreeEntry struct {
	Path       string    `json:"path"`
	Kind       EntryKind `json:"kind"`
	Mode       uint32    `json:"mode"`
	ObjectHash string    `json:"object_hash,omitempty"`
	Size       int64     `json:"size,omitempty"`
	LinkTarget string    `json:"link_target,omitempty"`
}

type TreeManifest struct {
	Version int         `json:"version"`
	Entries []TreeEntry `json:"entries"`
}

type CaptureOptions struct {
	Ignore func(relativePath string, info fs.FileInfo) bool
}

func (s *Store) PutBlob(ctx context.Context, reader io.Reader) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return "", 0, fmt.Errorf("objectstore: create root: %w", err)
	}
	f, err := os.CreateTemp(s.root, ".object-*.tmp")
	if err != nil {
		return "", 0, fmt.Errorf("objectstore: create blob temp: %w", err)
	}
	tmp := f.Name()
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	writer := io.MultiWriter(f, hasher)
	if _, err := io.WriteString(writer, blobPrefix); err != nil {
		return "", 0, err
	}
	n, err := copyWithContext(ctx, writer, reader)
	if err != nil {
		return "", 0, fmt.Errorf("objectstore: read blob: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		return "", 0, err
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	if err := s.commitTemp(tmp, hash); err != nil {
		return "", 0, err
	}
	keep = true
	return hash, n, nil
}

func (s *Store) OpenBlob(hash string) (io.ReadCloser, error) {
	if err := validateHash(hash); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.root, hash))
	if err != nil {
		return nil, fmt.Errorf("objectstore: open %s: %w", hash, err)
	}
	valid := false
	defer func() {
		if !valid {
			_ = f.Close()
		}
	}()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != hash {
		return nil, fmt.Errorf("objectstore: checksum mismatch for %s", hash)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	prefix := make([]byte, len(blobPrefix))
	if _, err := io.ReadFull(f, prefix); err != nil {
		return nil, err
	}
	if string(prefix) != blobPrefix {
		return nil, fmt.Errorf("objectstore: %s is not a blob", hash)
	}
	valid = true
	return f, nil
}

func (s *Store) CaptureTree(ctx context.Context, root string, options CaptureOptions) (string, TreeManifest, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return "", TreeManifest{}, fmt.Errorf("objectstore: stat tree root: %w", err)
	}
	if !info.IsDir() {
		return "", TreeManifest{}, fmt.Errorf("objectstore: tree root is not a directory")
	}
	manifest := TreeManifest{Version: 1, Entries: []TreeEntry{}}
	err = filepath.WalkDir(root, func(fullPath string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if fullPath == root {
			return nil
		}
		relOS, err := filepath.Rel(root, fullPath)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relOS)
		if err := validateRelativePath(rel); err != nil {
			return err
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		if options.Ignore != nil && options.Ignore(rel, info) {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entry := TreeEntry{Path: rel}
		switch {
		case info.Mode().IsDir():
			entry.Kind, entry.Mode = EntryDir, 0o755
		case info.Mode().IsRegular():
			entry.Kind, entry.Mode = EntryFile, normalizedFileMode(info.Mode())
			file, err := os.Open(fullPath)
			if err != nil {
				return err
			}
			hash, size, putErr := s.PutBlob(ctx, file)
			closeErr := file.Close()
			if putErr != nil {
				return putErr
			}
			if closeErr != nil {
				return closeErr
			}
			entry.ObjectHash, entry.Size = hash, size
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind, entry.Mode = EntrySymlink, 0o777
			target, err := os.Readlink(fullPath)
			if err != nil {
				return err
			}
			if err := validateLinkTarget(rel, target); err != nil {
				return err
			}
			entry.LinkTarget = target
		default:
			return fmt.Errorf("objectstore: unsupported file type at %s", rel)
		}
		manifest.Entries = append(manifest.Entries, entry)
		return nil
	})
	if err != nil {
		return "", TreeManifest{}, fmt.Errorf("objectstore: capture tree: %w", err)
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	hash, err := s.putTreeManifest(manifest)
	if err != nil {
		return "", TreeManifest{}, err
	}
	return hash, manifest, nil
}

// FingerprintTree computes the exact tree identity CaptureTree would use
// without creating the object directory or writing blobs. It is used to bind
// an interactive preview to the local content that was actually reviewed.
func (s *Store) FingerprintTree(ctx context.Context, root string, options CaptureOptions) (string, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("objectstore: stat tree root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("objectstore: tree root is not a directory")
	}
	manifest := TreeManifest{Version: 1, Entries: []TreeEntry{}}
	err = filepath.WalkDir(root, func(fullPath string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if fullPath == root {
			return nil
		}
		relOS, err := filepath.Rel(root, fullPath)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relOS)
		if err := validateRelativePath(rel); err != nil {
			return err
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		if options.Ignore != nil && options.Ignore(rel, info) {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entry := TreeEntry{Path: rel}
		switch {
		case info.Mode().IsDir():
			entry.Kind, entry.Mode = EntryDir, 0o755
		case info.Mode().IsRegular():
			entry.Kind, entry.Mode = EntryFile, normalizedFileMode(info.Mode())
			file, err := os.Open(fullPath)
			if err != nil {
				return err
			}
			hasher := sha256.New()
			if _, err := io.WriteString(hasher, blobPrefix); err != nil {
				_ = file.Close()
				return err
			}
			size, copyErr := copyWithContext(ctx, hasher, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if size != info.Size() {
				return fmt.Errorf("objectstore: file changed while fingerprinting %s", rel)
			}
			entry.ObjectHash, entry.Size = hex.EncodeToString(hasher.Sum(nil)), size
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind, entry.Mode = EntrySymlink, 0o777
			target, err := os.Readlink(fullPath)
			if err != nil {
				return err
			}
			if err := validateLinkTarget(rel, target); err != nil {
				return err
			}
			entry.LinkTarget = target
		default:
			return fmt.Errorf("objectstore: unsupported file type at %s", rel)
		}
		manifest.Entries = append(manifest.Entries, entry)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("objectstore: fingerprint tree: %w", err)
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	if err := validateManifest(manifest); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("objectstore: encode tree: %w", err)
	}
	return digest(append([]byte(treePrefix), encoded...)), nil
}

func (s *Store) ReadTree(hash string) (TreeManifest, error) {
	b, err := s.readEncoded(hash)
	if err != nil {
		return TreeManifest{}, err
	}
	if !bytes.HasPrefix(b, []byte(treePrefix)) {
		return TreeManifest{}, fmt.Errorf("objectstore: %s is not a tree", hash)
	}
	var manifest TreeManifest
	if err := json.Unmarshal(b[len(treePrefix):], &manifest); err != nil {
		return TreeManifest{}, fmt.Errorf("objectstore: decode tree: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return TreeManifest{}, err
	}
	return manifest, nil
}

func (s *Store) VerifyBlob(hash string) error {
	b, err := s.readEncoded(hash)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(b, []byte(blobPrefix)) {
		return fmt.Errorf("objectstore: %s is not a blob", hash)
	}
	return nil
}

func (s *Store) VerifyTree(hash string) error {
	manifest, err := s.ReadTree(hash)
	if err != nil {
		return err
	}
	for _, entry := range manifest.Entries {
		if entry.Kind != EntryFile {
			continue
		}
		reader, err := s.OpenBlob(entry.ObjectHash)
		if err != nil {
			return fmt.Errorf("objectstore: verify %s: %w", entry.Path, err)
		}
		size, copyErr := io.Copy(io.Discard, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if size != entry.Size {
			return fmt.Errorf("objectstore: size mismatch for %s", entry.Path)
		}
	}
	return nil
}

// MaterializeTree builds a fresh sibling directory and atomically renames it to destination.
func (s *Store) MaterializeTree(ctx context.Context, hash, destination string) (err error) {
	if !filepath.IsAbs(destination) {
		return fmt.Errorf("objectstore: destination must be absolute")
	}
	if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
		if statErr == nil {
			return fmt.Errorf("objectstore: destination already exists")
		}
		return statErr
	}
	manifest, err := s.ReadTree(hash)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := ensureSafeParent(parent); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tooltend-materialize-*")
	if err != nil {
		return fmt.Errorf("objectstore: create materialize temp: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()

	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Kind != EntryDir {
			continue
		}
		target, err := safeJoin(tmp, entry.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(target, fs.FileMode(entry.Mode)); err != nil {
			return err
		}
	}
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Kind == EntryDir {
			continue
		}
		target, err := safeJoin(tmp, entry.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch entry.Kind {
		case EntryFile:
			if err := s.materializeFile(entry, target); err != nil {
				return err
			}
		case EntrySymlink:
			if err := validateLinkTarget(entry.Path, entry.LinkTarget); err != nil {
				return err
			}
			if err := os.Symlink(entry.LinkTarget, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("objectstore: unsupported entry kind %q", entry.Kind)
		}
	}
	if err := syncTree(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return fmt.Errorf("objectstore: activate materialized tree: %w", err)
	}
	if dir, openErr := os.Open(parent); openErr == nil {
		defer dir.Close()
		if syncErr := dir.Sync(); syncErr != nil {
			return syncErr
		}
	}
	return nil
}

func (s *Store) materializeFile(entry TreeEntry, target string) (err error) {
	reader, err := s.OpenBlob(entry.ObjectHash)
	if err != nil {
		return err
	}
	defer reader.Close()
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(entry.Mode))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	n, err := io.Copy(file, reader)
	if err != nil {
		return err
	}
	if n != entry.Size {
		return fmt.Errorf("objectstore: size mismatch materializing %s", entry.Path)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func (s *Store) putTreeManifest(manifest TreeManifest) (string, error) {
	if err := validateManifest(manifest); err != nil {
		return "", err
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("objectstore: encode tree: %w", err)
	}
	encoded := append([]byte(treePrefix), b...)
	hash := digest(encoded)
	if err := s.putEncoded(hash, encoded); err != nil {
		return "", err
	}
	return hash, nil
}

func (s *Store) putEncoded(hash string, encoded []byte) (err error) {
	if err := validateHash(hash); err != nil {
		return err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("objectstore: create root: %w", err)
	}
	target := filepath.Join(s.root, hash)
	if _, err := os.Stat(target); err == nil {
		matches, verifyErr := fileHasDigest(target, hash)
		if verifyErr != nil {
			return verifyErr
		}
		if !matches {
			return fmt.Errorf("objectstore: corrupt existing object %s", hash)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(s.root, ".object-*.tmp")
	if err != nil {
		return fmt.Errorf("objectstore: create temp: %w", err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(encoded); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return s.commitTemp(tmp, hash)
}

func (s *Store) commitTemp(tmp, hash string) error {
	if err := validateHash(hash); err != nil {
		return err
	}
	target := filepath.Join(s.root, hash)
	if _, err := os.Stat(target); err == nil {
		matches, verifyErr := fileHasDigest(target, hash)
		if verifyErr != nil {
			return verifyErr
		}
		if !matches {
			return fmt.Errorf("objectstore: corrupt existing object %s", hash)
		}
		return os.Remove(tmp)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		if matches, verifyErr := fileHasDigest(target, hash); verifyErr == nil && matches {
			_ = os.Remove(tmp)
			return nil
		}
		return err
	}
	if err := os.Chmod(target, 0o400); err != nil {
		return err
	}
	if dir, openErr := os.Open(s.root); openErr == nil {
		defer dir.Close()
		if syncErr := dir.Sync(); syncErr != nil {
			return syncErr
		}
	}
	return nil
}

func fileHasDigest(filePath, expected string) (bool, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(hasher.Sum(nil)) == expected, nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			count, writeErr := destination.Write(buffer[:n])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func (s *Store) readEncoded(hash string) ([]byte, error) {
	if err := validateHash(hash); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(s.root, hash))
	if err != nil {
		return nil, fmt.Errorf("objectstore: read %s: %w", hash, err)
	}
	if digest(b) != hash {
		return nil, fmt.Errorf("objectstore: checksum mismatch for %s", hash)
	}
	return b, nil
}

func validateManifest(manifest TreeManifest) error {
	if manifest.Version != 1 {
		return fmt.Errorf("objectstore: unsupported tree version %d", manifest.Version)
	}
	previous := ""
	kinds := make(map[string]EntryKind, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if err := validateRelativePath(entry.Path); err != nil {
			return err
		}
		if previous != "" && entry.Path <= previous {
			return fmt.Errorf("objectstore: tree entries are not strictly sorted")
		}
		previous = entry.Path
		switch entry.Kind {
		case EntryDir:
			if entry.ObjectHash != "" || entry.LinkTarget != "" {
				return fmt.Errorf("objectstore: invalid directory entry %s", entry.Path)
			}
		case EntryFile:
			if err := validateHash(entry.ObjectHash); err != nil {
				return fmt.Errorf("objectstore: %s: %w", entry.Path, err)
			}
			if entry.Size < 0 {
				return fmt.Errorf("objectstore: negative size for %s", entry.Path)
			}
		case EntrySymlink:
			if err := validateLinkTarget(entry.Path, entry.LinkTarget); err != nil {
				return err
			}
		default:
			return fmt.Errorf("objectstore: invalid entry kind %q", entry.Kind)
		}
		kinds[entry.Path] = entry.Kind
	}
	for _, entry := range manifest.Entries {
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			if kind, exists := kinds[parent]; exists && kind != EntryDir {
				return fmt.Errorf("objectstore: non-directory %s is a parent of %s", parent, entry.Path)
			}
		}
	}
	return nil
}

func validateRelativePath(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("objectstore: unsafe relative path %q", value)
	}
	return nil
}

func validateLinkTarget(entryPath, target string) error {
	if target == "" || strings.ContainsRune(target, '\x00') || filepath.IsAbs(target) {
		return fmt.Errorf("objectstore: unsafe symlink target %q for %s", target, entryPath)
	}
	resolved := path.Clean(path.Join(path.Dir(entryPath), filepath.ToSlash(target)))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("objectstore: symlink escapes tree at %s", entryPath)
	}
	return nil
}

func safeJoin(root, relative string) (string, error) {
	if err := validateRelativePath(relative); err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("objectstore: path escapes root")
	}
	return target, nil
}

func ensureSafeParent(parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("objectstore: destination parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("objectstore: destination parent must be a real directory")
	}
	return nil
}

func normalizedFileMode(mode fs.FileMode) uint32 {
	if mode.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func validateHash(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("objectstore: invalid sha256 %q", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("objectstore: invalid sha256 %q", value)
	}
	return nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dirPath := range dirs {
		dir, err := os.Open(dirPath)
		if err != nil {
			return err
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
