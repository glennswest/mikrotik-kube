package registry

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// BlobStore provides on-disk storage for OCI blobs and manifests.
// Directory structure:
//
//	<root>/
//	  blobs/
//	    sha256/
//	      <hex digest>          — raw blob data
//	  manifests/
//	    <repo>/
//	      <tag or digest>.json  — manifest data
//	      <tag or digest>.type  — content-type metadata
type BlobStore struct {
	root string
	mu   sync.RWMutex
}

// NewBlobStore creates a new on-disk blob store at the given root directory.
func NewBlobStore(root string) (*BlobStore, error) {
	for _, dir := range []string{
		filepath.Join(root, "blobs", "sha256"),
		filepath.Join(root, "manifests"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating store directory %s: %w", dir, err)
		}
	}
	return &BlobStore{root: root}, nil
}

// GetBlob returns the raw data for a blob by its digest (e.g. "sha256:abc123").
func (s *BlobStore) GetBlob(digest string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.blobPath(digest)
	return os.ReadFile(path)
}

// HasBlob checks whether a blob exists and returns its size.
func (s *BlobStore) HasBlob(digest string) (exists bool, size int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	info, err := os.Stat(s.blobPath(digest))
	if err != nil {
		return false, 0
	}
	return true, info.Size()
}

// PutBlob stores blob data from a reader, keyed by digest.
func (s *BlobStore) PutBlob(digest string, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, r)
	return err
}

// GetManifest returns the manifest data and content type for a repo/reference.
func (s *BlobStore) GetManifest(repo, ref string) (data []byte, contentType string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	dataPath := s.manifestPath(repo, ref)
	typePath := dataPath + ".type"

	data, err = os.ReadFile(dataPath)
	if err != nil {
		return nil, "", err
	}

	typeBytes, err := os.ReadFile(typePath)
	if err != nil {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	} else {
		contentType = string(typeBytes)
	}

	return data, contentType, nil
}

// PutManifest stores a manifest for a repo/reference with its content type.
func (s *BlobStore) PutManifest(repo, ref, contentType string, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dataPath := s.manifestPath(repo, ref)
	if err := os.MkdirAll(filepath.Dir(dataPath), 0755); err != nil {
		return err
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	if err := os.WriteFile(dataPath, data, 0644); err != nil {
		return err
	}

	if contentType != "" {
		if err := os.WriteFile(dataPath+".type", []byte(contentType), 0644); err != nil {
			return err
		}
	}

	return nil
}

// ListRepositories returns all repository names that have stored manifests.
func (s *BlobStore) ListRepositories() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	manifestsDir := filepath.Join(s.root, "manifests")
	var repos []string

	filepath.Walk(manifestsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && !strings.HasSuffix(info.Name(), ".type") {
			rel, err := filepath.Rel(manifestsDir, filepath.Dir(path))
			if err == nil && rel != "." {
				repos = appendUnique(repos, rel)
			}
		}
		return nil
	})

	sort.Strings(repos)
	return repos
}

func (s *BlobStore) blobPath(digest string) string {
	// digest format: "sha256:hexhexhex..."
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return filepath.Join(s.root, "blobs", "sha256", digest)
	}
	return filepath.Join(s.root, "blobs", parts[0], parts[1])
}

func (s *BlobStore) manifestPath(repo, ref string) string {
	// Sanitize ref for filesystem (colons in digests)
	safe := strings.ReplaceAll(ref, ":", "-")
	return filepath.Join(s.root, "manifests", repo, safe+".json")
}

func appendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}
