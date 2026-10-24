package cloudflare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"path"
	"strings"
)

type File struct {
	Path, MIME string
	Data       []byte
}
type CompletionToken string

func (CompletionToken) MarshalJSON() ([]byte, error) {
	return nil, errors.New("completion token is private")
}
func AssetHash(data []byte, ext string) string {
	h := sha256.New()
	enc := base64.NewEncoder(base64.StdEncoding, h)
	_, _ = enc.Write(data)
	_ = enc.Close()
	_, _ = io.WriteString(h, ext)
	return hex.EncodeToString(h.Sum(nil))[:32]
}
func (c *Client) UploadAssets(ctx context.Context, account, worker string, files []File) (CompletionToken, error) {
	p, e := scriptPath(account, worker)
	if e != nil {
		return "", e
	}
	type entry struct {
		Hash string `json:"hash"`
		Size int    `json:"size"`
	}
	manifest := map[string]entry{}
	byHash := map[string]File{}
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "/") || path.Clean(f.Path) != f.Path || strings.ContainsAny(f.Path, "\\?#\x00") || len(f.Data) > 25<<20 {
			return "", errors.New("invalid static asset")
		}
		if _, ok := manifest[f.Path]; ok {
			return "", errors.New("duplicate static asset path")
		}
		if f.MIME == "" {
			return "", errors.New("asset MIME required")
		}
		h := AssetHash(f.Data, strings.TrimPrefix(path.Ext(f.Path), "."))
		manifest[f.Path] = entry{h, len(f.Data)}
		if prev, ok := byHash[h]; ok && (!bytes.Equal(prev.Data, f.Data) || prev.MIME != f.MIME) {
			return "", errors.New("asset hash or MIME collision")
		}
		byHash[h] = f
	}
	raw, _ := json.Marshal(map[string]any{"manifest": manifest})
	var session struct {
		JWT     string     `json:"jwt"`
		Buckets [][]string `json:"buckets"`
	}
	if e = c.request(ctx, "POST", p+"/assets-upload-session", c.token, "application/json", raw, &session, false, false); e != nil {
		return "", e
	}
	if session.JWT == "" {
		return "", errors.New("missing asset session token")
	}
	if len(session.Buckets) == 0 {
		return CompletionToken(session.JWT), nil
	}
	uploadPath, _ := accountPath(account)
	var completion string
	seen := map[string]bool{}
	for _, bucket := range session.Buckets {
		if len(bucket) == 0 {
			return "", errors.New("empty asset upload bucket")
		}
		for _, h := range bucket {
			_, ok := byHash[h]
			if !ok || seen[h] {
				return "", errors.New("invalid requested asset hash")
			}
			seen[h] = true
		}
		boundary := multipart.NewWriter(io.Discard).Boundary()
		body := func() io.ReadCloser {
			reader, writer := io.Pipe()
			go func() { writer.CloseWithError(writeAssetBucket(writer, boundary, bucket, byHash)) }()
			return reader
		}
		var r struct {
			JWT string `json:"jwt"`
		}
		if e = c.requestBody(ctx, "POST", uploadPath+"/assets/upload?base64=true", session.JWT, "multipart/form-data; boundary="+boundary, body, -1, &r, true, false); e != nil {
			return "", e
		}
		if r.JWT != "" {
			completion = r.JWT
		}
	}
	if completion == "" {
		return "", errors.New("asset upload incomplete")
	}
	return CompletionToken(completion), nil
}

func writeAssetBucket(dst io.Writer, boundary string, bucket []string, files map[string]File) error {
	mw := multipart.NewWriter(dst)
	if err := mw.SetBoundary(boundary); err != nil {
		return err
	}
	for _, hash := range bucket {
		f := files[hash]
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": hash, "filename": hash}))
		hdr.Set("Content-Type", f.MIME)
		part, err := mw.CreatePart(hdr)
		if err != nil {
			return err
		}
		enc := base64.NewEncoder(base64.StdEncoding, part)
		if _, err = enc.Write(f.Data); err != nil {
			return err
		}
		if err = enc.Close(); err != nil {
			return err
		}
	}
	return mw.Close()
}
