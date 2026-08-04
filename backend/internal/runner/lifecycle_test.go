package runner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestLifecyclePublicShapesDoNotExposeProviderIdentity(t *testing.T) {
	for _, value := range []any{
		LifecycleCursor{},
		LifecycleSnapshot{},
		LifecycleBootstrap{},
		LifecycleDelta{},
	} {
		typeOf := reflect.TypeOf(value)
		for index := 0; index < typeOf.NumField(); index++ {
			field := typeOf.Field(index)
			switch field.Name {
			case "AllocationID", "ProviderID", "Provider", "ExternalID":
				t.Fatalf("%s exposes internal identity field %s", typeOf.Name(), field.Name)
			}
		}
	}
}

func TestLifecycleSnapshotCursor(t *testing.T) {
	snapshot := LifecycleSnapshot{
		SessionID:  "11111111-1111-4111-8111-111111111111",
		Generation: 7, EventSequence: 31,
	}
	want := LifecycleCursor{SessionID: snapshot.SessionID, Generation: 7, EventSequence: 31}
	if got := snapshot.Cursor(); got != want {
		t.Fatalf("snapshot cursor = %+v, want %+v", got, want)
	}
}

func TestLifecycleEventJSONOmitsEmptyAllocationID(t *testing.T) {
	event := DurableEvent{Sequence: 2, Type: "ready", Payload: json.RawMessage(`{}`)}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if allocation, exists := object["AllocationID"]; exists && allocation != "" {
		t.Fatalf("public lifecycle event leaks allocation id: %s", encoded)
	}
}

func TestPostgresLifecycleReaderRejectsInvalidInputBeforeDatabase(t *testing.T) {
	store := &PostgresStore{}
	if _, err := store.BootstrapLifecycle(context.Background(), "not-a-uuid", nil); err == nil {
		t.Fatal("bootstrap accepted malformed user identity")
	}
	if _, err := store.BootstrapLifecycle(context.Background(), "11111111-1111-4111-8111-111111111111", &LifecycleCursor{}); !errors.Is(err, ErrLifecycleNotFoundOrForbidden) {
		t.Fatalf("malformed resume error = %v", err)
	}
	if _, err := store.ReadLifecycleDelta(context.Background(), "11111111-1111-4111-8111-111111111111", LifecycleCursor{}, 1); !errors.Is(err, ErrLifecycleNotFoundOrForbidden) {
		t.Fatalf("malformed delta cursor error = %v", err)
	}
	cursor := LifecycleCursor{
		SessionID:  "22222222-2222-4222-8222-222222222222",
		Generation: 1, EventSequence: 1,
	}
	if _, err := store.ReadLifecycleDelta(context.Background(), "11111111-1111-4111-8111-111111111111", cursor, maxLifecycleBootstrapReplay+1); !errors.Is(err, ErrLifecycleCursorUnavailable) {
		t.Fatalf("oversized delta limit error = %v", err)
	}
}
