// Package control implements the static in-container control runtime. Its
// response is never evidence of process cleanup: the host resets the container.
package control

const (
	StreamLimit                 = 256 * 1024
	DefaultTextLimit      int64 = 1024 * 1024
	DefaultTimeoutSeconds       = 60
	MaxTimeoutSeconds           = 300
	FileTimeoutSeconds          = 30
)

type Request struct {
	Operation      string            `json:"operation"`
	Argv           []string          `json:"argv,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Path           string            `json:"path,omitempty"`
	Content        string            `json:"content,omitempty"`
	MaxTextBytes   int64             `json:"max_text_bytes"`
}

type ExecResult struct {
	ExitCode        *int   `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	TimedOut        bool   `json:"timed_out"`
}

type TextResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

type Response struct {
	Exec  *ExecResult `json:"exec,omitempty"`
	Text  *TextResult `json:"text,omitempty"`
	Error *Error      `json:"error,omitempty"`
}
