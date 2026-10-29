package hybrid

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// integratedNavRoutes is the request surface the default navigation asks for:
// one row per path the rail's four pages and the sign-in screen actually send.
// A row that is in neither table below means that page has nothing to talk to,
// which is exactly the failure this integration must not ship.
var integratedNavRoutes = []struct{ method, path, source string }{
	{"POST", "/api/v1/auth/login", "web/src/api/auth.js:4"},
	{"GET", "/api/v1/me", "web/src/api/auth.js:5"},
	{"GET", "/api/v1/sessions", "web/src/api/sessions.js:11"},
	{"POST", "/api/v1/sessions", "web/src/api/sessions.js:11"},
	{"DELETE", "/api/v1/sessions/:id", "web/src/api/sessions.js:14"},
	{"GET", "/api/v1/sessions/:id/messages", "web/src/api/sessions.js:18"},
	{"POST", "/api/v1/sessions/:id/messages", "web/src/api/sessions.js:18"},
	{"POST", "/api/v1/sessions/:id/stream", "web/src/api/sessions.js:22"},
	{"GET", "/api/v1/knowledge/documents", "web/src/api/v2Documents.js:3"},
	{"POST", "/api/v1/knowledge/documents", "web/src/api/v2Documents.js:3"},
	{"GET", "/api/v1/knowledge/documents/:id", "web/src/api/v2Documents.js:6"},
	{"PUT", "/api/v1/knowledge/documents/:id", "web/src/api/v2Documents.js:6"},
	{"DELETE", "/api/v1/knowledge/documents/:id", "web/src/api/v2Documents.js:6"},
	{"POST", "/api/v1/knowledge/documents/:id/retry", "web/src/api/v2Documents.js:39"},
	{"POST", "/api/v1/knowledge/documents/:id/reindex", "web/src/api/v2Documents.js:40"},
	{"GET", "/api/v1/knowledge/documents/:document/source", "web/src/api/v2Documents.js:45"},
	{"GET", "/api/v1/AI/agent/memories", "web/src/api/v2Memory.js:3"},
	{"POST", "/api/v1/AI/agent/memories", "web/src/api/v2Memory.js:3"},
	{"PATCH", "/api/v1/AI/agent/memories/:id", "web/src/api/v2Memory.js:4"},
	{"POST", "/api/v1/AI/agent/memories/:id/archive", "web/src/api/v2Memory.js:4"},
	{"DELETE", "/api/v1/AI/agent/memories/:id", "web/src/api/v2Memory.js:4"},
	{"GET", "/api/v1/AI/agent/memories/:id/revisions", "web/src/api/v2Memory.js:4"},
	{"POST", "/api/v1/AI/agent/memory-drafts/:id/resolve", "web/src/api/v2Memory.js:42"},
	{"POST", "/api/v1/AI/agent/learning/start", "web/src/api/v2Learning.js:3"},
	{"GET", "/api/v1/AI/agent/learning/current", "web/src/api/v2Learning.js:3"},
	{"POST", "/api/v1/AI/agent/learning/answer", "web/src/api/v2Learning.js:3"},
	{"POST", "/api/v1/AI/agent/learning/action", "web/src/api/v2Learning.js:3"},
	{"POST", "/api/v1/AI/agent/learning/cancel", "web/src/api/v2Learning.js:3"},
	{"GET", "/api/v1/AI/agent/learning/progress", "web/src/api/v2Learning.js:3"},
	{"POST", "/api/v1/AI/agent/learning/correct", "web/src/api/v2Learning.js:3"},
	{"POST", "/api/v1/user/captcha", "web/src/api/v2Registration.js:5"},
	{"POST", "/api/v1/user/register", "web/src/api/v2Registration.js:5"},
	{"POST", "/api/v1/user/login", "web/src/api/v2Registration.js:5"},
	{"POST", "/api/v1/AI/agent/run", "v2/router/Agent.go:15"},
	{"POST", "/api/v1/AI/agent/run-stream", "v2/router/Agent.go:16"},
}

// mainlineCompat is the surface this module answers itself rather than passing
// through (hybrid/http.go:65-85).
var mainlineCompat = map[string]bool{
	"POST /api/v1/auth/login":                          true,
	"ANY /api/v1/auth/register":                        true,
	"GET /api/v1/me":                                   true,
	"GET /api/v1/knowledge/documents/:document/source": true,
	"GET /api/v1/sessions":                             true,
	"POST /api/v1/sessions":                            true,
	"DELETE /api/v1/sessions/:id":                      true,
	"GET /api/v1/sessions/:id/messages":                true,
	"POST /api/v1/sessions/:id/messages":               true,
	"POST /api/v1/sessions/:id/stream":                 true,
}

func nativeRouteSet(t *testing.T, handler http.Handler) map[string]bool {
	t.Helper()
	engine, ok := handler.(*gin.Engine)
	if !ok {
		t.Fatalf("the original V2 handler is %T, not a gin engine: the route table cannot be read", handler)
	}
	set := map[string]bool{}
	for _, route := range engine.Routes() {
		set[route.Method+" "+route.Path] = true
	}
	if len(set) == 0 {
		t.Fatal("the original V2 router registered nothing")
	}
	return set
}

// TestIntegratedRouteTablesCoverTheDefaultNavigation asserts coverage at the
// route table, not at the response code: a business handler may answer 404 for a
// missing row, which is a served path, while a missing registration is the
// failure mode under test.
func TestIntegratedRouteTablesCoverTheDefaultNavigation(t *testing.T) {
	app, _, _, _, _, _ := httpFixture(t)
	native := nativeRouteSet(t, app.Agent.Handler)
	missing := []string{}
	for _, row := range integratedNavRoutes {
		key := row.method + " " + row.path
		if native[key] || mainlineCompat[key] {
			continue
		}
		missing = append(missing, key+" (asked by "+row.source+")")
	}
	if len(missing) > 0 {
		t.Fatalf("default navigation asks for paths no table serves: %v", missing)
	}
}

// TestBaselineOnlySurfacesStayOutOfTheIntegratedApp pins the boundary the owner
// set as deferred: Collection management, the mainline memory and quiz REST
// surfaces, the session-level topic-memory switch, and the two registration
// shapes. Their absence here is deliberate, not an oversight, and the front-end
// rail must not offer them (web/test/integratedEntrySurface.test.js).
func TestBaselineOnlySurfacesStayOutOfTheIntegratedApp(t *testing.T) {
	app, _, _, _, token, _ := httpFixture(t)
	native := nativeRouteSet(t, app.Agent.Handler)
	for _, path := range []string{
		"/api/v1/collections",
		"/api/v1/collections/:id/documents",
		"/api/v1/documents/:id",
		"/api/v1/memories",
		"/api/v1/memories/:id/:action",
		"/api/v1/learning/sessions",
		"/api/v1/learning/progress",
		"/api/v1/sessions/:id/topic-memory",
	} {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
			if native[method+" "+path] {
				t.Errorf("baseline-only surface %s %s is registered on the integrated app", method, path)
			}
		}
	}
	// Legacy registration is an explicit refusal, never a silent success.
	body, _ := json.Marshal(map[string]string{"email": "new@invalid.example", "password": "synthetic-password", "display_name": "new"})
	status, envelope, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/auth/register", string(body), token)
	if status != 501 || envelope["error"] == nil {
		t.Fatalf("legacy registration answered %d %v, want 501 with a named refusal", status, envelope)
	}
	// Learning SSE stays excluded from the isolated surface (v2/router/isolated.go:29-35);
	// side-effect freedom is proved by TestLearningStreamIsExcludedBeforeAuthentication.
	status, _, _ = requestHTTP(t, app.Handler, "POST", "/api/v1/AI/agent/learning/start-stream", `{}`, token)
	if status != 404 {
		t.Fatalf("excluded learning SSE stream answered %d, want 404", status)
	}
}

// TestChatAndAgentTurnsUseDifferentModels proves the two routes do not bleed into
// each other: a chat turn calls the retained ordinary model and never touches the
// Agent's tool-calling model, and an Agent turn is the reverse. The Agent turn
// binding tools is what only the original Eino Runner does.
func TestChatAndAgentTurnsUseDifferentModels(t *testing.T) {
	app, _, ordinary, agentModel, token, _ := httpFixture(t)

	chatSession := createHTTP(t, app.Handler, token, "普通对话")
	chatBody, _ := json.Marshal(map[string]string{"content": "解释一下索引"})
	if status, envelope, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+chatSession+"/messages", string(chatBody), token); status != 200 || envelope["error"] != nil {
		t.Fatalf("chat turn answered %d %v", status, envelope)
	}
	if len(ordinary.requests) == 0 {
		t.Fatal("a chat turn reached no ordinary model")
	}
	if agentModel.CallCount() != 0 {
		t.Fatalf("a chat turn called the Agent's tool-calling model %d times", agentModel.CallCount())
	}

	agentSession := dataHTTP(t, app.Handler, "POST", "/api/v1/sessions", `{"mode":"learning_agent","title":"学习 Agent","requestId":"surface-agent-1"}`, token)["id"].(string)
	agentBody, _ := json.Marshal(map[string]any{"content": "查一下资料", "requestId": "surface-agent-turn-1"})
	status, envelope, _ := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+agentSession+"/messages", string(agentBody), token)
	if status != 200 || envelope["error"] != nil {
		t.Fatalf("agent turn answered %d %v", status, envelope)
	}
	if agentModel.CallCount() == 0 {
		t.Fatal("an Agent turn reached no tool-calling model: the original Runner was bypassed")
	}
	if bindings := agentModel.BoundToolCalls(); len(bindings) == 0 {
		t.Fatal("the Agent turn bound no tools, so it was not the Eino ReAct graph")
	} else {
		bound := map[string]bool{}
		for _, round := range bindings {
			for _, name := range round {
				bound[name] = true
			}
		}
		// `document_read` exists only in the original V2 tool surface
		// (v2/service/agent/tool/document_read.go:17); the mainline harness has no
		// such tool, so its presence is what distinguishes the two routes.
		for _, name := range []string{"knowledge_search", "document_read"} {
			if !bound[name] {
				t.Fatalf("the original tool %s was never bound; bindings were %v", name, bound)
			}
		}
		// And the default configuration keeps memory and episode recall out of the
		// model's reach: memoryReadMode=legacy_v2 and episodeRecallMode=shadow_only
		// (v2/config/config.go:247-288). Pinned here so a later config change is a
		// deliberate act with a failing test behind it.
		for _, name := range []string{"memory_search", "topic_memory_read", "propose_memory"} {
			if bound[name] {
				t.Fatalf("%s was bound under the default read mode: capability gate widened", name)
			}
		}
	}
	if len(ordinary.requests) != 1 {
		t.Fatalf("an Agent turn also invoked the ordinary model: %d requests", len(ordinary.requests))
	}
}
