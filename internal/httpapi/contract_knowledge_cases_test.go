package httpapi

import "net/http"

// allContractCases is the whole HTTP contract table: the pre-existing routes plus
// the phase 3A knowledge rows and the phase 5B.3 memory confirmation rows. Order
// matters only for the generated file, so it is fixed here rather than left to map
// iteration.
func allContractCases() []contractCase {
	cases := append(contractCases(), knowledgeContractCases()...)
	cases = append(cases, memoryContractCases()...)
	cases = append(cases, learningContractCases()...)
	// Topic memory last, for the same reason learning was: the golden file is ordered by this
	// list, and an appended block is the only way to add rows without restating one row of an
	// accepted contract.
	return append(cases, topicMemoryContractCases()...)
}

// knowledgeContractCases are the phase 3A rows: knowledge collection and document
// ingestion, plus grounded turns where the retrieval scope is authorized before
// anything is searched.
//
// Bodies use the fixture placeholders, so a case names the seeded collection
// rather than a literal id the store would have to guess.
func knowledgeContractCases() []contractCase {
	groundedBody := `{"content":"Redis 持久化 AOF 与 RDB 怎么取舍","knowledge":{"enabled":true,"collection_ids":["` + collectionPlaceholder + `"]}}`
	return []contractCase{
		// Knowledge collections.
		{name: "create collection", method: http.MethodPost, path: collectionsPath, body: `{"title":"部署手册"}`, token: tokenOwner},
		{name: "create collection without title", method: http.MethodPost, path: collectionsPath, body: `{}`, token: tokenOwner},
		{name: "create collection anonymous", method: http.MethodPost, path: collectionsPath, body: `{"title":"x"}`, token: tokenNone},
		{name: "list collections", method: http.MethodGet, path: collectionsPath, token: tokenOwner},
		{name: "list collections anonymous", method: http.MethodGet, path: collectionsPath, token: tokenNone},
		{name: "collections wrong method", method: http.MethodDelete, path: collectionsPath, token: tokenOwner},
		{name: "collection detail route does not exist", method: http.MethodGet, path: collectionsPath + "/" + collectionPlaceholder, token: tokenOwner},

		// Documents.
		{name: "add document", method: http.MethodPost, path: collectionDocumentsPath, body: `{"title":"kafka.md","format":"markdown","content":"# Kafka\n\n分区日志按 offset 顺序追加。"}`, token: tokenOwner},
		{name: "add document unknown field", method: http.MethodPost, path: collectionDocumentsPath, body: `{"title":"x.md","format":"markdown","content":"# x\n\ny","collection_id":"elsewhere"}`, token: tokenOwner},
		{name: "add document without content", method: http.MethodPost, path: collectionDocumentsPath, body: `{"title":"empty.md","format":"markdown","content":"   "}`, token: tokenOwner},
		{name: "add document unknown format", method: http.MethodPost, path: collectionDocumentsPath, body: `{"title":"book.pdf","format":"pdf","content":"some text"}`, token: tokenOwner},
		{name: "add document to foreign collection", method: http.MethodPost, path: collectionsPath + "/" + foreignPlaceholder + "/documents", body: `{"title":"x.md","format":"markdown","content":"# x\n\ny"}`, token: tokenOwner},
		{name: "add document to unknown collection", method: http.MethodPost, path: collectionsPath + "/collection-404/documents", body: `{"title":"x.md","format":"markdown","content":"# x\n\ny"}`, token: tokenOwner},
		{name: "add document anonymous", method: http.MethodPost, path: collectionDocumentsPath, body: `{"title":"x.md","format":"markdown","content":"# x\n\ny"}`, token: tokenNone},
		{name: "list documents", method: http.MethodGet, path: collectionDocumentsPath, token: tokenOwner},
		{name: "list documents anonymous", method: http.MethodGet, path: collectionDocumentsPath, token: tokenNone},
		{name: "list documents of foreign collection", method: http.MethodGet, path: collectionsPath + "/" + foreignPlaceholder + "/documents", token: tokenOwner},
		{name: "list documents of unknown collection", method: http.MethodGet, path: collectionsPath + "/collection-404/documents", token: tokenOwner},
		{name: "collection documents wrong method", method: http.MethodPatch, path: collectionDocumentsPath, token: tokenOwner},
		{name: "replace document content", method: http.MethodPut, path: documentPath, body: `{"format":"markdown","content":"# Redis 持久化（第二版）\n\nAOF 与 RDB 的取舍已重写，关于 everysec 的旧段落不再保留。"}`, token: tokenOwner},
		{name: "replace document unknown field", method: http.MethodPut, path: documentPath, body: `{"format":"text","content":"anything","version":99}`, token: tokenOwner},
		{name: "replace unknown document", method: http.MethodPut, path: "/api/v1/documents/document-404", body: `{"format":"text","content":"anything"}`, token: tokenOwner},
		{name: "delete document", method: http.MethodDelete, path: documentPath, token: tokenOwner},
		{name: "delete unknown document", method: http.MethodDelete, path: "/api/v1/documents/document-404", token: tokenOwner},
		{name: "delete foreign document", method: http.MethodDelete, path: "/api/v1/documents/" + foreignDocumentPlaceholder, token: tokenOwner},
		{name: "delete document anonymous", method: http.MethodDelete, path: documentPath, token: tokenNone},
		{name: "document wrong method", method: http.MethodPost, path: documentPath, body: `{}`, token: tokenOwner},
		{name: "documents subpath does not exist", method: http.MethodPost, path: collectionDocumentsPath + "/extra", body: `{}`, token: tokenOwner},

		// Grounded turns.
		{name: "grounded sync", method: http.MethodPost, path: sessionMessagesPath, body: groundedBody, token: tokenOwner},
		{name: "grounded stream", method: http.MethodPost, path: sessionStreamPath, body: groundedBody, token: tokenOwner},
		{name: "grounded foreign collection", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"Redis 持久化","knowledge":{"enabled":true,"collection_ids":["` + foreignPlaceholder + `"]}}`, token: tokenOwner},
		{name: "grounded unknown collection", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"Redis 持久化","knowledge":{"enabled":true,"collection_ids":["collection-404"]}}`, token: tokenOwner},
		{name: "grounded stream foreign collection", method: http.MethodPost, path: sessionStreamPath, body: `{"content":"Redis 持久化","knowledge":{"enabled":true,"collection_ids":["` + foreignPlaceholder + `"]}}`, token: tokenOwner},
		{name: "grounded empty collection scope", method: http.MethodPost, path: sessionMessagesPath, body: `{"content":"Redis 持久化","knowledge":{"enabled":true,"collection_ids":[]}}`, token: tokenOwner},
	}
}
