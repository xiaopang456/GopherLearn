package agent

import "GopherAI/common/code"

// ErrorCodeForAssembly forwards the original native Run error classification.
func ErrorCodeForAssembly(err error) code.Code { return agentErrorCode(err) }
