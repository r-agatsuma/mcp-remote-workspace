package mcpserver

// These types define the v0 external contract. Backend policy belongs outside
// this package; no runtime, image, mount, or host execution options are exposed.
type WorkspaceID string
type SourceType string
type ChangeStatus string
type SHA256 string
type Timestamp string
type ReturnedPath string

const (
	SourceEmpty     SourceType   = "empty"
	SourcePublicGit SourceType   = "public_git"
	ChangeAdded     ChangeStatus = "added"
	ChangeModified  ChangeStatus = "modified"
	ChangeDeleted   ChangeStatus = "deleted"
	ChangeRenamed   ChangeStatus = "renamed"
	ChangeUntracked ChangeStatus = "untracked"
)

type ErrorCode string

const (
	ErrorNotImplemented    ErrorCode = "not_implemented"
	ErrorInvalidArgument   ErrorCode = "invalid_argument"
	ErrorWorkspaceNotFound ErrorCode = "workspace_not_found"
	ErrorInvalidPath       ErrorCode = "invalid_path"
	ErrorTimeout           ErrorCode = "timeout"
	ErrorSizeLimit         ErrorCode = "size_limit"
	ErrorNonUTF8           ErrorCode = "non_utf8"
	ErrorNotGitRepository  ErrorCode = "not_git_repository"
	ErrorBaseRefRequired   ErrorCode = "base_ref_required"
	ErrorInvalidBaseRef    ErrorCode = "invalid_base_ref"
	ErrorBackend           ErrorCode = "backend_error"
)

type ToolError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type ErrorResult struct {
	Error ToolError `json:"error"`
}

type SourceInput struct {
	Type SourceType `json:"type"`
	URL  string     `json:"url,omitempty"`
	Ref  string     `json:"ref,omitempty"`
}

type SourceOutput struct {
	Type              SourceType `json:"type"`
	URL               string     `json:"url,omitempty"`
	RequestedRef      string     `json:"requested_ref,omitempty"`
	ResolvedCommitSHA string     `json:"resolved_commit_sha,omitempty"`
}

type WorkspaceCreateInput struct {
	Source *SourceInput `json:"source,omitempty"`
}

type WorkspaceCreateOutput struct {
	WorkspaceID WorkspaceID  `json:"workspace_id"`
	CreatedAt   Timestamp    `json:"created_at"`
	Source      SourceOutput `json:"source"`
}

type ExecInput struct {
	WorkspaceID    WorkspaceID       `json:"workspace_id"`
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"` // Defaults to ".".
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type ExecOutput struct {
	ExitCode        *int   `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	TimedOut        bool   `json:"timed_out"`
}

type WriteTextInput struct {
	WorkspaceID WorkspaceID `json:"workspace_id"`
	Path        string      `json:"path"`
	Content     string      `json:"content"`
}

type WriteTextOutput struct {
	Path      ReturnedPath `json:"path"`
	SizeBytes int64        `json:"size_bytes"`
	SHA256    SHA256       `json:"sha256"`
}

type ReadTextInput struct {
	WorkspaceID WorkspaceID `json:"workspace_id"`
	Path        string      `json:"path"`
}

type ReadTextOutput struct {
	Path      ReturnedPath `json:"path"`
	Content   string       `json:"content"`
	SizeBytes int64        `json:"size_bytes"`
	SHA256    SHA256       `json:"sha256"`
}

type WorkspaceChangesInput struct {
	WorkspaceID WorkspaceID `json:"workspace_id"`
	BaseRef     string      `json:"base_ref,omitempty"`
}

type Change struct {
	Status  ChangeStatus `json:"status"`
	Path    ReturnedPath `json:"path"`
	OldPath ReturnedPath `json:"old_path,omitempty"`
}

type WorkspaceChangesOutput struct {
	BaseRef string   `json:"base_ref"`
	HeadSHA string   `json:"head_sha,omitempty"`
	Changes []Change `json:"changes"`
}

type WorkspaceDestroyInput struct {
	WorkspaceID WorkspaceID `json:"workspace_id"`
}

type WorkspaceDestroyOutput struct {
	WorkspaceID WorkspaceID `json:"workspace_id"`
	Destroyed   bool        `json:"destroyed"`
}
