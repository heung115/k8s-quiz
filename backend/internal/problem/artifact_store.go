package problem

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// ArtifactStore persists immutable, content-addressed runtime artifacts. An
// implementation must never replace an object already installed for a digest.
type ArtifactStore interface {
	Ensure(context.Context, ArtifactRef, []byte) error
	Get(context.Context, ArtifactRef) ([]byte, error)
}

// ArtifactStoreErrorKind classifies failures without exposing filesystem
// details to callers that only need a fail-closed policy decision.
type ArtifactStoreErrorKind string

const (
	ArtifactStoreErrorNotFound    ArtifactStoreErrorKind = "not_found"
	ArtifactStoreErrorIntegrity   ArtifactStoreErrorKind = "integrity"
	ArtifactStoreErrorUnavailable ArtifactStoreErrorKind = "unavailable"
	ArtifactStoreErrorConflict    ArtifactStoreErrorKind = "conflict"
)

var (
	ErrArtifactNotFound    = errors.New("artifact not found")
	ErrArtifactIntegrity   = errors.New("artifact integrity violation")
	ErrArtifactUnavailable = errors.New("artifact store unavailable")
	ErrArtifactConflict    = errors.New("artifact reference conflict")
)

// ArtifactStoreError is returned for every storage-specific failure. Context
// cancellation is returned directly so errors.Is continues to work naturally.
type ArtifactStoreError struct {
	Kind ArtifactStoreErrorKind
	Op   string
	Err  error
}

func (e *ArtifactStoreError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("artifact store %s: %s", e.Op, e.Kind)
	}
	return fmt.Sprintf("artifact store %s: %s: %v", e.Op, e.Kind, e.Err)
}

func (e *ArtifactStoreError) Unwrap() error { return e.Err }

func (e *ArtifactStoreError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch target {
	case ErrArtifactNotFound:
		return e.Kind == ArtifactStoreErrorNotFound
	case ErrArtifactIntegrity:
		return e.Kind == ArtifactStoreErrorIntegrity
	case ErrArtifactUnavailable:
		return e.Kind == ArtifactStoreErrorUnavailable
	case ErrArtifactConflict:
		return e.Kind == ArtifactStoreErrorConflict
	default:
		return false
	}
}

func artifactStoreError(kind ArtifactStoreErrorKind, op string, err error) error {
	return &ArtifactStoreError{Kind: kind, Op: op, Err: err}
}

// FilesystemArtifactStore is a local durable CAS. The configured root is a
// trusted boundary; every path below it is traversed with openat and
// O_NOFOLLOW, and is revalidated on every operation.
type FilesystemArtifactStore struct {
	root string

	rootMu sync.RWMutex
	rootFD int
	closed bool
}

const (
	artifactStoreDirMode  = 0o700
	artifactStoreFileMode = 0o600
	artifactStoreMaxBytes = int64(maxRuntimeArtifactBytes)
)

// NewFilesystemArtifactStore creates the root when needed and verifies that
// it is a private directory owned by the effective user. An absolute path is
// required so later process working-directory changes cannot redirect it.
func NewFilesystemArtifactStore(root string) (*FilesystemArtifactStore, error) {
	if root == "" {
		return nil, artifactStoreError(ArtifactStoreErrorConflict, "open root", errors.New("artifact store root is empty"))
	}
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) {
		return nil, artifactStoreError(ArtifactStoreErrorConflict, "open root", errors.New("artifact store root must be absolute"))
	}
	fd, err := createAndOpenArtifactRoot(clean)
	if err != nil {
		return nil, err
	}
	return &FilesystemArtifactStore{root: clean, rootFD: fd}, nil
}

// Close releases the pinned root directory. Operations already holding a
// duplicate continue safely; later operations fail closed. It is idempotent.
func (s *FilesystemArtifactStore) Close() error {
	if s == nil {
		return nil
	}
	s.rootMu.Lock()
	defer s.rootMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.rootFD < 0 {
		return nil
	}
	err := unix.Close(s.rootFD)
	s.rootFD = -1
	if err != nil {
		return artifactStoreError(ArtifactStoreErrorUnavailable, "close root", err)
	}
	return nil
}

// Ensure installs data if absent, or verifies the already-installed object.
// The expected reference is checked before any filesystem mutation.
func (s *FilesystemArtifactStore) Ensure(ctx context.Context, ref ArtifactRef, data []byte) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateArtifactRef(ref); err != nil {
		return artifactStoreError(ArtifactStoreErrorConflict, "validate reference", err)
	}
	if int64(len(data)) != ref.Size {
		return artifactStoreError(ArtifactStoreErrorConflict, "validate input", fmt.Errorf("size is %d, expected %d", len(data), ref.Size))
	}
	if err := validateRuntimeArtifactRef(ref, data); err != nil {
		return artifactStoreError(ArtifactStoreErrorConflict, "validate input", err)
	}

	rootFD, err := s.openRoot()
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)

	dirFD, name, err := openArtifactObjectDir(ctx, rootFD, ref, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)

	if _, err := readArtifactObject(ctx, dirFD, name, ref); err == nil {
		return syncArtifactObjectDir(dirFD)
	} else if !errors.Is(err, ErrArtifactNotFound) {
		return err
	}

	tempName, tempFD, err := createArtifactTemp(ctx, dirFD)
	if err != nil {
		return err
	}
	tempPresent := true
	defer func() {
		if tempFD >= 0 {
			if err := unix.Close(tempFD); err != nil {
				resultErr = errors.Join(resultErr, artifactStoreError(ArtifactStoreErrorUnavailable, "close temporary object", err))
			}
		}
		if tempPresent {
			resultErr = errors.Join(resultErr, removeArtifactTemp(dirFD, tempName))
		}
	}()

	if err := writeArtifactTemp(ctx, tempFD, data); err != nil {
		return err
	}
	if err := unix.Close(tempFD); err != nil {
		tempFD = -1
		return artifactStoreError(ArtifactStoreErrorUnavailable, "close temporary object", err)
	}
	tempFD = -1

	if err := installArtifactNoReplace(dirFD, tempName, name); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return artifactStoreError(ArtifactStoreErrorUnavailable, "install object", err)
		}
		// Another writer won. Its object is acceptable only if it is exactly
		// the immutable object expected by this caller.
		if _, verifyErr := readArtifactObject(ctx, dirFD, name, ref); verifyErr != nil {
			return verifyErr
		}
		return syncArtifactObjectDir(dirFD)
	}
	tempPresent = false

	if err := syncArtifactObjectDir(dirFD); err != nil {
		return err
	}
	if _, err := readArtifactObject(ctx, dirFD, name, ref); err != nil {
		return err
	}
	return nil
}

func syncArtifactObjectDir(dirFD int) error {
	if err := unix.Fsync(dirFD); err != nil {
		return artifactStoreError(ArtifactStoreErrorUnavailable, "sync object directory", err)
	}
	return nil
}

func removeArtifactTemp(dirFD int, name string) error {
	if err := unix.Unlinkat(dirFD, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return artifactStoreError(ArtifactStoreErrorUnavailable, "remove temporary object", err)
	}
	if err := unix.Fsync(dirFD); err != nil {
		return artifactStoreError(ArtifactStoreErrorUnavailable, "sync temporary object removal", err)
	}
	return nil
}

// Get returns an independently owned byte slice after verifying the complete
// reference and the on-disk object.
func (s *FilesystemArtifactStore) Get(ctx context.Context, ref ArtifactRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateArtifactRef(ref); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorConflict, "validate reference", err)
	}

	rootFD, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)

	dirFD, name, err := openArtifactObjectDir(ctx, rootFD, ref, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)
	return readArtifactObject(ctx, dirFD, name, ref)
}

func (s *FilesystemArtifactStore) openRoot() (int, error) {
	if s == nil {
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "open root", errors.New("artifact store is nil"))
	}
	s.rootMu.RLock()
	defer s.rootMu.RUnlock()
	if s.closed || s.rootFD < 0 {
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "open root", errors.New("artifact store is closed"))
	}
	fd, err := unix.Dup(s.rootFD)
	if err != nil {
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "duplicate root", err)
	}
	unix.CloseOnExec(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "inspect root", err)
	}
	if err := validateArtifactDirStat(&stat); err != nil {
		_ = unix.Close(fd)
		return -1, artifactStoreError(ArtifactStoreErrorIntegrity, "inspect root", err)
	}
	return fd, nil
}

func createAndOpenArtifactRoot(root string) (int, error) {
	currentFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "open root anchor", err)
	}
	segments := strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator))
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			_ = unix.Close(currentFD)
			return -1, artifactStoreError(ArtifactStoreErrorConflict, "open root", errors.New("artifact store root contains an invalid segment"))
		}
		nextFD, openErr := unix.Openat(currentFD, segment, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(currentFD, segment, artifactStoreDirMode); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(currentFD)
				return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "create root component", mkdirErr)
			}
			if syncErr := unix.Fsync(currentFD); syncErr != nil {
				_ = unix.Close(currentFD)
				return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "sync root component parent", syncErr)
			}
			nextFD, openErr = unix.Openat(currentFD, segment, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			_ = unix.Close(currentFD)
			kind := ArtifactStoreErrorUnavailable
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
				kind = ArtifactStoreErrorIntegrity
			}
			return -1, artifactStoreError(kind, "open root component", openErr)
		}
		if closeErr := unix.Close(currentFD); closeErr != nil {
			_ = unix.Close(nextFD)
			return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "close root component", closeErr)
		}
		currentFD = nextFD
	}
	var stat unix.Stat_t
	if err := unix.Fstat(currentFD, &stat); err != nil {
		_ = unix.Close(currentFD)
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "inspect root", err)
	}
	if err := validateArtifactDirStat(&stat); err != nil {
		_ = unix.Close(currentFD)
		return -1, artifactStoreError(ArtifactStoreErrorIntegrity, "inspect root", err)
	}
	return currentFD, nil
}

func validateArtifactRef(ref ArtifactRef) error {
	if ref.DigestSchema != ArtifactDigestSchemaV1 {
		return fmt.Errorf("unsupported digest schema %d", ref.DigestSchema)
	}
	if ref.MediaType != RuntimeArtifactMediaTypeV1 {
		return fmt.Errorf("unsupported media type %q", ref.MediaType)
	}
	if ref.Size <= 0 || ref.Size > artifactStoreMaxBytes {
		return fmt.Errorf("artifact size %d is outside (0, %d]", ref.Size, artifactStoreMaxBytes)
	}
	var zero [sha256.Size]byte
	if ref.Digest == zero {
		return errors.New("artifact digest is empty")
	}
	return nil
}

func openArtifactObjectDir(ctx context.Context, rootFD int, ref ArtifactRef, create bool) (int, string, error) {
	if err := ctx.Err(); err != nil {
		return -1, "", err
	}
	digestHex := hex.EncodeToString(ref.Digest[:])
	segments := []string{"objects", "sha256", digestHex[:2]}
	parentFD := rootFD
	ownedParent := false
	for _, segment := range segments {
		if err := ctx.Err(); err != nil {
			if ownedParent {
				_ = unix.Close(parentFD)
			}
			return -1, "", err
		}
		var nextFD int
		var err error
		if create {
			nextFD, err = ensureArtifactDirAt(parentFD, segment)
		} else {
			nextFD, err = openArtifactDirAt(parentFD, segment)
		}
		if ownedParent {
			_ = unix.Close(parentFD)
		}
		if err != nil {
			if !create && errors.Is(err, ErrArtifactNotFound) {
				return -1, "", artifactStoreError(ArtifactStoreErrorNotFound, "open object", unix.ENOENT)
			}
			return -1, "", err
		}
		parentFD = nextFD
		ownedParent = true
	}
	return parentFD, digestHex[2:], nil
}

func ensureArtifactDirAt(parentFD int, name string) (int, error) {
	if err := unix.Mkdirat(parentFD, name, artifactStoreDirMode); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "create object directory", err)
		}
	} else if err := unix.Fsync(parentFD); err != nil {
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "sync object directory parent", err)
	}
	return openArtifactDirAt(parentFD, name)
}

func openArtifactDirAt(parentFD int, name string) (int, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return -1, artifactStoreError(ArtifactStoreErrorConflict, "open object directory", errors.New("invalid directory segment"))
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return -1, artifactStoreError(ArtifactStoreErrorNotFound, "open object directory", err)
		}
		kind := ArtifactStoreErrorUnavailable
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			kind = ArtifactStoreErrorIntegrity
		}
		return -1, artifactStoreError(kind, "open object directory", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, artifactStoreError(ArtifactStoreErrorUnavailable, "inspect object directory", err)
	}
	if err := validateArtifactDirStat(&stat); err != nil {
		_ = unix.Close(fd)
		return -1, artifactStoreError(ArtifactStoreErrorIntegrity, "inspect object directory", err)
	}
	return fd, nil
}

func validateArtifactDirStat(stat *unix.Stat_t) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("path is not a directory")
	}
	if stat.Uid != uint32(unix.Geteuid()) {
		return fmt.Errorf("directory owner is uid %d, expected %d", stat.Uid, unix.Geteuid())
	}
	if stat.Mode&0o7777 != artifactStoreDirMode {
		return fmt.Errorf("directory mode is %#o, expected %#o", stat.Mode&0o7777, artifactStoreDirMode)
	}
	return nil
}

func validateArtifactObjectStat(stat *unix.Stat_t, expectedSize int64) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("object is not a regular file")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("object link count is %d, expected 1", stat.Nlink)
	}
	if stat.Uid != uint32(unix.Geteuid()) {
		return fmt.Errorf("object owner is uid %d, expected %d", stat.Uid, unix.Geteuid())
	}
	if stat.Mode&0o7777 != artifactStoreFileMode {
		return fmt.Errorf("object mode is %#o, expected %#o", stat.Mode&0o7777, artifactStoreFileMode)
	}
	if stat.Size != expectedSize {
		return fmt.Errorf("object size is %d, expected %d", stat.Size, expectedSize)
	}
	return nil
}

func createArtifactTemp(ctx context.Context, dirFD int) (string, int, error) {
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", -1, err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", -1, artifactStoreError(ArtifactStoreErrorUnavailable, "name temporary object", err)
		}
		name := ".tmp-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, artifactStoreFileMode)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", -1, artifactStoreError(ArtifactStoreErrorUnavailable, "create temporary object", err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(dirFD, name, 0)
			return "", -1, artifactStoreError(ArtifactStoreErrorUnavailable, "inspect temporary object", err)
		}
		if err := validateArtifactObjectStat(&stat, 0); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(dirFD, name, 0)
			return "", -1, artifactStoreError(ArtifactStoreErrorIntegrity, "inspect temporary object", err)
		}
		return name, fd, nil
	}
	return "", -1, artifactStoreError(ArtifactStoreErrorUnavailable, "create temporary object", errors.New("temporary name collision limit reached"))
}

func writeArtifactTemp(ctx context.Context, fd int, data []byte) error {
	remaining := data
	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := unix.Write(fd, remaining)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return artifactStoreError(ArtifactStoreErrorUnavailable, "write temporary object", err)
		}
		if written <= 0 {
			return artifactStoreError(ArtifactStoreErrorUnavailable, "write temporary object", errors.New("short write"))
		}
		remaining = remaining[written:]
	}
	if err := unix.Fsync(fd); err != nil {
		return artifactStoreError(ArtifactStoreErrorUnavailable, "sync temporary object", err)
	}
	return nil
}

func readArtifactObject(ctx context.Context, dirFD int, name string, ref ArtifactRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var namedBefore unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &namedBefore, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, artifactStoreError(ArtifactStoreErrorNotFound, "inspect object path", err)
		}
		return nil, artifactStoreError(ArtifactStoreErrorUnavailable, "inspect object path", err)
	}
	if err := validateArtifactObjectStat(&namedBefore, ref.Size); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "inspect object path", err)
	}
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, artifactStoreError(ArtifactStoreErrorNotFound, "open object", err)
		}
		kind := ArtifactStoreErrorUnavailable
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			kind = ArtifactStoreErrorIntegrity
		}
		return nil, artifactStoreError(kind, "open object", err)
	}
	defer unix.Close(fd)

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorUnavailable, "inspect object", err)
	}
	if err := validateArtifactObjectStat(&before, ref.Size); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "inspect object", err)
	}
	if namedBefore.Dev != before.Dev || namedBefore.Ino != before.Ino {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "inspect object", errors.New("object path changed while opening"))
	}

	data := make([]byte, 0, int(ref.Size))
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, readErr := unix.Read(fd, buffer)
		if errors.Is(readErr, unix.EINTR) {
			continue
		}
		if read > 0 {
			if int64(len(data))+int64(read) > ref.Size {
				return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "read object", errors.New("object exceeds referenced size"))
			}
			data = append(data, buffer[:read]...)
		}
		if readErr != nil {
			return nil, artifactStoreError(ArtifactStoreErrorUnavailable, "read object", readErr)
		}
		if read == 0 {
			break
		}
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorUnavailable, "reinspect object", err)
	}
	if err := validateArtifactObjectStat(&after, ref.Size); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "reinspect object", err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Nlink != after.Nlink || before.Uid != after.Uid || before.Size != after.Size {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "reinspect object", errors.New("object metadata changed while reading"))
	}
	if int64(len(data)) != ref.Size {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "read object", fmt.Errorf("read %d bytes, expected %d", len(data), ref.Size))
	}
	if err := validateRuntimeArtifactRef(ref, data); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "verify object", err)
	}

	var named unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "reinspect object path", err)
	}
	if err := validateArtifactObjectStat(&named, ref.Size); err != nil {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "reinspect object path", err)
	}
	if named.Dev != after.Dev || named.Ino != after.Ino {
		return nil, artifactStoreError(ArtifactStoreErrorIntegrity, "reinspect object path", errors.New("object path changed while reading"))
	}
	return data, nil
}
