package rag

import "errors"

// ErrTransientEmbedding marks an exhausted retryable provider failure. The
// candidate executor maps it to the existing transient_dependency closure code.
var ErrTransientEmbedding = errors.New("transient embedding dependency")
