package httpapi

import "net/http"

// memoryContractCases are the Phase 5B.3 rows: the confirmation surface. They run against a
// fixture seeded by seedMemory, which holds exactly three rows - the owner's pending draft,
// the owner's confirmed topic memory, and a pending draft belonging to the other account -
// so every case names a row that really exists, and the isolation rows name a row the caller
// must not reach.
//
// What each case proves is stated once here because the golden file only records the answer:
//   - authentication and verb come before any stored state;
//   - the two listings are the caller's own and nothing else;
//   - a client proposes and cannot decide: status, confidence, identity and timestamps are
//     refused as unknown fields, and an action carries no body at all;
//   - the three lifecycle actions answer with the specific conflict they met, and a foreign
//     or unknown id is indistinguishable from a row that does not exist.
func memoryContractCases() []contractCase {
	// addBody is what a person types. seedMemory still creates the pending rows the
	// confirm/reject/archive rows act on, straight through the service, so those lifecycle
	// answers stay covered even though no HTTP request can produce a draft any more.
	addBody := `{"scope":"global","content":"用户习惯先看代码再读文档"}`
	addWith := func(extra string) string {
		return `{"scope":"global","content":"用户习惯先看代码再读文档",` + extra + `}`
	}
	return []contractCase{
		// The listings.
		{name: "memories list anonymous", method: http.MethodGet, path: memoryPath + "?state=pending", token: tokenNone, memory: true},
		{name: "memories list pending", method: http.MethodGet, path: memoryPath + "?state=pending", token: tokenOwner, memory: true},
		{name: "memories list active", method: http.MethodGet, path: memoryPath + "?state=active", token: tokenOwner, memory: true},
		{name: "memories list as the other user", method: http.MethodGet, path: memoryPath + "?state=pending", token: tokenOther, memory: true},
		{name: "memories list without a state", method: http.MethodGet, path: memoryPath, token: tokenOwner, memory: true},
		{name: "memories list an archived state", method: http.MethodGet, path: memoryPath + "?state=archived", token: tokenOwner, memory: true},
		{name: "memories list unknown state", method: http.MethodGet, path: memoryPath + "?state=decided", token: tokenOwner, memory: true},
		{name: "memories wrong method", method: http.MethodPut, path: memoryPath, body: `{}`, token: tokenOwner, memory: true},
		{name: "memories anonymous wrong method", method: http.MethodDelete, path: memoryPath, token: tokenNone, memory: true},

		// One memory has no read route of its own: the listings carry everything, and a
		// shape that looks like a detail page is not found rather than guessed at.
		{name: "memory detail path is not a route", method: http.MethodGet, path: memoryItemPath, token: tokenOwner, memory: true},
		{name: "memory detail path anonymous", method: http.MethodGet, path: memoryItemPath, token: tokenNone, memory: true},

		// Adding a memory: one request, and the row it writes is already active.
		{name: "add memory", method: http.MethodPost, path: memoryPath, body: addBody, token: tokenOwner, memory: true},
		{name: "add memory anonymous", method: http.MethodPost, path: memoryPath, body: addBody, token: tokenNone, memory: true},
		{name: "add memory topic", method: http.MethodPost, path: memoryPath, body: `{"scope":"topic","topic_key":"redis","content":"AOF 的 fsync 用 everysec"}`, token: tokenOwner, memory: true},
		// Every field that belongs to a proposal rather than to a statement is refused as
		// an unknown key, which is the same answer the decoder gives for a made-up one.
		{name: "add memory with status", method: http.MethodPost, path: memoryPath, body: addWith(`"status":"active"`), token: tokenOwner, memory: true},
		{name: "add memory with confidence", method: http.MethodPost, path: memoryPath, body: addWith(`"confidence":0.2`), token: tokenOwner, memory: true},
		{name: "add memory with user id", method: http.MethodPost, path: memoryPath, body: addWith(`"user_id":"user-9"`), token: tokenOwner, memory: true},
		{name: "add memory with a source session", method: http.MethodPost, path: memoryPath, body: addWith(`"source_session_id":"{{session}}"`), token: tokenOwner, memory: true},
		{name: "add memory with source messages", method: http.MethodPost, path: memoryPath, body: addWith(`"source_message_ids":["{{memory_message}}"]`), token: tokenOwner, memory: true},
		{name: "add memory with superseded at", method: http.MethodPost, path: memoryPath, body: addWith(`"superseded_at":"2026-01-01T00:00:00Z"`), token: tokenOwner, memory: true},
		{name: "add memory unknown scope", method: http.MethodPost, path: memoryPath, body: `{"scope":"mood","content":"用户习惯先看代码再读文档"}`, token: tokenOwner, memory: true},
		// An episode is something that happened in a conversation. Typed in, with no
		// conversation to point at, it is a claim about an event rather than a memory.
		{name: "add memory episodic", method: http.MethodPost, path: memoryPath, body: `{"scope":"episodic","content":"第一次把 AOF 调到 everysec 才敢上线"}`, token: tokenOwner, memory: true},
		{name: "add memory topic without a key", method: http.MethodPost, path: memoryPath, body: `{"scope":"topic","content":"用户习惯先看代码再读文档"}`, token: tokenOwner, memory: true},
		{name: "add memory global with a key", method: http.MethodPost, path: memoryPath, body: `{"scope":"global","topic_key":"redis","content":"用户习惯先看代码再读文档"}`, token: tokenOwner, memory: true},
		{name: "add memory blank content", method: http.MethodPost, path: memoryPath, body: `{"scope":"global","content":"   "}`, token: tokenOwner, memory: true},
		{name: "add memory empty body", method: http.MethodPost, path: memoryPath, body: "", token: tokenOwner, memory: true},
		{name: "add memory not json", method: http.MethodPost, path: memoryPath, body: `scope=global`, token: tokenOwner, memory: true},

		// The three decisions.
		{name: "confirm memory draft", method: http.MethodPost, path: memoryItemPath + "/confirm", token: tokenOwner, memory: true},
		{name: "confirm memory draft anonymous", method: http.MethodPost, path: memoryItemPath + "/confirm", token: tokenNone, memory: true},
		{name: "confirm an active memory", method: http.MethodPost, path: memoryActiveItemPath + "/confirm", token: tokenOwner, memory: true},
		{name: "confirm a foreign draft", method: http.MethodPost, path: memoryForeignItemPath + "/confirm", token: tokenOwner, memory: true},
		{name: "confirm an unknown draft", method: http.MethodPost, path: memoryPath + "/mem-does-not-exist/confirm", token: tokenOwner, memory: true},
		{name: "confirm memory draft wrong method", method: http.MethodGet, path: memoryItemPath + "/confirm", token: tokenOwner, memory: true},
		// Authentication is answered before the verb is judged, so an anonymous request
		// cannot use a 405 to confirm which action names this server recognises.
		{name: "memory action anonymous wrong method", method: http.MethodGet, path: memoryItemPath + "/confirm", token: tokenNone, memory: true},
		{name: "confirm memory draft with a body", method: http.MethodPost, path: memoryItemPath + "/confirm", body: `{"status":"active"}`, token: tokenOwner, memory: true},
		{name: "confirm memory draft with an empty object", method: http.MethodPost, path: memoryItemPath + "/confirm", body: `{}`, token: tokenOwner, memory: true},
		{name: "reject memory draft", method: http.MethodPost, path: memoryItemPath + "/reject", token: tokenOwner, memory: true},
		{name: "reject an active memory", method: http.MethodPost, path: memoryActiveItemPath + "/reject", token: tokenOwner, memory: true},
		{name: "archive active memory", method: http.MethodPost, path: memoryActiveItemPath + "/archive", token: tokenOwner, memory: true},
		{name: "archive a pending draft", method: http.MethodPost, path: memoryItemPath + "/archive", token: tokenOwner, memory: true},
		{name: "archive a foreign draft", method: http.MethodPost, path: memoryForeignItemPath + "/archive", token: tokenOwner, memory: true},
		{name: "memory unknown action", method: http.MethodPost, path: memoryItemPath + "/delete", token: tokenOwner, memory: true},
		{name: "memory action anonymous unknown action", method: http.MethodPost, path: memoryItemPath + "/delete", token: tokenNone, memory: true},
	}
}
