package model

const (
	// MaxSessionsPerUser bounds the durable conversation collection returned by
	// the current (non-paginated) session-list endpoint.
	MaxSessionsPerUser = 100

	// MaxMessagesPerSession is a durable safety boundary. One chat turn reserves
	// two rows: the user question and the assistant answer.
	MaxMessagesPerSession = 2000
)
