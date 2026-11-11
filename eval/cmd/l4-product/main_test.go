package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRefusesOnlineOrImplicitMode(t *testing.T) {
	for _, args := range [][]string{nil, {"-env", "real"}, {"-env", "fake", "-phase", "online"}} {
		if err := run(args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// TestAttemptQIDNamesEveryQuestion is the identity rule of an attempt that
// asks several questions: every question is its own attempt, and the number
// of the question is on the identifier, so that the scorer can tell which
// answer key the attempt belongs to.
func TestAttemptQIDNamesEveryQuestion(t *testing.T) {
	cases := []struct {
		total int
		want  []string
	}{
		{1, []string{"l4r2-2026ae-f1-01"}},
		{2, []string{"l4r2-2026ae-f1-01#q1", "l4r2-2026ae-f1-01#q2"}},
		{3, []string{"l4r2-2026ae-f1-01#q1", "l4r2-2026ae-f1-01#q2", "l4r2-2026ae-f1-01#q3"}},
	}
	for _, testCase := range cases {
		for index := range testCase.total {
			got := attemptQID("l4r2-2026ae-f1-01", index, testCase.total)
			if got != testCase.want[index] {
				t.Fatalf("total=%d index=%d got=%q want=%q", testCase.total, index, got, testCase.want[index])
			}
		}
	}
}

// TestReadQuestionsReadsTheOrder is the reading of the questions of an
// attempt: the file holds them in the order they are asked, and the order is
// the order of the file.
//
// A question may say which round of the conversation it was asked at. The
// turn is what the scorer reads the attempt by, so it has to survive the
// reading.
func TestReadQuestionsReadsTheOrder(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "questions.json")
	if err := os.WriteFile(file, []byte(`["第一问",{"turn":45,"question":"那第二项呢？"},{"turn":47,"question":"它的验收标准是什么？"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	questions, err := readQuestions(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 3 {
		t.Fatalf("questions=%v", questions)
	}
	if questions[0].Text != "第一问" || questions[0].Turn != 0 {
		t.Fatalf("the first question is %+v", questions[0])
	}
	if questions[1].Text != "那第二项呢？" || questions[1].Turn != 45 {
		t.Fatalf("the second question is %+v", questions[1])
	}
	if questions[2].Turn != 47 {
		t.Fatalf("the third question is %+v", questions[2])
	}
}

// TestReadQuestionsRefusals is the table of the question files the command
// has to refuse: an empty list, a list that is too long, an empty question
// and a file that carries something else.
func TestReadQuestionsRefusals(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
	}{
		{"empty list", "[]"},
		{"too many", `["a","b","c","d"]`},
		{"empty question", `[""]`},
		{"not an array", `{"question":"a"}`},
		{"trailing data", `["a"] {"b":1}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			file := filepath.Join(dir, testCase.name+".json")
			if err := os.WriteFile(file, []byte(testCase.content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readQuestions(file); err == nil {
				t.Fatal("accepted an invalid questions file")
			}
		})
	}
}

// TestReadSeedMemoriesReadsTheSeed is the reading of the profile memories of
// an attempt.
func TestReadSeedMemoriesReadsTheSeed(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "seed.json")
	content := `[{"scope":"global","subject":"常用编辑器","content":"常用编辑器: Neovim","tags":["工具"],"confirmed":true,"saved_at":"2026-01-14"}]`
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	memories, err := readSeedMemories(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].Subject != "常用编辑器" || memories[0].SavedAt != "2026-01-14" || !memories[0].Confirmed {
		t.Fatalf("memories=%+v", memories)
	}
}

// TestReadSeedMemoriesRefusals is the table of the seed files the command has
// to refuse.
func TestReadSeedMemoriesRefusals(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
	}{
		{"empty list", "[]"},
		{"not an array", `{"scope":"global"}`},
		{"unknown field", `[{"scope":"global","subject":"a","content":"b","confirmed":true,"saved_at":"2026-01-14","extra":1}]`},
		{"trailing data", `[] []`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			file := filepath.Join(dir, testCase.name+".json")
			if err := os.WriteFile(file, []byte(testCase.content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readSeedMemories(file); err == nil {
				t.Fatal("accepted an invalid seed memory file")
			}
		})
	}
}
