package migrator

import (
	"context"
	"errors"
	"testing"
)

func TestStoreBasicChanges(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.ApplyChange(ctx, Change{Kind: CreateTable, Table: "users", Columns: []Column{{Name: "id", Type: "int", Nullable: false}}}); err != nil {
		t.Fatal(err)
	}
	// duplicate table
	if err := s.ApplyChange(ctx, Change{Kind: CreateTable, Table: "users"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
	if err := s.ApplyChange(ctx, Change{Kind: AddColumn, Table: "users", Column: "email", Type: "string", Nullable: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyChange(ctx, Change{Kind: DropColumn, Table: "users", Column: "email"}); err != nil {
		t.Fatal(err)
	}
	// not null without default rejected
	if err := s.ApplyChange(ctx, Change{Kind: SetNullable, Table: "users", Column: "id", Nullable: false}); err == nil {
		t.Fatal("want error setting NOT NULL without default, got nil")
	}
	sc := s.CurrentSchema()
	if len(sc.Tables) != 1 || len(sc.Tables[0].Columns) != 1 {
		t.Fatalf("unexpected schema: %+v", sc)
	}
}

func TestStoreJournalAtomicity(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir)
	ch := Change{Kind: CreateTable, Table: "t1", Columns: []Column{{Name: "a", Type: "int", Nullable: true}}}
	j := &StepJournal{Version: "1.0.0", Direction: Up, Total: 1}
	if err := s.ApplyChangeAndJournal(ch, 0, j); err != nil {
		t.Fatal(err)
	}
	j2, err := s.LoadJournal()
	if err != nil || j2 == nil || len(j2.Applied) != 1 || j2.Applied[0] != 0 {
		t.Fatalf("journal not persisted: %+v err=%v", j2, err)
	}
	if len(s.CurrentSchema().Tables) != 1 {
		t.Fatal("change should be visible with journal")
	}
	if err := s.CompleteStep(*j2); err != nil {
		t.Fatal(err)
	}
	if j3, _ := s.LoadJournal(); j3 != nil {
		t.Fatal("journal should be cleared")
	}
	if !s.IsApplied("1.0.0") {
		t.Fatal("version should be marked applied")
	}
	// reopen: state persists
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsApplied("1.0.0") || len(s2.CurrentSchema().Tables) != 1 {
		t.Fatal("state did not persist across reopen")
	}
}
