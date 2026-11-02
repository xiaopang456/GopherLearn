package conversation

import "testing"

func TestCountJobs(t *testing.T) {
	jobs := []JobState{{ID: "1", Kind: "episode", Status: "pending"}, {ID: "2", Kind: "memory", Status: "processing", Attempts: 1}, {ID: "3", Kind: "memory", Status: "rejected", Attempts: 1}}
	got, err := CountJobs(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if got["episode"].Pending != 1 || got["memory"].Processing != 1 || got["memory"].Rejected != 1 {
		t.Fatalf("bad counts: %+v", got)
	}
	for _, bad := range [][]JobState{{jobs[0], jobs[0]}, {{ID: "x", Kind: "episode", Status: "rejected"}}, {{ID: "x", Kind: "memory", Status: "unknown"}}} {
		if _, err = CountJobs(bad); err == nil {
			t.Fatal("accepted invalid observation")
		}
	}
}
