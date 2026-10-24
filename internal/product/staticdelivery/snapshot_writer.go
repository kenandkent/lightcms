package staticdelivery

import "io"

type snapshotWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *snapshotWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, failure(CodeAssetLimitExceeded, "private rollback snapshot exceeds persistence budget", nil)
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
