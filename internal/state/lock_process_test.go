package state

import (
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// The real contention is between separate crew processes: crew watch holding
// the loop while a CLI command mutates the same state. Goroutine tests do not
// prove that, because an in-process mutex would pass them. This test re-execs
// the test binary so the lock is exercised across genuine process boundaries.
const bumpEnv = "CREW_STATE_BUMP_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(bumpEnv); dir != "" {
		loc, err := time.LoadLocation("America/Denver")
		if err != nil {
			os.Exit(2)
		}
		s, err := Open(dir, loc)
		if err != nil {
			os.Exit(3)
		}
		for range 10 {
			if _, err := s.Update(func(st *State) error {
				ts := st.Tasks["shared"]
				if ts == nil {
					ts = &TaskState{ID: "shared"}
					st.Upsert(ts)
				}
				ts.Cycle++
				return nil
			}); err != nil {
				os.Exit(4)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestUpdateIsSerializedAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	const procs = 6
	const bumpsEach = 10

	var wg sync.WaitGroup
	errs := make(chan error, procs)
	for range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestMain")
			cmd.Env = append(os.Environ(), bumpEnv+"="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- err
				t.Logf("child failed: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("child process error: %v", err)
	}

	loc, _ := time.LoadLocation("America/Denver")
	s, _ := Open(dir, loc)
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	want := procs * bumpsEach
	if got := st.Tasks["shared"].Cycle; got != want {
		t.Fatalf("Cycle = %d, want %d (lost cross-process updates)", got, want)
	}
}
