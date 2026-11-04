package main

import "testing"

func TestScenarioIdentity(t *testing.T) {
	id, name, err := scenarioIdentity("run", "arm-a", "case-a", "main")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		got, n, e := scenarioIdentity("run", "arm-a", "case-a", "main")
		if e != nil || got != id || n != name {
			t.Fatal("identity changes between phases")
		}
	}
	for _, x := range [][4]string{{"run", "arm-a", "case-b", "main"}, {"run", "arm-b", "case-a", "main"}, {"run", "arm-a", "case-a", "control"}, {"run2", "arm-a", "case-a", "main"}} {
		got, n, e := scenarioIdentity(x[0], x[1], x[2], x[3])
		if e != nil || got == id || n == name {
			t.Fatalf("identity not isolated: %v", x)
		}
	}
	if _, _, err := scenarioIdentity("", "a", "b", "main"); err == nil {
		t.Fatal("missing run accepted")
	}
}
