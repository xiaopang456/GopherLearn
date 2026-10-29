package agent

import (
	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
)

// The evidence kinds this server's tools can produce. A kind is part of the
// deduplication key, so a document passage and a recalled memory that happened to share
// an identifier could not cancel out each other's text, and a future tool has to name
// its own kind rather than borrow one.
const (
	evidenceKindDocument    = "document"
	evidenceKindMemory      = "memory"
	evidenceKindEpisode     = "episode"
	evidenceKindHistoryRead = "history-message"
)

// alreadySentNote replaces a repeated unit's body. It states the fact the model needs
// in order not to ask for the same passage again, and quotes no content.
const alreadySentNote = "（这段正文本轮的较早一次结果里已经完整给出，此处不再重复）"

// evidenceID describes one displayed unit of evidence for the turn's shared allowance:
// what it is, which record it came from, at which revision, and how much of that record
// this turn showed.
//
// The displayed length belongs in the key. Two requests for the same record that show
// different amounts of it are not the same evidence, and merging them would let a turn
// treat a shortened excerpt as though the whole passage had already been sent.
func evidenceID(kind, namespace, recordID string, version int64, displayed string) contextwindow.EvidenceID {
	return contextwindow.EvidenceID{
		Kind:         kind,
		Namespace:    namespace,
		RecordID:     recordID,
		Version:      version,
		DisplayStart: 0,
		DisplayLimit: len([]rune(displayed)),
	}
}
