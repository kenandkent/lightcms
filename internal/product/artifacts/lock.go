package artifacts

import (
	"context"
	"errors"
	"os"
	"time"
)

var errLockBusy = errors.New("artifact lifecycle lock busy")

// Keep lock files permanently: unlinking an inode while another process waits
// on it would create two independent locks for the same content hash.
func (s *Store) lockObject(ctx context.Context, hash string) (func(), error) {
	if _, err := objectPath(hash); err != nil {
		return nil, err
	}
	if err := s.root.MkdirAll("objects/locks", 0700); err != nil {
		return nil, err
	}
	file, err := s.root.OpenFile("objects/locks/"+hash+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err := tryExclusive(file)
		if err == nil {
			return func() { unlockFile(file); file.Close() }, nil
		}
		if !errors.Is(err, errLockBusy) {
			file.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
