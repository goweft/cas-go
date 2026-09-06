package store_test

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/goweft/cas/internal/store"
)

func TestOrchestrationRunLifecycleSQLite(t *testing.T) {
	f, _ := os.CreateTemp("", "cas-orch-*.db")
	f.Close()
	defer os.Remove(f.Name())
	s, err := store.NewSQLiteStore(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testOrchestrationRunLifecycle(t, s)
}

func TestOrchestrationRunLifecycleMemory(t *testing.T) {
	testOrchestrationRunLifecycle(t, store.NewMemoryStore())
}

// A run is inserted as running with no summary, then finalized in place.
func testOrchestrationRunLifecycle(t *testing.T, s store.Store) {
	t.Helper()
	run := store.OrchestrationRunRow{
		ID: "run-1", SessionID: "sess-1", Instruction: "do the thing",
		Status: store.OrchestrationRunning, CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.SaveOrchestrationRun(run); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.LoadOrchestrationRuns("sess-1")
	if len(runs) != 1 || runs[0].Status != store.OrchestrationRunning {
		t.Fatalf("after insert: %+v", runs)
	}

	if err := s.SaveOrchestrationStep(store.OrchestrationStepRow{
		ID: "step-1", RunID: "run-1", StepNumber: 1, WorkspaceID: "ws", Instruction: "x", Error: "boom",
	}); err != nil {
		t.Fatal(err)
	}

	run.Status = store.OrchestrationFailed
	run.Error = "step 1: boom"
	run.StepCount = 1
	if err := s.UpdateOrchestrationRun(run); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.LoadOrchestrationRuns("sess-1")
	if len(runs) != 1 {
		t.Fatalf("update must not create a second row, got %d", len(runs))
	}
	if runs[0].Status != store.OrchestrationFailed || runs[0].Error != "step 1: boom" || runs[0].StepCount != 1 {
		t.Errorf("after finalize: %+v", runs[0])
	}
	steps, _ := s.LoadOrchestrationSteps("run-1")
	if len(steps) != 1 || steps[0].Error != "boom" {
		t.Errorf("step error not persisted: %+v", steps)
	}

	if err := s.UpdateOrchestrationRun(store.OrchestrationRunRow{ID: "nope"}); err == nil {
		t.Error("updating an unknown run must fail, not silently no-op")
	}
}

// TestOrchestrationMigrationV3BackfillsCompleted: a v2 database (runs only
// ever written after success) opens under v3 with those runs reported as
// completed and their steps error-free.
func TestOrchestrationMigrationV3BackfillsCompleted(t *testing.T) {
	f, _ := os.CreateTemp("", "cas-orch-v2-*.db")
	f.Close()
	defer os.Remove(f.Name())

	// Build a v2-shaped database by hand.
	db, err := sql.Open("sqlite", f.Name())
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE orchestration_runs (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, instruction TEXT NOT NULL, summary TEXT NOT NULL DEFAULT '', step_count INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL)`,
		`CREATE TABLE orchestration_steps (id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES orchestration_runs(id), step_number INTEGER NOT NULL, workspace_id TEXT NOT NULL, instruction TEXT NOT NULL, output TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO orchestration_runs VALUES ('r1','s1','old run','done',1,'2026-01-01T00:00:00Z')`,
		`INSERT INTO orchestration_steps VALUES ('st1','r1',1,'ws','x','ok')`,
		`PRAGMA user_version = 2`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := store.NewSQLiteStore(f.Name())
	if err != nil {
		t.Fatalf("open v2 db under v3 code: %v", err)
	}
	defer s.Close()

	runs, err := s.LoadOrchestrationRuns("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != store.OrchestrationCompleted || runs[0].Error != "" {
		t.Errorf("v2 run not backfilled as completed: %+v", runs)
	}
	steps, err := s.LoadOrchestrationSteps("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Error != "" || steps[0].Output != "ok" {
		t.Errorf("v2 step not readable under v3: %+v", steps)
	}
}
