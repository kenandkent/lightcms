package backup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

var drillName = regexp.MustCompile(`^[a-z0-9_-]+-restore-test(?:-[a-z0-9_-]+)?$`)

// RestoreForVerification is intentionally not a production restore command.
// It accepts only a new isolated test database and a new files directory. TTL
// indexes are retained as definitions in the backup, but are not activated in
// the drill, so historical jobs cannot disappear while being inspected.
func RestoreForVerification(ctx context.Context, client *mongo.Client, project, source, name, destination string) (Manifest, error) {
	manifest, err := Verify(project, source)
	if err != nil {
		return manifest, err
	}
	if name == manifest.Database || !drillName.MatchString(name) || !local(destination) || source == destination || strings.HasPrefix(destination, source+"/") || strings.HasPrefix(source, destination+"/") {
		return manifest, fmt.Errorf("restore requires a new isolated *-restore-test database and disjoint directory")
	}
	target := client.Database(name)
	collections, err := target.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return manifest, err
	}
	if len(collections) > 0 {
		return manifest, fmt.Errorf("restore database must be empty")
	}
	projectRoot, err := os.OpenRoot(project)
	if err != nil {
		return manifest, err
	}
	defer projectRoot.Close()
	if err := projectRoot.MkdirAll(path.Dir(destination), 0700); err != nil {
		return manifest, err
	}
	if err := projectRoot.Mkdir(destination, 0700); err != nil {
		return manifest, fmt.Errorf("restore directory must not exist: %w", err)
	}
	input, err := projectRoot.OpenRoot(source)
	if err != nil {
		return manifest, err
	}
	defer input.Close()
	output, err := projectRoot.OpenRoot(destination)
	if err != nil {
		return manifest, err
	}
	defer output.Close()
	// Atomic creation reserves the empty database against another drill.
	if err := target.CreateCollection(ctx, "_restore_verification_guard"); err != nil {
		return manifest, err
	}
	for _, collection := range manifest.Collections {
		if strings.HasPrefix(collection.Name, "system.") || collection.Name == "instance_liveness" || collection.Name == "_restore_verification_guard" {
			return manifest, fmt.Errorf("invalid restore collection")
		}
		reader, err := input.Open(collection.Data)
		if err != nil {
			return manifest, err
		}
		count := int64(0)
		err = readBSON(contextReader{ctx, reader}, func(raw bson.Raw) error {
			_, err := target.Collection(collection.Name).InsertOne(ctx, raw)
			if err == nil {
				count++
			}
			return err
		})
		reader.Close()
		if err != nil {
			return manifest, err
		}
		if count != collection.Count {
			return manifest, fmt.Errorf("restored collection count mismatch")
		}
		// Empty collections must still exist for index restoration.
		if count == 0 {
			if err := target.CreateCollection(ctx, collection.Name); err != nil {
				return manifest, err
			}
		}
		indexes, err := input.Open(collection.Indexes)
		if err != nil {
			return manifest, err
		}
		err = readBSON(contextReader{ctx, indexes}, func(raw bson.Raw) error {
			var index bson.M
			if err := bson.Unmarshal(raw, &index); err != nil {
				return err
			}
			if index["name"] == "_id_" {
				return nil
			}
			if _, ttl := index["expireAfterSeconds"]; ttl {
				return nil
			}
			delete(index, "ns")
			delete(index, "v")
			delete(index, "background")
			return target.RunCommand(ctx, bson.D{{Key: "createIndexes", Value: collection.Name}, {Key: "indexes", Value: bson.A{index}}}).Err()
		})
		indexes.Close()
		if err != nil {
			return manifest, err
		}
	}
	for _, entry := range manifest.Files {
		if !strings.HasPrefix(entry.Path, "files/") {
			continue
		}
		name := strings.TrimPrefix(entry.Path, "files/")
		if !local(name) {
			return manifest, fmt.Errorf("invalid private file path")
		}
		if err := output.MkdirAll(path.Dir(name), 0700); err != nil {
			return manifest, err
		}
		reader, err := input.Open(entry.Path)
		if err != nil {
			return manifest, err
		}
		writer, err := output.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			reader.Close()
			return manifest, err
		}
		hash := sha256.New()
		size, err := io.Copy(io.MultiWriter(writer, hash), contextReader{ctx, reader})
		reader.Close()
		if err == nil {
			err = writer.Sync()
		}
		writer.Close()
		if err != nil {
			return manifest, err
		}
		if size != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return manifest, fmt.Errorf("restored file integrity mismatch")
		}
	}
	// Detect any backup mutation during the drill before reporting success.
	if _, err := Verify(project, source); err != nil {
		return manifest, err
	}
	return manifest, nil
}
func readBSON(reader io.Reader, visit func(bson.Raw) error) error {
	for {
		prefix := make([]byte, 4)
		_, err := io.ReadFull(reader, prefix)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		size := int64(int32(binary.LittleEndian.Uint32(prefix)))
		if size < 5 || size > 16<<20 {
			return fmt.Errorf("invalid BSON record size")
		}
		raw := make([]byte, int(size))
		copy(raw, prefix)
		if _, err := io.ReadFull(reader, raw[4:]); err != nil {
			return err
		}
		if err := bson.Raw(raw).Validate(); err != nil {
			return err
		}
		if err := visit(raw); err != nil {
			return err
		}
	}
}
