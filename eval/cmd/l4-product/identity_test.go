package main

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"GopherAI/model"
)

// userNameColumnWidth reads the narrowest width the product's own tables give
// the user name. The derived user name is written into every one of them, so
// the evaluator has to stay inside the narrowest: a name that does not fit
// makes an attempt fail with a database error before the question is ever
// asked.
//
// A column without an explicit varchar tag falls back to the database's own
// default, which is far wider than any explicit width here; such a column
// cannot be the narrowest one, so it is not measured rather than guessed at.
func userNameColumnWidth(t *testing.T) int {
	t.Helper()
	narrowest := 0
	for _, column := range []struct {
		model  interface{}
		field  string
		notice string
	}{
		{model.Message{}, "UserName", "the message table"},
		{model.User{}, "Username", "the user table"},
		{model.Session{}, "UserName", "the session table"},
	} {
		field, ok := reflect.TypeOf(column.model).FieldByName(column.field)
		if !ok {
			t.Fatalf("%s has no %s field", column.notice, column.field)
		}
		match := regexp.MustCompile(`varchar\((\d+)\)`).FindStringSubmatch(field.Tag.Get("gorm"))
		if len(match) != 2 {
			continue
		}
		width, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatal(err)
		}
		if narrowest == 0 || width < narrowest {
			narrowest = width
		}
	}
	if narrowest == 0 {
		t.Fatal("no explicit varchar width found for the user name")
	}
	return narrowest
}

// The derived name is what every user-name column stores, so it has to fit the
// narrowest one. The width counts characters, which is what a varchar width
// is, so the name is measured in characters too.
func TestScenarioIdentityNameFitsTheMessageTable(t *testing.T) {
	width := userNameColumnWidth(t)
	for _, x := range [][4]string{
		{"run", "arm-a", "case-a", "main"},
		{"a-very-long-evaluation-run-id", "a-configuration-with-a-long-name", "a-scenario-with-a-long-id", "distractor"},
	} {
		_, name, err := scenarioIdentity(x[0], x[1], x[2], x[3])
		if err != nil {
			t.Fatal(err)
		}
		if runes := utf8.RuneCountInString(name); runes > width {
			t.Fatalf("the derived name %q is %d characters, the narrowest user-name column allows %d", name, runes, width)
		}
	}
}

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

func allExplicit() map[string]bool {
	return map[string]bool{"run-id": true, "arm": true, "qid": true, "condition": true}
}

func scenarioInput() identityInput {
	return identityInput{phase: "prepare", run: "eval-run-a", arm: "config-a", qid: "scenario-01", condition: "main"}
}

// The preparation and the three repeats have to restore the same conversation
// into the same user, otherwise a repeat answers from an empty history.
func TestDeriveScenarioIdentityIsStableAcrossPhasesAndRepeats(t *testing.T) {
	id, name, err := deriveScenarioIdentity(allExplicit(), scenarioInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepare", "answer"} {
		for rep := 0; rep <= 2; rep++ {
			in := scenarioInput()
			in.phase, in.rep = phase, rep
			got, gotName, err := deriveScenarioIdentity(allExplicit(), in)
			if err != nil {
				t.Fatalf("phase %s rep %d: %v", phase, rep, err)
			}
			if got != id || gotName != name {
				t.Fatalf("phase %s rep %d gave %d/%s, want %d/%s", phase, rep, got, gotName, id, name)
			}
		}
	}
}

// Each run, configuration, scenario and condition must land on its own user.
func TestDeriveScenarioIdentitySeparatesExperimentGroups(t *testing.T) {
	seen := map[int64]string{}
	seenNames := map[string]int64{}
	combinations := 0
	for _, run := range []string{"eval-run-a", "eval-run-b"} {
		for _, arm := range []string{"config-a", "config-b", "config-c"} {
			for _, qid := range []string{"scenario-01", "scenario-02", "scenario-03"} {
				for _, condition := range []string{"main", "control", "distractor"} {
					in := scenarioInput()
					in.run, in.arm, in.qid, in.condition = run, arm, qid, condition
					id, name, err := deriveScenarioIdentity(allExplicit(), in)
					if err != nil {
						t.Fatal(err)
					}
					combinations++
					if other, taken := seen[id]; taken {
						t.Fatalf("user id %d serves both %s and %s", id, other, name)
					}
					if other, taken := seenNames[name]; taken {
						t.Fatalf("user name %s serves both %d and %d", name, other, id)
					}
					seen[id], seenNames[name] = name, id
					if id <= 0 || !strings.HasPrefix(name, "l4_") {
						t.Fatalf("identity %d/%s is not usable", id, name)
					}
				}
			}
		}
	}
	if combinations != 54 || len(seen) != 54 || len(seenNames) != 54 {
		t.Fatalf("combinations=%d ids=%d names=%d", combinations, len(seen), len(seenNames))
	}
}

func TestDeriveScenarioIdentityRefusesSharedStateAndSilentDefaults(t *testing.T) {
	cases := []struct {
		name     string
		explicit map[string]bool
		mutate   func(*identityInput)
		want     string
	}{
		{"missing run-id", map[string]bool{"arm": true, "qid": true, "condition": true}, func(i *identityInput) {}, "explicit run-id"},
		{"missing arm", map[string]bool{"run-id": true, "qid": true, "condition": true}, func(i *identityInput) {}, "explicit arm"},
		{"missing qid", map[string]bool{"run-id": true, "arm": true, "condition": true}, func(i *identityInput) {}, "explicit qid"},
		{"missing condition", map[string]bool{"run-id": true, "arm": true, "qid": true}, func(i *identityInput) {}, "explicit condition"},
		{"empty run-id value", allExplicit(), func(i *identityInput) { i.run = "" }, "required"},
		{"knowledge seed", allExplicit(), func(i *identityInput) { i.knowledgeSeed = "seed.json" }, "empty knowledge base"},
		{"explicit user id", allExplicit(), func(i *identityInput) { i.userID = 7 }, "derived identity"},
		{"explicit user name", allExplicit(), func(i *identityInput) { i.userName = "hand-picked" }, "derived identity"},
		{"blank user name", allExplicit(), func(i *identityInput) { i.userName = "   " }, "derived identity"},
		{"answer phase with knowledge seed", allExplicit(), func(i *identityInput) { i.phase = "answer"; i.knowledgeSeed = "seed.json" }, "empty knowledge base"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := scenarioInput()
			c.mutate(&in)
			id, name, err := deriveScenarioIdentity(c.explicit, in)
			if err == nil {
				t.Fatalf("%s/%d/%s accepted with %q supplied", c.name, id, name, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s reported %q, want it to mention %q", c.name, err.Error(), c.want)
			}
		})
	}
}

// The shared corpus index is the one phase that legitimately runs as an
// operator-supplied user; nothing about a scenario attempt may change that.
func TestDeriveScenarioIdentityLeavesIndexPhaseAlone(t *testing.T) {
	in := scenarioInput()
	in.phase, in.userID, in.userName, in.knowledgeSeed = "index", 42, "corpus-owner", "seed.json"
	id, name, err := deriveScenarioIdentity(map[string]bool{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 || name != "corpus-owner" {
		t.Fatalf("index phase rewrote the identity to %d/%s", id, name)
	}
}

func TestScenarioIdentityCollisionCheckIsExhaustiveForListedGroups(t *testing.T) {
	ids := make([]int64, 0, 300)
	for i := 0; i < 300; i++ {
		in := scenarioInput()
		in.qid = fmt.Sprintf("scenario-%02d", i)
		id, _, err := deriveScenarioIdentity(allExplicit(), in)
		if err != nil {
			t.Fatal(err)
		}
		for _, previous := range ids {
			if previous == id {
				t.Fatalf("scenario %s reused user id %d", in.qid, id)
			}
		}
		ids = append(ids, id)
	}
}
