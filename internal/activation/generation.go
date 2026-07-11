package activation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const generationsDir = "generations"

func GenerationPath(root, generation string) (string, error) {
	if err := validateGeneration(generation); err != nil {
		return "", err
	}
	return filepath.Join(root, generationsDir, generation), nil
}

// Current returns the generation named by the current symlink. It validates
// the pointer syntax but deliberately does not require the target to exist so
// recovery can reason about a dangling pointer.
func Current(root string) (string, error) {
	var target string
	var err error
	for attempt := 0; attempt < 32; attempt++ {
		target, err = os.Readlink(filepath.Join(root, "current"))
		if !errors.Is(err, syscall.EINVAL) {
			break
		}
		// Darwin can transiently report EINVAL when readlink races an atomic
		// rename over the same symlink. The pointer itself is never partial;
		// retrying the syscall prevents exposing that kernel race to callers.
		runtime.Gosched()
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoCurrent
	}
	if err != nil {
		return "", fmt.Errorf("read current generation: %w", err)
	}
	if filepath.IsAbs(target) || filepath.Clean(target) != target {
		return "", fmt.Errorf("invalid current target %q", target)
	}
	dir, generation := filepath.Split(target)
	if filepath.Clean(dir) != generationsDir || generation == "" {
		return "", fmt.Errorf("current target escapes generations: %q", target)
	}
	if err := validateGeneration(generation); err != nil {
		return "", fmt.Errorf("invalid current target: %w", err)
	}
	return generation, nil
}

// SwitchCurrent atomically replaces current with a relative symlink to an
// already complete generation. It never follows a generation symlink.
func SwitchCurrent(root, generation string) error {
	if err := ensureRoot(root); err != nil {
		return err
	}
	generationPath, err := GenerationPath(root, generation)
	if err != nil {
		return err
	}
	info, err := os.Lstat(generationPath)
	if err != nil {
		return fmt.Errorf("inspect generation %q: %w", generation, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("generation %q is not a real directory", generation)
	}

	currentPath := filepath.Join(root, "current")
	if currentInfo, statErr := os.Lstat(currentPath); statErr == nil {
		if currentInfo.Mode()&os.ModeSymlink == 0 {
			return errors.New("current exists and is not a symlink")
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect current generation: %w", statErr)
	}

	tmp, err := temporaryPointer(root)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	target := filepath.Join(generationsDir, generation)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("create temporary generation pointer: %w", err)
	}
	if err := os.Rename(tmp, currentPath); err != nil {
		return fmt.Errorf("replace current generation: %w", err)
	}
	return syncDir(root)
}

func clearCurrent(root string) error {
	currentPath := filepath.Join(root, "current")
	info, err := os.Lstat(currentPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return errors.New("refusing to remove non-symlink current")
	}
	if err := os.Remove(currentPath); err != nil {
		return err
	}
	return syncDir(root)
}

// ClearCurrent removes the managed generation pointer without following it.
// Lifecycle operations use this to unwind a pointer switch when a surrounding
// host mutation or database transaction does not commit.
func ClearCurrent(root string) error {
	return clearCurrent(root)
}

// HashGeneration returns a deterministic SHA-256 over relative paths, object
// types, permission bits, and file bytes. Symlinks and special files are
// rejected rather than followed.
func HashGeneration(root string) (string, error) {
	return hashGeneration(root, false, nil)
}

// HashGenerationAllowSafeSymlinks is used for isolated package-manager
// runtimes, whose internal bin directories commonly contain relative links.
// Link targets are hashed without following them and must remain inside the
// generation root. File-like agent extensions continue to use the stricter
// HashGeneration function.
func HashGenerationAllowSafeSymlinks(root string) (string, error) {
	return hashGeneration(root, true, nil)
}

// HashRuntimeGeneration hashes an isolated npm/Python runtime while ignoring
// only package-manager generated caches that may appear after first use. The
// immutable package files and safe relative links remain integrity protected.
func HashRuntimeGeneration(root string) (string, error) {
	return hashGeneration(root, true, isMutableRuntimePath)
}

func hashGeneration(root string, allowSafeSymlinks bool, ignore func(string) bool) (string, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("generation root is not a real directory")
	}

	digest := sha256.New()
	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("generation path escapes root")
		}
		relative = filepath.ToSlash(relative)
		if ignore != nil && ignore(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if !allowSafeSymlinks {
				return fmt.Errorf("symlink is not allowed in generation: %s", relative)
			}
			target, err := os.Readlink(filePath)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("absolute symlink is not allowed in generation: %s", relative)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(relative), filepath.ToSlash(target)))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink escapes generation: %s", relative)
			}
			writeHashHeader(digest, 'L', relative, info.Mode().Perm(), int64(len(target)))
			digest.Write([]byte(target))
			return nil
		}
		switch {
		case info.IsDir():
			writeHashHeader(digest, 'D', relative, info.Mode().Perm(), 0)
			return nil
		case info.Mode().IsRegular():
			writeHashHeader(digest, 'F', relative, info.Mode().Perm(), info.Size())
			file, err := os.Open(filePath)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(digest, file)
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		default:
			return fmt.Errorf("unsupported generation object: %s", relative)
		}
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func isMutableRuntimePath(path string) bool {
	path = filepath.ToSlash(path)
	return path == "__pycache__" || strings.Contains(path, "/__pycache__/") || strings.HasSuffix(path, "/__pycache__") ||
		strings.HasSuffix(path, ".pyc") || strings.HasSuffix(path, ".pyo") ||
		path == "node_modules/.cache" || strings.Contains(path, "/node_modules/.cache/")
}

func writeHashHeader(digest hash.Hash, kind byte, name string, mode fs.FileMode, size int64) {
	digest.Write([]byte{kind})
	_ = binary.Write(digest, binary.BigEndian, uint32(len(name)))
	digest.Write([]byte(name))
	_ = binary.Write(digest, binary.BigEndian, uint32(mode.Perm()))
	_ = binary.Write(digest, binary.BigEndian, uint64(size))
}

func ensureRoot(root string) error {
	if root == "" {
		return errors.New("activation root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("activation root is not a real directory")
	}
	generations := filepath.Join(root, generationsDir)
	if err := os.MkdirAll(generations, 0o700); err != nil {
		return err
	}
	info, err = os.Lstat(generations)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("generations root is not a real directory")
	}
	return nil
}

func validateGeneration(generation string) error {
	if generation == "" || generation == "." || generation == ".." || len(generation) > 200 {
		return errors.New("invalid generation ID")
	}
	if strings.ContainsRune(generation, 0) || strings.ContainsAny(generation, `/\`) || filepath.Base(generation) != generation {
		return errors.New("generation ID must be one path segment")
	}
	return nil
}

func temporaryPointer(root string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate temporary pointer name: %w", err)
	}
	return filepath.Join(root, ".current-"+hex.EncodeToString(random[:])), nil
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
