package migrator

import "testing"

func planWith(ms ...Migration) Plan {
	var steps []PlannedMigration
	for i := range ms {
		steps = append(steps, PlannedMigration{Migration: &ms[i], Direction: Up})
	}
	return Plan{Steps: steps}
}

func TestCheckCompatibility_AdditiveIsOK(t *testing.T) {
	m := Migration{Version: "1.0.0", Up: []Change{
		{Kind: CreateTable, Table: "t", Columns: []Column{{Name: "id", Type: "int", Nullable: false, HasDefault: true, Default: 0}}},
		{Kind: AddColumn, Table: "t", Column: "name", Type: "string", Nullable: true},
	}}
	rep := CheckCompatibility(planWith(m))
	if !rep.Compatible {
		t.Fatalf("additive migration should be compatible, got %+v", rep.Violations)
	}
}

func TestCheckCompatibility_DropColumnStaged(t *testing.T) {
	m := Migration{Version: "2.0.0", Up: []Change{{Kind: DropColumn, Table: "t", Column: "old", Type: "string"}}}
	rep := CheckCompatibility(planWith(m))
	if rep.Compatible {
		t.Fatal("drop column must be rejected")
	}
	if rep.Violations[0].Reason != RejectDropColumn {
		t.Fatalf("want drop_column, got %s", rep.Violations[0].Reason)
	}
	if len(rep.Staged) != 1 || len(rep.Staged[0].Phases) != 2 {
		t.Fatalf("want 2 phases, got %+v", rep.Staged)
	}
	if rep.Staged[0].Phases[0].Version != "2.0.0+expand" {
		t.Fatalf("bad phase version: %s", rep.Staged[0].Phases[0].Version)
	}
}

func TestCheckCompatibility_AlterTypeStaged(t *testing.T) {
	m := Migration{Version: "3.0.0", Up: []Change{{Kind: AlterColumn, Table: "t", Column: "c", Type: "bigint"}}}
	rep := CheckCompatibility(planWith(m))
	if rep.Compatible || rep.Violations[0].Reason != RejectAlterType {
		t.Fatalf("want alter_type rejection, got %+v", rep)
	}
	if len(rep.Staged[0].Phases) != 3 {
		t.Fatalf("want 3 phases for type change, got %d", len(rep.Staged[0].Phases))
	}
}

func TestCheckCompatibility_NotNullStaged(t *testing.T) {
	// no default -> reject, staged
	m := Migration{Version: "4.0.0", Up: []Change{{Kind: SetNullable, Table: "t", Column: "c", Nullable: false}}}
	rep := CheckCompatibility(planWith(m))
	if rep.Compatible || rep.Violations[0].Reason != RejectAddNotNull {
		t.Fatalf("want not null rejection, got %+v", rep)
	}
	if len(rep.Staged[0].Phases) != 2 {
		t.Fatalf("want 2 phases, got %d", len(rep.Staged[0].Phases))
	}
}

func TestCheckCompatibility_RenameStaged(t *testing.T) {
	m := Migration{Version: "5.0.0", Up: []Change{{Kind: RenameColumn, Table: "t", Column: "a", RenameTo: "b", Type: "string"}}}
	rep := CheckCompatibility(planWith(m))
	if rep.Compatible || rep.Violations[0].Reason != RejectRenameColumn {
		t.Fatalf("want rename rejection, got %+v", rep)
	}
	if len(rep.Staged[0].Phases) != 3 {
		t.Fatalf("want 3 phases, got %d", len(rep.Staged[0].Phases))
	}
}

func TestCheckCompatibility_DropTableNoSafePath(t *testing.T) {
	m := Migration{Version: "6.0.0", Up: []Change{{Kind: DropTable, Table: "t"}}}
	rep := CheckCompatibility(planWith(m))
	if rep.Compatible || rep.Violations[0].Reason != RejectDropTable {
		t.Fatalf("want drop_table rejection, got %+v", rep)
	}
	if rep.Violations[0].Staged {
		t.Fatal("drop table should not offer staged path")
	}
	if len(rep.Staged) != 0 {
		t.Fatalf("drop table must have no staged migrations, got %+v", rep.Staged)
	}
}

func TestBuildStagedMigrations(t *testing.T) {
	m := Migration{Version: "7.0.0", Up: []Change{{Kind: DropColumn, Table: "t", Column: "x", Type: "string"}}}
	sm := BuildStagedMigrations(m)
	if len(sm) != 1 || sm[0].Source != "7.0.0" {
		t.Fatalf("bad staged: %+v", sm)
	}
}
