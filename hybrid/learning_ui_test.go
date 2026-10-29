package hybrid

import (
	"GopherAI/model"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

func TestLearningPageLogicThroughLocalSocket(t *testing.T) {
	app, db, agent, _, _ := learningFixture(t, true)
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	command := exec.Command("node", "../web/test/helpers/hybridLearningUiClient.mjs")
	command.Env = append(os.Environ(), "HYBRID_TEST_URL="+server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("actual page logic/API -> native auth/service/DAO: %v\n%s", err, output)
	}
	var task model.LearningSession
	if err := db.First(&task).Error; err != nil || task.Status != model.LearningSessionCancelled || task.CurrentQuestion != 3 {
		t.Fatalf("page did not persist original state: %+v %v", task, err)
	}
	var attempts []model.LearningAttempt
	if err := db.Order("created_at").Find(&attempts).Error; err != nil || len(attempts) != 2 {
		t.Fatalf("actual attempts %+v %v", attempts, err)
	}
	if attempts[0].EffectiveTotal != 25 || attempts[1].Status != model.LearningAttemptSkipped {
		t.Fatal("page did not preserve correction/skip", attempts)
	}
	agent.state.mu.Lock()
	defer agent.state.mu.Unlock()
	if agent.state.generationCalls != 1 || agent.state.gradingCalls != 1 {
		t.Fatalf("page retry duplicated model calls: %d %d", agent.state.generationCalls, agent.state.gradingCalls)
	}
}
