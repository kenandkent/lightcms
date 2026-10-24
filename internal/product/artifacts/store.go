// Package artifacts stores immutable private objects by content hash. It is
// deliberately not an HTTP filesystem and never resolves a caller's raw path.
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

var ErrCorrupt = errors.New("artifact object failed integrity verification")
var ErrLimit = errors.New("artifact capacity limit exceeded")

type Ref struct {
	Hash string `bson:"hash" json:"hash"`
	Size int64  `bson:"size" json:"size"`
	MIME string `bson:"mime" json:"mime"`
}
type Store struct{ root *os.Root }

// NewRootStore takes ownership of an already confined private directory.
func NewRootStore(root *os.Root) *Store { return &Store{root: root} }

func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Store{root}, nil
}
func (s *Store) Close() error { return s.root.Close() }
func objectPath(hash string) (string, error) {
	if len(hash) != 64 || strings.ToLower(hash) != hash {
		return "", fmt.Errorf("invalid object hash")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("invalid object hash")
	}
	return "objects/" + hash[:2] + "/" + hash, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func (s *Store) Put(ctx context.Context, reader io.Reader, mime string, limit int64) (Ref, error) {
	var ref Ref
	if limit < 1 || limit > 512<<20 {
		return ref, ErrLimit
	}
	if mime == "" {
		return ref, fmt.Errorf("object MIME is required")
	}
	if err := s.root.MkdirAll("objects/tmp", 0700); err != nil {
		return ref, err
	}
	temp := "objects/tmp/" + primitive.NewObjectID().Hex()
	file, err := s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ref, err
	}
	defer s.root.Remove(temp)
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, reader}, limit+1))
	if err != nil {
		return ref, err
	}
	if size > limit {
		return ref, ErrLimit
	}
	ref = Ref{Hash: hex.EncodeToString(hash.Sum(nil)), Size: size, MIME: mime}
	unlock, err := s.lockObject(ctx, ref.Hash)
	if err != nil {
		return Ref{}, err
	}
	defer unlock()
	target, _ := objectPath(ref.Hash)
	if err := file.Sync(); err != nil {
		return Ref{}, err
	}
	if err := file.Close(); err != nil {
		return Ref{}, err
	}
	if err := s.root.MkdirAll("objects/"+ref.Hash[:2], 0700); err != nil {
		return Ref{}, err
	}
	if _, err := s.root.Lstat(target); err == nil {
		existing, err := s.Open(ctx, ref)
		if err != nil {
			return Ref{}, err
		}
		existing.Close()
		return ref, nil
	} else if !os.IsNotExist(err) {
		return Ref{}, err
	}
	if err := s.root.Rename(temp, target); err != nil {
		return Ref{}, err
	}
	dir, err := s.root.Open("objects/" + ref.Hash[:2])
	if err != nil {
		return Ref{}, err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}
func (s *Store) Open(ctx context.Context, ref Ref) (io.ReadCloser, error) {
	path, err := objectPath(ref.Hash)
	if err != nil {
		return nil, err
	}
	file, err := s.root.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) { file.Close(); return nil, err }
	stat, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !stat.Mode().IsRegular() || stat.Size() != ref.Size {
		return fail(ErrCorrupt)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, file}); err != nil {
		return fail(err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != ref.Hash {
		return fail(ErrCorrupt)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return file, nil
}
func (s *Store) Stat(ctx context.Context, ref Ref) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := objectPath(ref.Hash)
	if err != nil {
		return nil, err
	}
	return s.root.Stat(path)
}

// Remove is used only after Catalog confirms an unreferenced deleting object.
func (s *Store) Remove(hash string) error {
	path, err := objectPath(hash)
	if err != nil {
		return err
	}
	err = s.root.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Admission accounts for simultaneous build memory, not merely goroutine count.
type Admission struct {
	mu                sync.Mutex
	limit, used, peak int64
	changed           chan struct{}
	waiters           int64
	waitNanos         int64
}

func NewAdmission(limit int64) *Admission {
	return &Admission{limit: limit, changed: make(chan struct{})}
}
func (a *Admission) Acquire(ctx context.Context, n int64) (func(), error) {
	if n <= 0 || n > a.limit {
		return nil, ErrLimit
	}
	var waitStart time.Time
	defer func() {
		if !waitStart.IsZero() {
			a.mu.Lock()
			a.waiters--
			a.waitNanos += time.Since(waitStart).Nanoseconds()
			a.mu.Unlock()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a.mu.Lock()
		if a.used+n <= a.limit {
			a.used += n
			if a.used > a.peak {
				a.peak = a.used
			}
			a.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					a.mu.Lock()
					defer a.mu.Unlock()
					a.used -= n
					close(a.changed)
					a.changed = make(chan struct{})
				})
			}, nil
		}
		changed := a.changed
		if waitStart.IsZero() {
			waitStart = time.Now()
			a.waiters++
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
func (a *Admission) Peak() int64 { a.mu.Lock(); defer a.mu.Unlock(); return a.peak }

type AdmissionStats struct{ Limit, Used, Peak, Waiters, WaitNanos int64 }

func (a *Admission) Stats() AdmissionStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AdmissionStats{Limit: a.limit, Used: a.used, Peak: a.peak, Waiters: a.waiters, WaitNanos: a.waitNanos}
}
