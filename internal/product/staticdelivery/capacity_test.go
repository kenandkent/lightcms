package staticdelivery

import (
	"context"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"testing"
)

type sizedReleaseStore struct {
	memoryReleaseStore
	reads int
}

func (s *sizedReleaseStore) LoadBytes(context.Context, primitive.ObjectID) (int64, error) {
	s.reads++
	return 100 << 20, nil
}
func TestReleaseReservationIncludesBothRollbackSources(t *testing.T) {
	store := &sizedReleaseStore{}
	one, two := primitive.NewObjectID(), primitive.NewObjectID()
	single, err := releaseReservation(context.Background(), store, one)
	if err != nil {
		t.Fatal(err)
	}
	both, err := releaseReservation(context.Background(), store, one, two, one)
	if err != nil {
		t.Fatal(err)
	}
	if both-single != 300<<20 || store.reads != 3 {
		t.Fatalf("rollback memory not counted or duplicate counted: single=%d both=%d reads=%d", single, both, store.reads)
	}
}
