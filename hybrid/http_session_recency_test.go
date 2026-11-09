package hybrid

import (
	"context"
	"errors"
	"testing"
	"time"

	sessionDAO "GopherAI/dao/session"
	"github.com/cloudwego/eino/schema"
)

// The session list is ordered by updated_at, and the retired ordinary append bumped that
// timestamp in the same transaction as the message insert. The shared path commits the
// answer without touching the session row at all, so moving a conversation onto it would
// quietly stop a session from rising to the top when the learner uses it. This is the
// behaviour that has to survive the move, on both endpoints.
func TestConversationTurnKeepsItsSessionAtTheTopOfTheList(t *testing.T) {
	app, db, agent, token, _ := httpFixture(t)
	older := createHTTP(t, app.Handler, token, "older")
	newer := createHTTP(t, app.Handler, token, "newer")

	top := func() string {
		t.Helper()
		items := dataHTTP(t, app.Handler, "GET", "/api/v1/sessions", "", token)["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("session list %v", items)
		}
		return items[0].(map[string]any)["id"].(string)
	}
	stamp := func(sid string) time.Time {
		t.Helper()
		row, err := sessionDAO.NewRepository(db).GetByIDAndUserID(context.Background(), sid, 1)
		if err != nil {
			t.Fatal(err)
		}
		return row.UpdatedAt
	}

	if top() != newer {
		t.Fatalf("the list did not start in creation order: top=%s", top())
	}
	before := stamp(older)

	dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+older+"/messages", `{"content":"非流式"}`, token)
	if top() != older {
		t.Fatalf("a non-streaming conversation turn did not raise its session: top=%s", top())
	}
	if !stamp(older).After(before) {
		t.Fatalf("the session timestamp did not advance: before=%s after=%s", before, stamp(older))
	}

	dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+newer+"/messages", `{"content":"把 newer 顶回去"}`, token)
	if top() != newer {
		t.Fatalf("the second session did not rise: top=%s", top())
	}
	before = stamp(older)
	status, _, stream := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+older+"/stream", `{"content":"流式"}`, token)
	if status != 200 {
		t.Fatalf("conversation stream %d %s", status, stream)
	}
	if top() != older {
		t.Fatalf("a streaming conversation turn did not raise its session: top=%s", top())
	}
	if !stamp(older).After(before) {
		t.Fatalf("the streaming turn left the timestamp alone: before=%s after=%s", before, stamp(older))
	}

	// A turn that produced no answer has nothing to be recent about.
	dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+newer+"/messages", `{"content":"再把 newer 顶回去"}`, token)
	before = stamp(older)
	agent.setRespond(func(context.Context, int) (*schema.Message, error) {
		return nil, errors.New("synthetic model failure")
	})
	if status, _, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+older+"/messages", `{"content":"会失败"}`, token); status == 200 {
		t.Fatal("the failing turn was reported as a success, so this case proved nothing")
	}
	if top() != newer || !stamp(older).Equal(before) {
		t.Fatalf("a failed turn raised its session: top=%s before=%s after=%s", top(), before, stamp(older))
	}

	// Nor has a request the ceiling refused.
	if status, _, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+older+"/messages", `{"content":"要知识库","knowledgePolicy":"knowledge_only"}`, token); status != 400 {
		t.Fatalf("a chat session asking for the library was not refused: %d", status)
	}
	if top() != newer || !stamp(older).Equal(before) {
		t.Fatalf("a refused request raised its session: top=%s before=%s after=%s", top(), before, stamp(older))
	}
}
