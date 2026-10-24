// Package backup creates offline, verified recovery points. It never stops a
// server, replaces an existing backup, or restores into a production database.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Collection struct {
	Name    string `json:"name"`
	Data    string `json:"data"`
	Indexes string `json:"indexes"`
	Count   int64  `json:"count"`
}
type Manifest struct {
	Schema      int          `json:"schema"`
	Database    string       `json:"database"`
	CapturedAt  time.Time    `json:"captured_at"`
	Collections []Collection `json:"collections"`
	Files       []File       `json:"files"`
	Roots       []string     `json:"roots"`
}

const maxFile = int64(2 << 30)

func local(name string) bool {
	return name != "" && name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00")
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

func Create(parent context.Context, db *database.DB, project, destination string, roots []string) (Manifest, error) {
	manifest := Manifest{Schema: 1, CapturedAt: time.Now().UTC(), Roots: append([]string(nil), roots...)}
	if !local(destination) || len(roots) == 0 {
		return manifest, fmt.Errorf("new project-relative destination and source roots required")
	}
	for _, root := range roots {
		if !local(root) || root == destination || strings.HasPrefix(destination, root+"/") || strings.HasPrefix(root, destination+"/") {
			return manifest, fmt.Errorf("backup roots and destination must be disjoint")
		}
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	owner := "backup-" + primitive.NewObjectID().Hex()
	slot := "__exclusive_site_writer__"
	now := time.Now()
	_, err := db.Collection("instance_liveness").UpdateOne(ctx, bson.M{"_id": slot, "last_heartbeat": bson.M{"$lte": now.Add(-10 * time.Minute)}}, bson.M{"$set": bson.M{"owner": owner, "started_at": now, "last_heartbeat": now}}, options.Update().SetUpsert(true))
	if err != nil {
		return manifest, fmt.Errorf("stop the CMS before creating an offline backup: writer lease unavailable")
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		_, _ = db.Collection("instance_liveness").DeleteOne(cleanup, bson.M{"_id": slot, "owner": owner})
	}()
	rivals, err := db.Collection("instance_liveness").CountDocuments(ctx, bson.M{"_id": bson.M{"$ne": slot}, "last_heartbeat": bson.M{"$gte": now.Add(-10 * time.Minute)}})
	if err != nil {
		return manifest, err
	}
	if rivals > 0 {
		return manifest, fmt.Errorf("live server heartbeat remains; stop the CMS first")
	}
	source, err := os.OpenRoot(project)
	if err != nil {
		return manifest, err
	}
	defer source.Close()
	if err := source.MkdirAll(path.Dir(destination), 0700); err != nil {
		return manifest, err
	}
	if err := source.Mkdir(destination, 0700); err != nil {
		return manifest, fmt.Errorf("backup destination must not exist: %w", err)
	}
	out, err := source.OpenRoot(destination)
	if err != nil {
		return manifest, err
	}
	defer out.Close()
	write := func(name string, reader io.Reader) error {
		if !local(name) || len(manifest.Files) >= 100000 {
			return fmt.Errorf("invalid backup file or file count exceeded")
		}
		if err := out.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		file, err := out.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		hash := sha256.New()
		size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, reader}, maxFile+1))
		if err != nil {
			return err
		}
		if size > maxFile {
			return fmt.Errorf("backup file exceeds 2 GiB limit")
		}
		if err := file.Sync(); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, File{Path: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))})
		return nil
	}
	mongoDB := db.Collection("content").Database()
	manifest.Database = mongoDB.Name()
	names, err := mongoDB.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return manifest, err
	}
	sort.Strings(names)
	session, err := mongoDB.Client().StartSession(options.Session().SetSnapshot(true))
	if err != nil {
		return manifest, err
	}
	defer session.EndSession(ctx)
	err = mongo.WithSession(ctx, session, func(sc mongo.SessionContext) error {
		for _, name := range names {
			if name == "instance_liveness" || strings.HasPrefix(name, "system.") {
				continue
			}
			encoded := base64.RawURLEncoding.EncodeToString([]byte(name))
			entry := Collection{Name: name, Data: "database/" + encoded + ".bson", Indexes: "indexes/" + encoded + ".bson"}
			cursor, err := mongoDB.Collection(name).Find(sc, bson.M{}, options.Find().SetBatchSize(100))
			if err != nil {
				return err
			}
			reader, writer := io.Pipe()
			done := make(chan error, 1)
			go func() {
				var failure error
				defer func() { cursor.Close(sc); writer.CloseWithError(failure); done <- failure }()
				for cursor.Next(sc) {
					if _, failure = writer.Write(cursor.Current); failure != nil {
						return
					}
					entry.Count++
				}
				failure = cursor.Err()
			}()
			copyErr := write(entry.Data, reader)
			reader.CloseWithError(copyErr)
			cursorErr := <-done
			if copyErr != nil {
				return copyErr
			}
			if cursorErr != nil {
				return cursorErr
			}
			// Index definitions are metadata; offline writer fencing prevents changes.
			indexes, err := mongoDB.Collection(name).Indexes().List(ctx)
			if err != nil {
				return err
			}
			reader, writer = io.Pipe()
			done = make(chan error, 1)
			go func() {
				var failure error
				defer func() { indexes.Close(ctx); writer.CloseWithError(failure); done <- failure }()
				for indexes.Next(ctx) {
					if _, failure = writer.Write(indexes.Current); failure != nil {
						return
					}
				}
				failure = indexes.Err()
			}()
			copyErr = write(entry.Indexes, reader)
			reader.CloseWithError(copyErr)
			cursorErr = <-done
			if copyErr != nil {
				return copyErr
			}
			if cursorErr != nil {
				return cursorErr
			}
			manifest.Collections = append(manifest.Collections, entry)
		}
		return nil
	})
	if err != nil {
		return manifest, err
	}
	for _, root := range roots {
		err = fs.WalkDir(source.FS(), root, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("backup refuses symlink: %s", name)
			}
			if entry.IsDir() {
				if strings.Contains(entry.Name(), ".building-") {
					return fs.SkipDir
				}
				return nil
			}
			before, err := entry.Info()
			if err != nil {
				return err
			}
			if !before.Mode().IsRegular() {
				return fmt.Errorf("non-regular backup source")
			}
			file, err := source.Open(name)
			if err != nil {
				return err
			}
			defer file.Close()
			if err := write("files/"+name, file); err != nil {
				return err
			}
			after, err := source.Stat(name)
			if err != nil {
				return err
			}
			if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				return fmt.Errorf("source changed during offline backup: %s", name)
			}
			return nil
		})
		if err != nil {
			return manifest, err
		}
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	if err := db.Collection("instance_liveness").FindOne(ctx, bson.M{"_id": slot, "owner": owner, "last_heartbeat": now}).Err(); err != nil {
		return manifest, fmt.Errorf("backup lost exclusive writer fence")
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	if len(raw) > 32<<20 {
		return manifest, fmt.Errorf("backup manifest exceeds limit")
	}
	file, err := out.OpenFile("manifest.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return manifest, err
	}
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return manifest, err
	}
	err = file.Sync()
	file.Close()
	if err != nil {
		return manifest, err
	}
	sum := sha256.Sum256(raw)
	complete, err := out.OpenFile("complete.sha256", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return manifest, err
	}
	_, err = complete.WriteString(hex.EncodeToString(sum[:]))
	if err == nil {
		err = complete.Sync()
	}
	complete.Close()
	if err != nil {
		return manifest, err
	}
	return Verify(project, destination)
}

func Verify(project, destination string) (Manifest, error) {
	var manifest Manifest
	if !local(destination) {
		return manifest, fmt.Errorf("invalid backup path")
	}
	projectRoot, err := os.OpenRoot(project)
	if err != nil {
		return manifest, err
	}
	defer projectRoot.Close()
	root, err := projectRoot.OpenRoot(destination)
	if err != nil {
		return manifest, err
	}
	defer root.Close()
	read := func(name string, limit int64) ([]byte, error) {
		f, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, limit+1))
		if int64(len(raw)) > limit {
			return nil, fmt.Errorf("backup metadata too large")
		}
		return raw, err
	}
	raw, err := read("manifest.json", 32<<20)
	if err != nil {
		return manifest, err
	}
	marker, err := read("complete.sha256", 64)
	if err != nil {
		return manifest, err
	}
	sum := sha256.Sum256(raw)
	if string(marker) != hex.EncodeToString(sum[:]) {
		return manifest, fmt.Errorf("backup manifest integrity failed")
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Schema != 1 || len(manifest.Files) > 100000 {
		return manifest, fmt.Errorf("unsupported backup manifest")
	}
	seen := map[string]bool{}
	for _, entry := range manifest.Files {
		if !local(entry.Path) || seen[entry.Path] || entry.Size < 0 || entry.Size > maxFile {
			return manifest, fmt.Errorf("invalid backup entry")
		}
		seen[entry.Path] = true
		file, err := root.Open(entry.Path)
		if err != nil {
			return manifest, err
		}
		hash := sha256.New()
		size, err := io.Copy(hash, io.LimitReader(file, entry.Size+1))
		file.Close()
		if err != nil {
			return manifest, err
		}
		if size != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return manifest, fmt.Errorf("backup file integrity failed: %s", entry.Path)
		}
	}
	for _, collection := range manifest.Collections {
		if collection.Name == "" || !seen[collection.Data] || !seen[collection.Indexes] {
			return manifest, fmt.Errorf("backup database manifest incomplete")
		}
	}
	return manifest, nil
}
