package ssm

// StartSessionInput represents the input for SSM StartSession API
type StartSessionInput struct {
	Target       string
	DocumentName string
	Parameters   map[string][]string
}

// StartSessionOutput represents the output from SSM StartSession API
type StartSessionOutput struct {
	SessionId  string
	StreamUrl  string
	TokenValue string
}

// TerminateSessionInput represents the input for SSM TerminateSession API
type TerminateSessionInput struct {
	SessionId string
}

// TerminateSessionOutput represents the output from SSM TerminateSession API
type TerminateSessionOutput struct {
	SessionId string
}
