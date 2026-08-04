//go:build linux || darwin

package problem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFilesystemArtifactStoreRoundTrip(t *testing.T) {
	t.Parallel()

	root := filepath.Join(canonicalTempDir(t), "artifact-store")
	store, err := NewFilesystemArtifactStore(root)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactStore: %v", err)
	}
	data := []byte("canonical runtime artifact")
	ref := testArtifactRef(data)

	if err := store.Ensure(context.Background(), ref, data); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Get() = %q, want %q", got, data)
	}

	got[0] ^= 0xff
	again, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if !bytes.Equal(again, data) {
		t.Fatalf("Get returned storage-backed bytes: got %q, want %q", again, data)
	}

	assertArtifactStoreModes(t, root, ref)
	assertNoArtifactTemps(t, root)
}

func TestFilesystemArtifactStorePinsRootAcrossPathReplacement(t *testing.T) {
	t.Parallel()

	parent := canonicalTempDir(t)
	root := filepath.Join(parent, "artifact-store")
	store, err := NewFilesystemArtifactStore(root)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	pinned := filepath.Join(parent, "pinned-artifact-store")
	if err := os.Rename(root, pinned); err != nil {
		t.Fatalf("rename pinned root: %v", err)
	}
	if err := os.Mkdir(root, artifactStoreDirMode); err != nil {
		t.Fatalf("create replacement root: %v", err)
	}
	data := []byte("root-pinned artifact")
	ref := testArtifactRef(data)
	if err := store.Ensure(context.Background(), ref, data); err != nil {
		t.Fatalf("Ensure through pinned root: %v", err)
	}
	if _, err := os.Stat(testArtifactPath(pinned, ref)); err != nil {
		t.Fatalf("artifact was not written through pinned root: %v", err)
	}
	if _, err := os.Stat(testArtifactPath(root, ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact was redirected to replacement root: %v", err)
	}
}

func TestFilesystemArtifactStoreCloseIsIdempotentAndFailsClosed(t *testing.T) {
	t.Parallel()

	store, err := NewFilesystemArtifactStore(filepath.Join(canonicalTempDir(t), "artifact-store"))
	if err != nil {
		t.Fatalf("NewFilesystemArtifactStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	data := []byte("closed artifact store")
	ref := testArtifactRef(data)
	if err := store.Ensure(context.Background(), ref, data); !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("Ensure after Close error = %v, want ErrArtifactUnavailable", err)
	}
	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("Get after Close error = %v, want ErrArtifactUnavailable", err)
	}
}

func TestFilesystemArtifactStoreConcurrentEnsureIsIdempotent(t *testing.T) {
	t.Parallel()

	root := filepath.Join(canonicalTempDir(t), "artifact-store")
	store, err := NewFilesystemArtifactStore(root)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactStore: %v", err)
	}
	data := bytes.Repeat([]byte("concurrent artifact"), 4096)
	ref := testArtifactRef(data)

	const writers = 32
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- store.Ensure(context.Background(), ref, data)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Ensure: %v", err)
		}
	}
	if t.Failed() {
		return
	}

	got, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Get() length = %d, content differs", len(got))
	}
	assertNoArtifactTemps(t, root)
}

func TestFilesystemArtifactStoreRejectsReferenceAndInputConflicts(t *testing.T) {
	t.Parallel()

	store, err := NewFilesystemArtifactStore(filepath.Join(canonicalTempDir(t), "artifact-store"))
	if err != nil {
		t.Fatalf("NewFilesystemArtifactStore: %v", err)
	}
	data := []byte("artifact")
	valid := testArtifactRef(data)
	zeroDigest := valid
	zeroDigest.Digest = [sha256.Size]byte{}

	tests := []struct {
		name string
		ref  ArtifactRef
		data []byte
	}{
		{name: "digest schema", ref: func() ArtifactRef { r := valid; r.DigestSchema++; return r }(), data: data},
		{name: "media type", ref: func() ArtifactRef { r := valid; r.MediaType += "+unknown"; return r }(), data: data},
		{name: "zero digest", ref: zeroDigest, data: data},
		{name: "zero size", ref: func() ArtifactRef { r := valid; r.Size = 0; return r }(), data: nil},
		{name: "oversize", ref: func() ArtifactRef { r := valid; r.Size = artifactStoreMaxBytes + 1; return r }(), data: data},
		{name: "input size", ref: valid, data: append(append([]byte(nil), data...), '!')},
		{name: "input digest", ref: valid, data: []byte("ARTIFACT")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.Ensure(context.Background(), tt.ref, tt.data); !errors.Is(err, ErrArtifactConflict) {
				t.Fatalf("Ensure error = %v, want ErrArtifactConflict", err)
			}
		})
	}

	if _, err := store.Get(context.Background(), zeroDigest); !errors.Is(err, ErrArtifactConflict) {
		t.Fatalf("Get invalid reference error = %v, want ErrArtifactConflict", err)
	}
	if _, err := store.Get(context.Background(), valid); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("Get absent artifact error = %v, want ErrArtifactNotFound", err)
	}
}

func TestFilesystemArtifactStoreRejectsCorruptObjectsWithoutReplacement(t *testing.T) {
	t.Parallel()

	mutations := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "same-size corruption",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("ReadFile: %v", err)
				}
				data[len(data)/2] ^= 0xff
				if err := os.WriteFile(path, data, artifactStoreFileMode); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
		},
		{
			name: "truncation",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Truncate(path, 1); err != nil {
					t.Fatalf("Truncate: %v", err)
				}
			},
		},
		{
			name: "append",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatalf("OpenFile: %v", err)
				}
				if _, err := file.Write([]byte("appended")); err != nil {
					_ = file.Close()
					t.Fatalf("Write: %v", err)
				}
				if err := file.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			},
		},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(canonicalTempDir(t), "artifact-store")
			store, err := NewFilesystemArtifactStore(root)
			if err != nil {
				t.Fatalf("NewFilesystemArtifactStore: %v", err)
			}
			data := []byte("immutable artifact content")
			ref := testArtifactRef(data)
			if err := store.Ensure(context.Background(), ref, data); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			path := testArtifactPath(root, ref)
			mutation.mutate(t, path)

			if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrArtifactIntegrity) {
				t.Fatalf("Get corrupt object error = %v, want ErrArtifactIntegrity", err)
			}
			if err := store.Ensure(context.Background(), ref, data); !errors.Is(err, ErrArtifactIntegrity) {
				t.Fatalf("Ensure corrupt object error = %v, want ErrArtifactIntegrity", err)
			}
			assertNoArtifactTemps(t, root)
		})
	}
}

func TestFilesystemArtifactStoreRejectsObjectSubstitution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		create func(*testing.T, string, []byte)
	}{
		{
			name: "symlink",
			create: func(t *testing.T, path string, data []byte) {
				t.Helper()
				target := filepath.Join(canonicalTempDir(t), "target")
				if err := os.WriteFile(target, data, artifactStoreFileMode); err != nil {
					t.Fatalf("WriteFile target: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
			},
		},
		{
			name: "fifo",
			create: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				if err := unix.Mkfifo(path, artifactStoreFileMode); err != nil {
					t.Fatalf("Mkfifo: %v", err)
				}
			},
		},
		{
			name: "directory",
			create: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				if err := os.Mkdir(path, artifactStoreDirMode); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(canonicalTempDir(t), "artifact-store")
			store, err := NewFilesystemArtifactStore(root)
			if err != nil {
				t.Fatalf("NewFilesystemArtifactStore: %v", err)
			}
			data := []byte("expected object")
			ref := testArtifactRef(data)
			path := testArtifactPath(root, ref)
			if err := os.MkdirAll(filepath.Dir(path), artifactStoreDirMode); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			tt.create(t, path, data)

			if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrArtifactIntegrity) {
				t.Fatalf("Get substituted object error = %v, want ErrArtifactIntegrity", err)
			}
			if err := store.Ensure(context.Background(), ref, data); !errors.Is(err, ErrArtifactIntegrity) {
				t.Fatalf("Ensure substituted object error = %v, want ErrArtifactIntegrity", err)
			}
			assertNoArtifactTemps(t, root)
		})
	}
}

func TestFilesystemArtifactStoreRejectsHardLinkedObject(t *testing.T) {
	t.Parallel()

	root := filepath.Join(canonicalTempDir(t), "artifact-store")
	store, err := NewFilesystemArtifactStore(root)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactStore: %v", err)
	}
	data := []byte("hardlink-sensitive artifact")
	ref := testArtifactRef(data)
	if err := store.Ensure(context.Background(), ref, data); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	path := testArtifactPath(root, ref)
	if err := os.Link(path, path+".extra-link"); err != nil {
		t.Fatalf("Link: %v", err)
	}

	if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("Get hardlinked object error = %v, want ErrArtifactIntegrity", err)
	}
	if err := store.Ensure(context.Background(), ref, data); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("Ensure hardlinked object error = %v, want ErrArtifactIntegrity", err)
	}
}

func TestFilesystemArtifactStoreRejectsDirectorySubstitutionAndUnsafeModes(t *testing.T) {
	t.Parallel()

	t.Run("relative root", func(t *testing.T) {
		if _, err := NewFilesystemArtifactStore("relative/store"); !errors.Is(err, ErrArtifactConflict) {
			t.Fatalf("error = %v, want ErrArtifactConflict", err)
		}
	})

	t.Run("permissive root", func(t *testing.T) {
		root := filepath.Join(canonicalTempDir(t), "store")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if _, err := NewFilesystemArtifactStore(root); !errors.Is(err, ErrArtifactIntegrity) {
			t.Fatalf("error = %v, want ErrArtifactIntegrity", err)
		}
	})

	t.Run("symlink object directory", func(t *testing.T) {
		root := filepath.Join(canonicalTempDir(t), "store")
		store, err := NewFilesystemArtifactStore(root)
		if err != nil {
			t.Fatalf("NewFilesystemArtifactStore: %v", err)
		}
		target := filepath.Join(canonicalTempDir(t), "elsewhere")
		if err := os.Mkdir(target, artifactStoreDirMode); err != nil {
			t.Fatalf("Mkdir target: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(root, "objects")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		data := []byte("artifact")
		ref := testArtifactRef(data)
		if _, err := store.Get(context.Background(), ref); !errors.Is(err, ErrArtifactIntegrity) {
			t.Fatalf("Get error = %v, want ErrArtifactIntegrity", err)
		}
		if err := store.Ensure(context.Background(), ref, data); !errors.Is(err, ErrArtifactIntegrity) {
			t.Fatalf("Ensure error = %v, want ErrArtifactIntegrity", err)
		}
	})

	t.Run("symlink intermediate root component", func(t *testing.T) {
		parent := canonicalTempDir(t)
		target := filepath.Join(parent, "target")
		if err := os.Mkdir(target, artifactStoreDirMode); err != nil {
			t.Fatalf("Mkdir target: %v", err)
		}
		alias := filepath.Join(parent, "alias")
		if err := os.Symlink(target, alias); err != nil {
			t.Fatalf("Symlink intermediate root: %v", err)
		}
		if _, err := NewFilesystemArtifactStore(filepath.Join(alias, "store")); !errors.Is(err, ErrArtifactIntegrity) {
			t.Fatalf("error = %v, want ErrArtifactIntegrity", err)
		}
	})
}

func TestFilesystemArtifactStoreHonorsCanceledContextWithoutPublishing(t *testing.T) {
	t.Parallel()

	root := filepath.Join(canonicalTempDir(t), "artifact-store")
	store, err := NewFilesystemArtifactStore(root)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactStore: %v", err)
	}
	data := []byte("artifact")
	ref := testArtifactRef(data)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Ensure(ctx, ref, data); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure error = %v, want context.Canceled", err)
	}
	if _, err := store.Get(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(testArtifactPath(root, ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled Ensure published object: %v", err)
	}
	assertNoArtifactTemps(t, root)
}

func testArtifactRef(data []byte) ArtifactRef {
	return runtimeArtifactRef(data)
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return dir
}

func testArtifactPath(root string, ref ArtifactRef) string {
	digest := fmt.Sprintf("%x", ref.Digest)
	return filepath.Join(root, "objects", "sha256", digest[:2], digest[2:])
}

func assertNoArtifactTemps(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if len(entry.Name()) >= len(".tmp-") && entry.Name()[:len(".tmp-")] == ".tmp-" {
			t.Errorf("temporary artifact remains at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}

func assertArtifactStoreModes(t *testing.T, root string, ref ArtifactRef) {
	t.Helper()
	paths := []string{
		root,
		filepath.Join(root, "objects"),
		filepath.Join(root, "objects", "sha256"),
		filepath.Dir(testArtifactPath(root, ref)),
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if got := info.Mode().Perm(); got != artifactStoreDirMode {
			t.Errorf("directory %s mode = %#o, want %#o", path, got, artifactStoreDirMode)
		}
	}
	info, err := os.Stat(testArtifactPath(root, ref))
	if err != nil {
		t.Fatalf("Stat object: %v", err)
	}
	if got := info.Mode().Perm(); got != artifactStoreFileMode {
		t.Errorf("object mode = %#o, want %#o", got, artifactStoreFileMode)
	}
}
