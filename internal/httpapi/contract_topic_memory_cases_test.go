package httpapi

import "net/http"

// topicMemoryContractCases are the Phase 9A.2 rows: the conversation-scoped topic-memory switch,
// GET and PUT /api/v1/sessions/{id}/topic-memory.
//
// What the block proves, stated once because the golden file only records the answer:
//   - the default is answered, not missing: a conversation that was never switched on reads
//     `{"enabled":false}` with a 200, so "no row" is never a 404 and never an error a client has to
//     interpret;
//   - the body is one boolean. Every other name a client could send - topic_key, user_id, scope,
//     session_id, all_users - is refused before anything is read, because a body that could name a
//     subject would be a client claiming an authority it was never given;
//   - an absent `enabled` is a malformed request, not an off decision: a zero value that switched
//     the permission off would let a client that forgot the field act for the learner;
//   - mode decides who may store a yes (chat: 400) and does not decide who may read (chat: 200
//     false);
//   - a conversation that is somebody else's and one that never existed answer the same way, on
//     both verbs, which is what rows 3/4 and 12/13 are for read side by side;
//   - authentication and verb come before any stored state, and an unsupported verb on this path is
//     405 rather than a second spelling of PUT.
//
// Sequence is not here: each row gets a fresh fixture, so "read back what I just wrote", repeated
// writes staying idempotent, and "a refused write changed nothing" are the end-to-end file's job.
func topicMemoryContractCases() []contractCase {
	on := `{"enabled":true}`
	off := `{"enabled":false}`
	// withExtra appends a forged key to a well-formed body, so the refusal can only be about the
	// extra name - not about a malformed boolean the caller never sent.
	withExtra := func(extra string) string {
		return `{"enabled":true,` + extra + `}`
	}
	return []contractCase{
		// Reading.
		{name: "topic memory read anonymous", method: http.MethodGet, path: topicMemoryPath, token: tokenNone, agent: true},
		{name: "topic memory read defaults to off", method: http.MethodGet, path: topicMemoryPath, token: tokenOwner, agent: true},
		{name: "topic memory read as the other user", method: http.MethodGet, path: topicMemoryPath, token: tokenOther, agent: true},
		{name: "topic memory read a missing conversation", method: http.MethodGet,
			path: "/api/v1/sessions/session-never-created/topic-memory", token: tokenOwner, agent: true},
		{name: "topic memory read a chat conversation", method: http.MethodGet, path: chatTopicMemoryPath, token: tokenOwner},
		// A chat conversation may read its own answer, and a chat conversation with a forged row is
		// still readable: the refusal lives on the write and on the recall, not on the read.
		{name: "topic memory read unknown action", method: http.MethodGet,
			path: "/api/v1/sessions/" + sessionPlaceholder + "/topicmemory", token: tokenOwner},

		// Writing.
		{name: "topic memory switch on", method: http.MethodPut, path: topicMemoryPath, body: on, token: tokenOwner, agent: true},
		{name: "topic memory switch off", method: http.MethodPut, path: topicMemoryPath, body: off, token: tokenOwner, agent: true},
		{name: "topic memory switch anonymous", method: http.MethodPut, path: topicMemoryPath, body: on, token: tokenNone, agent: true},
		{name: "topic memory switch a chat conversation", method: http.MethodPut, path: chatTopicMemoryPath, body: on, token: tokenOwner},
		{name: "topic memory switch as the other user", method: http.MethodPut, path: topicMemoryPath, body: on, token: tokenOther, agent: true},
		{name: "topic memory switch a missing conversation", method: http.MethodPut,
			path: "/api/v1/sessions/session-never-created/topic-memory", body: on, token: tokenOwner, agent: true},

		// A body that is not one boolean.
		{name: "topic memory switch without enabled", method: http.MethodPut, path: topicMemoryPath, body: `{}`, token: tokenOwner, agent: true},
		{name: "topic memory switch empty body", method: http.MethodPut, path: topicMemoryPath, body: "", token: tokenOwner, agent: true},
		{name: "topic memory switch not json", method: http.MethodPut, path: topicMemoryPath, body: `enabled=true`, token: tokenOwner, agent: true},
		{name: "topic memory switch wrong type", method: http.MethodPut, path: topicMemoryPath,
			body: `{"enabled":"true"}`, token: tokenOwner, agent: true},
		{name: "topic memory switch null", method: http.MethodPut, path: topicMemoryPath,
			body: `null`, token: tokenOwner, agent: true},
		{name: "topic memory switch array", method: http.MethodPut, path: topicMemoryPath,
			body: `[{"enabled":true}]`, token: tokenOwner, agent: true},
		{name: "topic memory switch trailing json", method: http.MethodPut, path: topicMemoryPath,
			body: on + off, token: tokenOwner, agent: true},

		// Names a client must not be able to send.
		{name: "topic memory switch with topic key", method: http.MethodPut, path: topicMemoryPath,
			body: withExtra(`"topic_key":"redis"`), token: tokenOwner, agent: true},
		{name: "topic memory switch with user id", method: http.MethodPut, path: topicMemoryPath,
			body: withExtra(`"user_id":"user-9"`), token: tokenOwner, agent: true},
		{name: "topic memory switch with scope", method: http.MethodPut, path: topicMemoryPath,
			body: withExtra(`"scope":"topic"`), token: tokenOwner, agent: true},
		{name: "topic memory switch with session id", method: http.MethodPut, path: topicMemoryPath,
			body: withExtra(`"session_id":"session-2"`), token: tokenOwner, agent: true},
		{name: "topic memory switch with all users", method: http.MethodPut, path: topicMemoryPath,
			body: withExtra(`"all_users":true`), token: tokenOwner, agent: true},
		{name: "topic memory switch with mode", method: http.MethodPut, path: topicMemoryPath,
			body: withExtra(`"mode":"learning_agent"`), token: tokenOwner, agent: true},

		// The shared body rules, on the new surface: a byte sequence that cannot be UTF-8 and an
		// escape that cannot be a code point are refused before anything is decoded, and nothing is
		// written either way.
		{name: "topic memory switch invalid utf-8", method: http.MethodPut, path: topicMemoryPath,
			body: `{"enabled":true,"note":"中文` + "\xc3\x28" + `"}`, token: tokenOwner, agent: true},
		{name: "topic memory switch lone surrogate escape", method: http.MethodPut, path: topicMemoryPath,
			body: `{"enabled":true,"note":"` + `\ud800"}`, token: tokenOwner, agent: true},
		{name: "topic memory switch truncated utf-8", method: http.MethodPut, path: topicMemoryPath,
			body: `{"enabled":true,"note":"中文` + "\xe6\x9c" + `"}`, token: tokenOwner, agent: true},

		// Verb.
		{name: "topic memory wrong method", method: http.MethodPost, path: topicMemoryPath, body: on, token: tokenOwner, agent: true},
		{name: "topic memory delete method", method: http.MethodDelete, path: topicMemoryPath, token: tokenOwner, agent: true},
		{name: "topic memory patch method", method: http.MethodPatch, path: topicMemoryPath, body: on, token: tokenOwner, agent: true},
		{name: "topic memory anonymous wrong method", method: http.MethodPost, path: topicMemoryPath, token: tokenNone, agent: true},
	}
}
