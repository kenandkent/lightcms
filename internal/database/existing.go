package database

import (
	"context"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ConnectExisting is for backup/inspection tools: unlike application startup,
// connecting does not create indexes or mutate existing collection metadata.
func ConnectExisting(ctx context.Context, uri, name string) (*DB, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.WithoutCancel(ctx))
		return nil, err
	}
	return &DB{client: client, database: client.Database(name)}, nil
}
